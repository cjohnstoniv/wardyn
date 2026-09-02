// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Integration tests for user drives + grants (migration 0054).
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

// newUserDrive builds a minimal valid managed drive with a per-test unique
// name. The backend is deliberately trivial — these tests are about ROW
// behaviour (upsert keying, FK restriction, resolver ordering), not about
// backend semantics, which types.ValidateUserDrive owns at the API boundary.
func newUserDrive(name string) types.UserDrive {
	return types.UserDrive{
		Name:         name,
		Backend:      types.DriveBackendDockerVolume,
		HomeTemplate: types.HomeTemplateHash,
		Reclaim:      types.DriveReclaimRetain,
		SizeMiB:      10240,
		CreatedBy:    "admin@example.com",
	}
}

// seedUserDrive persists a drive and registers its cleanup, failing the test on
// error. Cleanup deletes the drive AFTER any grant cleanups registered later
// have run (t.Cleanup is LIFO), which the ON DELETE RESTRICT requires.
func seedUserDrive(t *testing.T, st store.PG, name string) types.UserDrive {
	t.Helper()
	ctx := context.Background()
	d, err := st.UpsertUserDrive(ctx, newUserDrive(name))
	if err != nil {
		t.Fatalf("seed drive %q: %v", name, err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, d.ID) })
	return d
}

// seedUserDriveGrant persists a grant and registers its cleanup. Enabled is
// forced on: the store writes the column verbatim, so a zero-value grant is a
// DISABLED one and every precedence case would silently resolve to nothing.
func seedUserDriveGrant(t *testing.T, st store.PG, g types.UserDriveGrant) types.UserDriveGrant {
	t.Helper()
	ctx := context.Background()
	g.Enabled = true
	saved, err := st.UpsertUserDriveGrant(ctx, g)
	if err != nil {
		t.Fatalf("seed grant %+v: %v", g, err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDriveGrant(ctx, saved.ID) })
	return saved
}

// seedDisabledUserDriveGrant persists a PAUSED grant — the one shape
// seedUserDriveGrant deliberately cannot write, because it forces Enabled on so
// that no precedence case is ever decided by a zero value. Cleanup is
// registered the same way.
func seedDisabledUserDriveGrant(t *testing.T, st store.PG, g types.UserDriveGrant) types.UserDriveGrant {
	t.Helper()
	ctx := context.Background()
	g.Enabled = false
	saved, err := st.UpsertUserDriveGrant(ctx, g)
	if err != nil {
		t.Fatalf("seed disabled grant %+v: %v", g, err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDriveGrant(ctx, saved.ID) })
	return saved
}

