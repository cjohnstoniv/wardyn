// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for governance profiles + assignments (migration 0052).
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
// Run with: WARDYN_TEST_PG=postgres://... go test ./internal/store/...
//
// Every case mints UNIQUE names/subjects and asserts only on rows it created,
// so these are isolated within the shared substrate (both tables are global —
// there is no per-run scoping) and safe to run repeatedly.
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newGovernanceProfile builds a minimal valid profile with a per-test unique
// name. The ceiling is deliberately trivial — these tests are about ROW
// behaviour (upsert keying, FK restriction, resolver ordering), not about spec
// semantics, which validatePolicySpec owns at the API boundary.
func newGovernanceProfile(name string) types.GovernanceProfile {
	return types.GovernanceProfile{
		Name: name,
		Ceiling: types.RunPolicySpec{
			AllowedDomains:      []string{"pypi.org"},
			MinConfinementClass: types.CC2,
		},
		Limits:    types.GovernanceLimits{DenyInteractive: true},
		CreatedBy: "admin@example.com",
	}
}

// seedGovernanceProfile persists a profile and registers its cleanup, failing
// the test on error. Cleanup deletes the profile AFTER any assignment cleanups
// registered later have run (t.Cleanup is LIFO), which is what the ON DELETE
// RESTRICT requires.
func seedGovernanceProfile(t *testing.T, st store.PG, name string) types.GovernanceProfile {
	t.Helper()
	ctx := context.Background()
	p, err := st.UpsertGovernanceProfile(ctx, newGovernanceProfile(name))
	if err != nil {
		t.Fatalf("seed profile %q: %v", name, err)
	}
	t.Cleanup(func() { _ = st.DeleteGovernanceProfile(ctx, p.ID) })
	return p
}

// seedGovernanceAssignment persists an assignment and registers its cleanup.
func seedGovernanceAssignment(t *testing.T, st store.PG, a types.GovernanceAssignment) types.GovernanceAssignment {
	t.Helper()
	ctx := context.Background()
	saved, err := st.UpsertGovernanceAssignment(ctx, a)
	if err != nil {
		t.Fatalf("seed assignment %+v: %v", a, err)
	}
	t.Cleanup(func() { _ = st.DeleteGovernanceAssignment(ctx, saved.ID) })
	return saved
}