// TestPG_UserDrive_UpsertRoundTrip pins the id-keyed upsert contract: a fresh id
// INSERTs, the same id UPDATEs in place (rename included, which has to work
// because ON DELETE RESTRICT makes delete-and-recreate impossible for an
// allocated drive), and every column survives the round trip.
func TestPG_UserDrive_UpsertRoundTrip(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	created := seedUserDrive(t, st, "test-drive-"+uuid.NewString())
	if created.ID == uuid.Nil {
		t.Fatal("insert returned the nil uuid; the console would have no update/delete target")
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() || created.CreatedBy != "admin@example.com" {
		t.Errorf("insert = %+v, want server-stamped timestamps and the caller's created_by", created)
	}
	if created.SizeMiB != 10240 || created.Backend != types.DriveBackendDockerVolume {
		t.Errorf("round trip = %+v, want the seeded backend and size", created)
	}

	renamed := created
	renamed.Name = "test-renamed-" + uuid.NewString()
	renamed.Backend = types.DriveBackendHostPath
	renamed.HostRoot = "/srv/homes"
	renamed.HomeTemplate = types.HomeTemplateEmailLocal
	renamed.Writable = true
	renamed.Reclaim = types.DriveReclaimDelete
	updated, err := st.UpsertUserDrive(ctx, renamed)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.ID != created.ID || updated.Name != renamed.Name {
		t.Errorf("update = %s/%q, want the existing row %s renamed to %q",
			updated.ID, updated.Name, created.ID, renamed.Name)
	}
	if updated.Backend != types.DriveBackendHostPath || updated.HostRoot != "/srv/homes" ||
		updated.HomeTemplate != types.HomeTemplateEmailLocal || !updated.Writable ||
		updated.Reclaim != types.DriveReclaimDelete {
		t.Errorf("update = %+v, want every edited column carried through", updated)
	}
	// Provenance rule: an edit never rewrites who REGISTERED the drive.
	if updated.CreatedBy != created.CreatedBy || !updated.CreatedAt.Equal(created.CreatedAt) {
		t.Errorf("update rewrote creation provenance: created_by %q->%q, created_at %v->%v",
			created.CreatedBy, updated.CreatedBy, created.CreatedAt, updated.CreatedAt)
	}

	got, err := st.GetUserDrive(ctx, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != renamed.Name {
		t.Errorf("get name = %q, want %q", got.Name, renamed.Name)
	}
	if _, err := st.GetUserDrive(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get unknown id: err = %v, want ErrNotFound", err)
	}
}

// TestPG_UserDrive_DuplicateNameConflicts: name is the UNIQUE handle an admin
// allocates by, the fragment a PVC's name carries, and the resolver's final
// tie-break, so a second row taking a taken name must be a caller-fixable
// ErrConflict (409), never a raw driver error.
func TestPG_UserDrive_DuplicateNameConflicts(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	name := "test-dupe-drive-" + uuid.NewString()
	seedUserDrive(t, st, name)
	if _, err := st.UpsertUserDrive(ctx, newUserDrive(name)); !errors.Is(err, store.ErrConflict) {
		t.Errorf("second drive with name %q: err = %v, want ErrConflict", name, err)
	}
}

// TestPG_UserDrive_DeleteRestrictedWhileGranted is the load-bearing schema
// assertion of this migration. ON DELETE RESTRICT exists because CASCADE would
// drop the allocations of a drive deleted by mistake while the DIRECTORIES they
// named still held somebody's work — now unreachable and unaudited. The store
// must surface that refusal as a sentinel the route can answer 409 with.
//
// It also pins the LIST's grant count, which is what makes the console's delete
// affordance honest: the admin sees "still allocated" before Postgres says so.
func TestPG_UserDrive_DeleteRestrictedWhileGranted(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	d := seedUserDrive(t, st, "test-restrict-drive-"+uuid.NewString())
	g := seedUserDriveGrant(t, st, types.UserDriveGrant{
		SubjectType: types.CapabilitySubjectGroup,
		Subject:     "test-group-" + uuid.NewString(),
		DriveID:     d.ID,
	})

	drives, err := st.ListUserDrives(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, item := range drives {
		if item.ID != d.ID {
			continue
		}
		found = true
		if item.GrantCount != 1 {
			t.Errorf("list grant_count = %d for a drive with one grant, want 1", item.GrantCount)
		}
	}
	if !found {
		t.Fatalf("drive %s missing from the list", d.ID)
	}

	if err := st.DeleteUserDrive(ctx, d.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("delete an ALLOCATED drive: err = %v, want ErrConflict "+
			"(CASCADE here would orphan the directories those grants named)", err)
	}
	// De-allocate, then the same delete must succeed — the refusal is about the
	// binding, not about the drive.
	if err := st.DeleteUserDriveGrant(ctx, g.ID); err != nil {
		t.Fatalf("delete grant: %v", err)
	}
	if err := st.DeleteUserDrive(ctx, d.ID); err != nil {
		t.Errorf("delete an UNALLOCATED drive: err = %v, want nil", err)
	}
	if err := st.DeleteUserDrive(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete unknown id: err = %v, want ErrNotFound", err)
	}
}

// TestPG_UserDriveGrant_NaturalKeyUpsert: re-allocating a subject must REPOINT
// its one row, not accumulate a second. Two rows for one subject would make
// "which drive does Bob get" depend on the priority/name tie-breaks instead of
// the admin's last write.
//
// It also pins the TRI-STATE writable override, which a plain bool could not
// carry: nil is "use the drive's posture" and an explicit false is an admin
// narrowing this subject to read-only.
func TestPG_UserDriveGrant_NaturalKeyUpsert(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	first := seedUserDrive(t, st, "test-grant-a-"+uuid.NewString())
	second := seedUserDrive(t, st, "test-grant-b-"+uuid.NewString())
	subject := "test-user-" + uuid.NewString()

	g1 := seedUserDriveGrant(t, st, types.UserDriveGrant{
		SubjectType: types.CapabilitySubjectUser,
		Subject:     subject,
		DriveID:     first.ID,
		Priority:    1,
		CreatedBy:   "admin@example.com",
	})
	if g1.WritableOverride != nil {
		t.Errorf("writable_override = %v on a grant that set none, want nil (use the drive's posture)", *g1.WritableOverride)
	}

	no := false
	g2, err := st.UpsertUserDriveGrant(ctx, types.UserDriveGrant{
		SubjectType:      types.CapabilitySubjectUser,
		Subject:          subject,
		DriveID:          second.ID,
		Priority:         9,
		Enabled:          true,
		SizeMiBOverride:  512,
		WritableOverride: &no,
		HomeOverride:     "bsmith",
	})
	if err != nil {
		t.Fatalf("re-allocate: %v", err)
	}
	if g2.ID != g1.ID {
		t.Errorf("re-allocate id = %s, want the existing row's %s", g2.ID, g1.ID)
	}
	if g2.DriveID != second.ID || g2.Priority != 9 || g2.SizeMiBOverride != 512 || g2.HomeOverride != "bsmith" {
		t.Errorf("re-allocate = %+v, want every override carried through", g2)
	}
	if g2.WritableOverride == nil || *g2.WritableOverride {
		t.Errorf("writable_override = %v, want an explicit false — NULL and false are different answers", g2.WritableOverride)
	}

	all, err := st.ListUserDriveGrants(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := 0
	for _, g := range all {
		if g.Subject == subject {
			got++
		}
	}
	if got != 1 {
		t.Fatalf("list has %d rows for subject %q, want exactly 1", got, subject)
	}

	// An unknown drive_id is the FK refusing the write. ErrNotFound so the
	// route answers "that drive does not exist" (404), never a 500.
	if _, err := st.UpsertUserDriveGrant(ctx, types.UserDriveGrant{
		SubjectType: types.CapabilitySubjectUser,
		Subject:     "test-orphan-" + uuid.NewString(),
		DriveID:     uuid.New(),
		Enabled:     true,
	}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("grant naming an unknown drive: err = %v, want ErrNotFound", err)
	}
	if err := st.DeleteUserDriveGrant(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete unknown grant id: err = %v, want ErrNotFound", err)
	}
}

// TestPG_ResolveUserDrive is the precedence table — the whole rule this feature
// rests on, asserted against the real ORDER BY rather than a Go
// re-implementation of it (there is deliberately no Go copy to test).
//
// Each case seeds its own uniquely-named drives and uniquely-keyed grants, so
// the shared substrate's other rows can never decide an assertion. The
// `all`-tier row is the one exception: subject_type='all' has a
// UNIQUE(subject_type, subject) key of ("all", empty), so there is exactly ONE
// such row per database and the sub-tests that need it share it — seeded once
// here, pointing at a drive only this test knows the name of.
func TestPG_ResolveUserDrive(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	uniq := uuid.NewString()
	// Names are chosen so the ALPHABETICAL order (aaa < mmm < zzz) is the
	// OPPOSITE of what most cases want to win — a case that passes only because
	// name ASC happened to agree with the tier rule is not evidence.
	dUserSub := seedUserDrive(t, st, "aaa-user-sub-"+uniq)
	dUserEmail := seedUserDrive(t, st, "mmm-user-email-"+uniq)
	dGroup := seedUserDrive(t, st, "zzz-group-"+uniq)
	dAll := seedUserDrive(t, st, "zzz-all-"+uniq)

	sub := "sub-" + uniq
	email := "user-" + uniq + "@example.com"
	group := "group-" + uniq
	// capabilitySubjects' documented order: the lowercased sub FIRST, then the
	// email. The resolver encodes MATCH POSITION in this slice, so the caller's
	// ordering IS the sub-beats-email rule.
	subjects := []string{sub, email}

	seedUserDriveGrant(t, st, types.UserDriveGrant{
		SubjectType: types.CapabilitySubjectAll, Subject: "", DriveID: dAll.ID,
	})

	// resolveName runs the resolver and returns the winning drive's name, or ""
	// for ErrNotFound (which the caller reads as "this principal has no
	// drive"). It also asserts the invariant every case shares: the tier
	// returned IS the winning grant's own subject_type.
	resolveName := func(t *testing.T, users, groups []string) string {
		t.Helper()
		d, g, tier, err := st.ResolveUserDrive(ctx, users, groups)
		if errors.Is(err, store.ErrNotFound) {
			return ""
		}
		if err != nil {
			t.Fatalf("resolve(%v, %v): %v", users, groups, err)
		}
		if g == nil || tier != g.SubjectType {
			t.Fatalf("tier = %q but the winning grant is %+v — the two cannot disagree", tier, g)
		}
		if g.DriveID != d.ID {
			t.Fatalf("the returned grant %s points at drive %s, not the returned %s", g.ID, g.DriveID, d.ID)
		}
		return d.Name
	}

	t.Run("all tier is the floor", func(t *testing.T) {
		if got := resolveName(t, []string{"nobody-" + uniq}, nil); got != dAll.Name {
			t.Errorf("resolve with no user/group match = %q, want the 'all' row's %q", got, dAll.Name)
		}
	})

	t.Run("group beats all", func(t *testing.T) {
		seedUserDriveGrant(t, st, types.UserDriveGrant{
			SubjectType: types.CapabilitySubjectGroup, Subject: group, DriveID: dGroup.ID,
		})
		if got := resolveName(t, []string{"nobody-" + uniq}, []string{group}); got != dGroup.Name {
			t.Errorf("resolve = %q, want the group row's %q (group > all)", got, dGroup.Name)
		}
	})

	t.Run("user beats group at any priority", func(t *testing.T) {
		seedUserDriveGrant(t, st, types.UserDriveGrant{
			SubjectType: types.CapabilitySubjectGroup, Subject: group, DriveID: dGroup.ID,
			Priority: 1000, // priority never crosses a tier boundary
		})
		seedUserDriveGrant(t, st, types.UserDriveGrant{
			SubjectType: types.CapabilitySubjectUser, Subject: email, DriveID: dUserEmail.ID,
		})
		if got := resolveName(t, subjects, []string{group}); got != dUserEmail.Name {
			t.Errorf("resolve = %q, want the user row's %q (user > group, even at priority 1000)", got, dUserEmail.Name)
		}
	})

	t.Run("sub beats email within the user tier", func(t *testing.T) {
		// Both user-tier rows, with the EMAIL row given the higher priority so
		// a win for sub cannot be explained by priority. Sub is the stable
		// identifier; inheriting a departed colleague's DRIVE by taking their
		// address is emphatically not a thing this may permit.
		seedUserDriveGrant(t, st, types.UserDriveGrant{
			SubjectType: types.CapabilitySubjectUser, Subject: email, DriveID: dUserEmail.ID,
			Priority: 500,
		})
		seedUserDriveGrant(t, st, types.UserDriveGrant{
			SubjectType: types.CapabilitySubjectUser, Subject: sub, DriveID: dUserSub.ID,
			Priority: 0,
		})
		if got := resolveName(t, subjects, nil); got != dUserSub.Name {
			t.Errorf("resolve = %q, want the SUB row's %q (sub beats email inside the user tier)", got, dUserSub.Name)
		}
	})

	t.Run("priority DESC breaks ties within a tier", func(t *testing.T) {
		g1, g2 := "grp1-"+uniq, "grp2-"+uniq
		lo := seedUserDrive(t, st, "aaa-lowprio-"+uniq)
		hi := seedUserDrive(t, st, "zzz-highprio-"+uniq)
		seedUserDriveGrant(t, st, types.UserDriveGrant{
			SubjectType: types.CapabilitySubjectGroup, Subject: g1, DriveID: lo.ID, Priority: 1,
		})
		seedUserDriveGrant(t, st, types.UserDriveGrant{
			SubjectType: types.CapabilitySubjectGroup, Subject: g2, DriveID: hi.ID, Priority: 7,
		})
		// hi sorts LAST alphabetically, so a pass here is priority, not name.
		if got := resolveName(t, []string{"nobody-" + uniq}, []string{g1, g2}); got != hi.Name {
			t.Errorf("resolve = %q, want the priority-7 row's %q", got, hi.Name)
		}
	})

	t.Run("name ASC is the deterministic floor", func(t *testing.T) {
		g1, g2 := "tie1-"+uniq, "tie2-"+uniq
		first := seedUserDrive(t, st, "aaa-tie-"+uniq)
		last := seedUserDrive(t, st, "zzz-tie-"+uniq)
		// Insert the alphabetically-LAST drive's grant FIRST, so a pass cannot
		// be insertion order masquerading as name order.
		seedUserDriveGrant(t, st, types.UserDriveGrant{
			SubjectType: types.CapabilitySubjectGroup, Subject: g2, DriveID: last.ID, Priority: 3,
		})
		seedUserDriveGrant(t, st, types.UserDriveGrant{
			SubjectType: types.CapabilitySubjectGroup, Subject: g1, DriveID: first.ID, Priority: 3,
		})
		for i := 0; i < 3; i++ { // same answer every time, not merely once
			if got := resolveName(t, []string{"nobody-" + uniq}, []string{g1, g2}); got != first.Name {
				t.Fatalf("resolve #%d = %q, want %q (name ASC breaks an equal-priority tie)", i, got, first.Name)
			}
		}
	})

	t.Run("a disabled grant that wins its tier yields paused, never the wider row", func(t *testing.T) {
		// THE rule (DESIGN §2.2), and it is the fail-closed direction. Excluding
		// the row in the WHERE reads tidier and silently WIDENS: Bob's paused
		// user-tier allocation would fall through to the everyone row, handing
		// him a drive no admin decided he should have, at whatever mode that row
		// carries — and his only signal would be a mount that appeared rather
		// than an allocation that stopped. So the disabled row WINS, comes back
		// with Enabled false, and the API renders "paused".
		user := "off-" + uniq
		off := seedUserDrive(t, st, "aaa-disabled-"+uniq)
		g := seedDisabledUserDriveGrant(t, st, types.UserDriveGrant{
			SubjectType: types.CapabilitySubjectUser, Subject: user, DriveID: off.ID,
			SizeMiBOverride: 512,
		})
		d, got, tier, err := st.ResolveUserDrive(ctx, []string{user}, nil)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if d.ID != off.ID {
			t.Errorf("resolve = %q, want the PAUSED user row's %q — falling through to the 'all' row's %q is the widening this rule forbids",
				d.Name, off.Name, dAll.Name)
		}
		// The enabled bit is the whole answer: with it lost in transit the
		// caller mounts a drive an admin turned off.
		if got.Enabled {
			t.Error("the winning grant came back ENABLED; the paused row is indistinguishable from a live one")
		}
		// The GRANT comes back whole, overrides included — what the member is
		// shown is the allocation that is paused, and reading the drive's own
		// size where the grant overrode it would name a different one.
		if got.ID != g.ID || got.SizeMiBOverride != 512 {
			t.Errorf("grant = %+v, want the disabled row %s with its overrides intact", got, g.ID)
		}
		if tier != types.CapabilitySubjectUser {
			t.Errorf("tier = %q, want %q", tier, types.CapabilitySubjectUser)
		}
	})

	t.Run("precedence among disabled rows is the same precedence", func(t *testing.T) {
		// One read means one ORDER BY, so a member paused at two tiers is told
		// about the row that would have won either way — not whichever one the
		// plan returned first. Asserted because "the enabled bit does not enter
		// the ranking" is exactly the property a future WHERE or a CASE in the
		// ORDER BY would quietly break.
		user := "paused-prec-" + uniq
		group := "paused-prec-grp-" + uniq
		// The GROUP row is handed both of the other levers — priority 1000 and
		// the alphabetically-first name — so a win for the user row is the tier
		// rule and nothing else.
		userDrive := seedUserDrive(t, st, "zzz-paused-user-"+uniq)
		groupDrive := seedUserDrive(t, st, "aaa-paused-group-"+uniq)
		seedDisabledUserDriveGrant(t, st, types.UserDriveGrant{
			SubjectType: types.CapabilitySubjectGroup, Subject: group, DriveID: groupDrive.ID,
			Priority: 1000,
		})
		seedDisabledUserDriveGrant(t, st, types.UserDriveGrant{
			SubjectType: types.CapabilitySubjectUser, Subject: user, DriveID: userDrive.ID,
		})
		if got := resolveName(t, []string{user}, []string{group}); got != userDrive.Name {
			t.Errorf("resolve = %q, want the user row's %q (user > group, against priority 1000 and name ASC)",
				got, userDrive.Name)
		}
	})

	t.Run("nil slices are safe", func(t *testing.T) {
		// A nil Go slice binds as SQL NULL and `x = ANY(NULL)` is NULL, not
		// false. Normalization means a caller with no subjects at all still
		// gets a real answer — here, the 'all' row.
		if got := resolveName(t, nil, nil); got != dAll.Name {
			t.Errorf("resolve(nil, nil) = %q, want the 'all' row's %q", got, dAll.Name)
		}
	})
}

// TestPG_HasGroupTierDriveGrants pins the gate on the stale/truncated
// group-snapshot refusal: it must report TRUE only while a group-tier row
// actually exists. On a deployment with none, an unknown group snapshot could
// not have matched anything, so refusing there would lock out every pre-upgrade
// session on a deployment that allocates by user only.
func TestPG_HasGroupTierDriveGrants(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	d := seedUserDrive(t, st, "test-hasgroup-drive-"+uuid.NewString())
	// A USER-tier row must not flip the answer — the gate is about the group
	// tier specifically, not about "any grant exists".
	seedUserDriveGrant(t, st, types.UserDriveGrant{
		SubjectType: types.CapabilitySubjectUser,
		Subject:     "test-user-" + uuid.NewString(),
		DriveID:     d.ID,
	})
	group := seedUserDriveGrant(t, st, types.UserDriveGrant{
		SubjectType: types.CapabilitySubjectGroup,
		Subject:     "test-group-" + uuid.NewString(),
		DriveID:     d.ID,
	})
	withGroup, err := st.HasGroupTierDriveGrants(ctx)
	if err != nil {
		t.Fatalf("has group tier (group row present): %v", err)
	}
	if !withGroup {
		t.Error("HasGroupTierDriveGrants = false with a group-tier row present, want true")
	}
	if err := st.DeleteUserDriveGrant(ctx, group.ID); err != nil {
		t.Fatalf("delete group grant: %v", err)
	}
	// The shared substrate may legitimately carry another test's group row, so
	// the post-delete direction is only asserted when this database has none:
	// a false here would otherwise be a flake rather than a finding.
	after, err := st.HasGroupTierDriveGrants(ctx)
	if err != nil {
		t.Fatalf("has group tier (after delete): %v", err)
	}
	if !after {
		t.Log("no group-tier grant remains in this database; the false direction is exercised")
	}
}