// TestPG_GovernanceProfile_UpsertRoundTrip pins the id-keyed upsert contract:
// a fresh id INSERTs, the same id UPDATEs in place (rename included, which has
// to work because ON DELETE RESTRICT makes delete-and-recreate impossible for
// an assigned profile), and the JSONB ceiling/limits survive the round trip.
func TestPG_GovernanceProfile_UpsertRoundTrip(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	created := seedGovernanceProfile(t, st, "test-profile-"+uuid.NewString())
	if created.ID == uuid.Nil {
		t.Fatal("insert returned the nil uuid; the console would have no update/delete target")
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() || created.CreatedBy != "admin@example.com" {
		t.Errorf("insert = %+v, want server-stamped timestamps and the caller's created_by", created)
	}
	if len(created.Ceiling.AllowedDomains) != 1 || created.Ceiling.AllowedDomains[0] != "pypi.org" {
		t.Errorf("ceiling round trip = %+v, want allowed_domains [pypi.org]", created.Ceiling)
	}
	if !created.Limits.DenyInteractive || created.Limits.DenyTaskModeExec {
		t.Errorf("limits round trip = %+v, want {DenyInteractive:true}", created.Limits)
	}

	renamed := created
	renamed.Name = "test-renamed-" + uuid.NewString()
	renamed.Ceiling.AllowedDomains = []string{"pypi.org", "files.pythonhosted.org"}
	renamed.Limits = types.GovernanceLimits{DenyTaskModeExec: true}
	updated, err := st.UpsertGovernanceProfile(ctx, renamed)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.ID != created.ID {
		t.Errorf("update id = %s, want the existing row's %s", updated.ID, created.ID)
	}
	if updated.Name != renamed.Name {
		t.Errorf("update name = %q, want the rename %q", updated.Name, renamed.Name)
	}
	if len(updated.Ceiling.AllowedDomains) != 2 {
		t.Errorf("update ceiling = %+v, want the two-domain allowlist", updated.Ceiling)
	}
	if !updated.Limits.DenyTaskModeExec || updated.Limits.DenyInteractive {
		t.Errorf("update limits = %+v, want {DenyTaskModeExec:true}", updated.Limits)
	}
	// A-9 provenance rule: an edit never rewrites who AUTHORED the profile.
	if updated.CreatedBy != created.CreatedBy || !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("update rewrote creation provenance: created_by %q->%q, created_at %v->%v",
			created.CreatedBy, updated.CreatedBy, created.CreatedAt, updated.CreatedAt)
	}

	got, err := st.GetGovernanceProfile(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != renamed.Name {
		t.Errorf("get name = %q, want %q", got.Name, renamed.Name)
	}
	if _, err := st.GetGovernanceProfile(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get unknown id: err = %v, want ErrNotFound", err)
	}
}

// TestPG_GovernanceProfile_DuplicateNameConflicts: name is the UNIQUE handle an
// admin assigns by and the resolver's final tie-break, so a second row taking a
// taken name must be a caller-fixable ErrConflict (409), never a raw driver
// error — the CreatePolicy contract.
func TestPG_GovernanceProfile_DuplicateNameConflicts(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	name := "test-dupe-" + uuid.NewString()
	seedGovernanceProfile(t, st, name)
	if _, err := st.UpsertGovernanceProfile(ctx, newGovernanceProfile(name)); !errors.Is(err, store.ErrConflict) {
		t.Errorf("second profile with name %q: err = %v, want ErrConflict", name, err)
	}
}

// TestPG_GovernanceProfile_DeleteRestrictedWhileAssigned is the load-bearing
// schema assertion of this migration. ON DELETE RESTRICT exists because
// CASCADE would move every member of a deleted profile back to the deployment
// ceiling — a silent WIDENING with nothing said. The store must surface that
// refusal as a sentinel the route can answer 409 with; a raw driver error would
// become a 500 and an admin would learn nothing.
func TestPG_GovernanceProfile_DeleteRestrictedWhileAssigned(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	p := seedGovernanceProfile(t, st, "test-restrict-"+uuid.NewString())
	a := seedGovernanceAssignment(t, st, types.GovernanceAssignment{
		SubjectType: types.CapabilitySubjectGroup,
		Subject:     "test-group-" + uuid.NewString(),
		ProfileID:   p.ID,
	})

	if err := st.DeleteGovernanceProfile(ctx, p.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("delete an ASSIGNED profile: err = %v, want ErrConflict "+
			"(CASCADE here would silently widen every member back to the deployment ceiling)", err)
	}
	// Unassign, then the same delete must succeed — the refusal is about the
	// binding, not about the profile.
	if err := st.DeleteGovernanceAssignment(ctx, a.ID); err != nil {
		t.Fatalf("delete assignment: %v", err)
	}
	if err := st.DeleteGovernanceProfile(ctx, p.ID); err != nil {
		t.Errorf("delete an UNASSIGNED profile: err = %v, want nil", err)
	}
	if err := st.DeleteGovernanceProfile(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete unknown id: err = %v, want ErrNotFound", err)
	}
}

// TestPG_GovernanceAssignment_NaturalKeyUpsert: re-assigning a subject must
// REPOINT its one row, not accumulate a second. Two rows for one subject would
// make "which profile does Bob get" depend on the priority/name tie-breaks
// instead of the admin's last write — resolvable, but not explainable. The
// returned id must be the EXISTING row's, so the console's DELETE target names
// a real row.
func TestPG_GovernanceAssignment_NaturalKeyUpsert(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	first := seedGovernanceProfile(t, st, "test-assign-a-"+uuid.NewString())
	second := seedGovernanceProfile(t, st, "test-assign-b-"+uuid.NewString())
	subject := "test-user-" + uuid.NewString()

	a1 := seedGovernanceAssignment(t, st, types.GovernanceAssignment{
		SubjectType: types.CapabilitySubjectUser,
		Subject:     subject,
		ProfileID:   first.ID,
		Priority:    1,
		CreatedBy:   "admin@example.com",
	})
	a2, err := st.UpsertGovernanceAssignment(ctx, types.GovernanceAssignment{
		SubjectType: types.CapabilitySubjectUser,
		Subject:     subject,
		ProfileID:   second.ID,
		Priority:    9,
	})
	if err != nil {
		t.Fatalf("re-assign: %v", err)
	}
	if a2.ID != a1.ID {
		t.Errorf("re-assign id = %s, want the existing row's %s", a2.ID, a1.ID)
	}
	if a2.ProfileID != second.ID || a2.Priority != 9 {
		t.Errorf("re-assign = {profile:%s priority:%d}, want {%s 9}", a2.ProfileID, a2.Priority, second.ID)
	}

	all, err := st.ListGovernanceAssignments(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := 0
	for _, a := range all {
		if a.Subject == subject {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("list has %d rows for subject %q, want exactly 1", got, subject)
	}

	// An unknown profile_id is the FK refusing the write. ErrNotFound so the
	// route answers "that profile does not exist" (404), never a 500.
	if _, err := st.UpsertGovernanceAssignment(ctx, types.GovernanceAssignment{
		SubjectType: types.CapabilitySubjectUser,
		Subject:     "test-orphan-" + uuid.NewString(),
		ProfileID:   uuid.New(),
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("assignment naming an unknown profile: err = %v, want ErrNotFound", err)
	}
	if err := st.DeleteGovernanceAssignment(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete unknown assignment id: err = %v, want ErrNotFound", err)
	}
}

// TestPG_ResolveGovernanceProfile is the precedence table — the whole rule this
// feature rests on, asserted against the real ORDER BY rather than a Go
// re-implementation of it (there is deliberately no Go copy to test).
//
// Each case seeds its own uniquely-named profiles and uniquely-keyed
// assignments, so the shared substrate's other rows can never decide an
// assertion. The `all`-tier row is the one exception: subject_type='all' has a
// UNIQUE(subject_type, subject) key of ("all", empty), so there is exactly ONE
// such row per database and the sub-tests that need it share it — seeded once
// here, pointing at a profile only this test knows the name of.
func TestPG_ResolveGovernanceProfile(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	uniq := uuid.NewString()
	// Names are chosen so the ALPHABETICAL order (aaa < mmm < zzz) is the
	// OPPOSITE of what most cases want to win — a case that passes only because
	// name ASC happened to agree with the tier rule is not evidence.
	pUserSub := seedGovernanceProfile(t, st, "aaa-user-sub-"+uniq)
	pUserEmail := seedGovernanceProfile(t, st, "mmm-user-email-"+uniq)
	pGroup := seedGovernanceProfile(t, st, "zzz-group-"+uniq)
	pAll := seedGovernanceProfile(t, st, "zzz-all-"+uniq)

	sub := "sub-" + uniq
	email := "user-" + uniq + "@example.com"
	group := "group-" + uniq
	// capabilitySubjects' documented order: the lowercased sub FIRST, then the
	// email. The resolver encodes MATCH POSITION in this slice, so the caller's
	// ordering IS the sub-beats-email rule.
	subjects := []string{sub, email}

	seedGovernanceAssignment(t, st, types.GovernanceAssignment{
		SubjectType: types.CapabilitySubjectAll, Subject: "", ProfileID: pAll.ID,
	})

	// resolveName runs the resolver and returns the winning profile's name, or
	// "" for ErrNotFound (which the caller reads as "the deployment ceiling").
	resolveName := func(t *testing.T, users, groups []string) string {
		t.Helper()
		// The matched TIER is discarded here on purpose: this table is about the
		// ORDER BY picking the right ROW. What the tier is FOR — telling a
		// user-tier winner from an all-tier one when a group snapshot cannot be
		// evaluated — is internal/api's TestEffectiveCeilingPrecedence.
		p, _, err := st.ResolveGovernanceProfile(ctx, users, groups)
		if errors.Is(err, store.ErrNotFound) {
			return ""
		}
		if err != nil {
			t.Fatalf("resolve(%v, %v): %v", users, groups, err)
		}
		return p.Name
	}

	t.Run("all tier is the floor", func(t *testing.T) {
		if got := resolveName(t, []string{"nobody-" + uniq}, nil); got != pAll.Name {
			t.Errorf("resolve with no user/group match = %q, want the 'all' row's %q", got, pAll.Name)
		}
	})

	t.Run("group beats all", func(t *testing.T) {
		seedGovernanceAssignment(t, st, types.GovernanceAssignment{
			SubjectType: types.CapabilitySubjectGroup, Subject: group, ProfileID: pGroup.ID,
		})
		if got := resolveName(t, []string{"nobody-" + uniq}, []string{group}); got != pGroup.Name {
			t.Errorf("resolve = %q, want the group row's %q (group > all)", got, pGroup.Name)
		}
	})

	t.Run("user beats group at any priority", func(t *testing.T) {
		seedGovernanceAssignment(t, st, types.GovernanceAssignment{
			SubjectType: types.CapabilitySubjectGroup, Subject: group, ProfileID: pGroup.ID,
			Priority: 1000, // priority never crosses a tier boundary
		})
		seedGovernanceAssignment(t, st, types.GovernanceAssignment{
			SubjectType: types.CapabilitySubjectUser, Subject: email, ProfileID: pUserEmail.ID,
		})
		if got := resolveName(t, subjects, []string{group}); got != pUserEmail.Name {
			t.Errorf("resolve = %q, want the user row's %q (user > group, even at priority 1000)", got, pUserEmail.Name)
		}
	})

	t.Run("sub beats email within the user tier", func(t *testing.T) {
		// Both user-tier rows, with the EMAIL row given the higher priority so a
		// win for sub cannot be explained by priority. (Priority ranks BELOW
		// match position on
		// purpose: sub is the stable identifier, an email is reassignable, and
		// inheriting a departed colleague's ceiling by taking their address is
		// not something this may permit.)
		seedGovernanceAssignment(t, st, types.GovernanceAssignment{
			SubjectType: types.CapabilitySubjectUser, Subject: email, ProfileID: pUserEmail.ID,
			Priority: 500,
		})
		seedGovernanceAssignment(t, st, types.GovernanceAssignment{
			SubjectType: types.CapabilitySubjectUser, Subject: sub, ProfileID: pUserSub.ID,
			Priority: 0,
		})
		if got := resolveName(t, subjects, nil); got != pUserSub.Name {
			t.Errorf("resolve = %q, want the SUB row's %q (sub beats email inside the user tier)", got, pUserSub.Name)
		}
	})

	t.Run("priority DESC breaks ties within a tier", func(t *testing.T) {
		g1, g2 := "grp1-"+uniq, "grp2-"+uniq
		lo := seedGovernanceProfile(t, st, "aaa-lowprio-"+uniq)
		hi := seedGovernanceProfile(t, st, "zzz-highprio-"+uniq)
		seedGovernanceAssignment(t, st, types.GovernanceAssignment{
			SubjectType: types.CapabilitySubjectGroup, Subject: g1, ProfileID: lo.ID, Priority: 1,
		})
		seedGovernanceAssignment(t, st, types.GovernanceAssignment{
			SubjectType: types.CapabilitySubjectGroup, Subject: g2, ProfileID: hi.ID, Priority: 7,
		})
		// hi sorts LAST alphabetically, so a pass here is priority, not name.
		if got := resolveName(t, []string{"nobody-" + uniq}, []string{g1, g2}); got != hi.Name {
			t.Errorf("resolve = %q, want the priority-7 row's %q", got, hi.Name)
		}
	})

	t.Run("name ASC is the deterministic floor", func(t *testing.T) {
		g1, g2 := "tie1-"+uniq, "tie2-"+uniq
		first := seedGovernanceProfile(t, st, "aaa-tie-"+uniq)
		last := seedGovernanceProfile(t, st, "zzz-tie-"+uniq)
		// Insert the alphabetically-LAST profile's assignment FIRST, so a pass
		// cannot be insertion order masquerading as name order.
		seedGovernanceAssignment(t, st, types.GovernanceAssignment{
			SubjectType: types.CapabilitySubjectGroup, Subject: g2, ProfileID: last.ID, Priority: 3,
		})
		seedGovernanceAssignment(t, st, types.GovernanceAssignment{
			SubjectType: types.CapabilitySubjectGroup, Subject: g1, ProfileID: first.ID, Priority: 3,
		})
		for i := 0; i < 3; i++ { // same answer every time, not merely once
			if got := resolveName(t, []string{"nobody-" + uniq}, []string{g1, g2}); got != first.Name {
				t.Fatalf("resolve #%d = %q, want %q (name ASC breaks an equal-priority tie)", i, got, first.Name)
			}
		}
	})

	t.Run("nil slices are safe", func(t *testing.T) {
		// A nil Go slice binds as SQL NULL and `x = ANY(NULL)` is NULL, not
		// false. Normalization means a caller with no subjects at all still
		// gets a real answer — here, the 'all' row.
		if got := resolveName(t, nil, nil); got != pAll.Name {
			t.Errorf("resolve(nil, nil) = %q, want the 'all' row's %q", got, pAll.Name)
		}
	})
}

// TestPG_HasGroupTierAssignments pins the gate on the stale/truncated
// group-snapshot refusal: it must report TRUE only while a group-tier row
// actually exists. On a deployment with none, an unknown group snapshot could
// not have matched anything, so refusing there would break "no assignment ⇒
// byte-for-byte today" for every pre-upgrade session.
func TestPG_HasGroupTierAssignments(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	p := seedGovernanceProfile(t, st, "test-hasgroup-"+uuid.NewString())
	// A USER-tier row must not flip the answer — the gate is about the group
	// tier specifically, not about "any assignment exists".
	seedGovernanceAssignment(t, st, types.GovernanceAssignment{
		SubjectType: types.CapabilitySubjectUser,
		Subject:     "test-user-" + uuid.NewString(),
		ProfileID:   p.ID,
	})
	userOnly, err := st.HasGroupTierAssignments(ctx)
	if err != nil {
		t.Fatalf("has group tier (user row only): %v", err)
	}

	group := seedGovernanceAssignment(t, st, types.GovernanceAssignment{
		SubjectType: types.CapabilitySubjectGroup,
		Subject:     "test-group-" + uuid.NewString(),
		ProfileID:   p.ID,
	})
	withGroup, err := st.HasGroupTierAssignments(ctx)
	if err != nil {
		t.Fatalf("has group tier (group row present): %v", err)
	}
	if !withGroup {
		t.Error("HasGroupTierAssignments = false with a group-tier row present, want true")
	}
	if err := st.DeleteGovernanceAssignment(ctx, group.ID); err != nil {
		t.Fatalf("delete group assignment: %v", err)
	}
	// The shared substrate may legitimately carry another test's group row, so
	// the false leg is only assertable when the user-only probe above already
	// saw an empty group tier. When it did, removing OUR row must return the
	// answer to false — otherwise the predicate is not tracking rows at all.
	if !userOnly {
		after, err := st.HasGroupTierAssignments(ctx)
		if err != nil {
			t.Fatalf("has group tier (after delete): %v", err)
		}
		if after {
			t.Error("HasGroupTierAssignments = true after the only group-tier row was deleted, want false")
		}
	}
}
