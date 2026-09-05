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
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

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
	saved, err := st.UpsertUserDriveGrant(ctx, g, true)
	if err != nil {
		t.Fatalf("seed grant %+v: %v", g, err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUserDriveGrant(ctx, saved.ID) })
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
	saved, err := st.UpsertUserDriveGrant(ctx, g, true)
	if err != nil {
		t.Fatalf("seed disabled grant %+v: %v", g, err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUserDriveGrant(ctx, saved.ID) })
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
	// The DELETE hands back the row it removed — the audit row for this action
	// is written from it, and the row is gone by then.
	gone, err := st.DeleteUserDriveGrant(ctx, g.ID)
	if err != nil {
		t.Fatalf("delete grant: %v", err)
	}
	if gone.ID != g.ID || gone.Subject != g.Subject || gone.DriveID != g.DriveID {
		t.Errorf("DELETE RETURNING = %+v, want the row it removed (%+v)", gone, g)
	}
	if err := st.DeleteUserDrive(ctx, d.ID); err != nil {
		t.Errorf("delete an UNALLOCATED drive: err = %v, want nil", err)
	}
	if err := st.DeleteUserDrive(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete unknown id: err = %v, want ErrNotFound", err)
	}

	// A PAUSED allocation pins the drive exactly as a live one does. The
	// RESTRICT is on the FOREIGN KEY, not on `enabled`, and it has to be: a
	// paused grant is the shape an admin leaves behind while they work out what
	// to do with somebody's storage, and it is the ONLY remaining record of
	// which directory was that person's. Reading "paused" as "not really
	// allocated" would let exactly that drive be deleted, orphaning the
	// directory with nothing in the product naming it — the failure the
	// RESTRICT exists to prevent, arriving through its quietest door.
	paused := seedUserDrive(t, st, "test-restrict-paused-"+uuid.NewString())
	pausedGrant := seedDisabledUserDriveGrant(t, st, types.UserDriveGrant{
		SubjectType: types.CapabilitySubjectUser,
		Subject:     "test-paused-user-" + uuid.NewString(),
		DriveID:     paused.ID,
	})
	if err := st.DeleteUserDrive(ctx, paused.ID); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("delete a drive held by a PAUSED grant: err = %v, want ErrConflict", err)
	}
	if _, err := st.DeleteUserDriveGrant(ctx, pausedGrant.ID); err != nil {
		t.Fatalf("delete paused grant: %v", err)
	}
	if err := st.DeleteUserDrive(ctx, paused.ID); err != nil {
		t.Errorf("delete after the paused grant is gone: err = %v, want nil — the refusal is about the binding", err)
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
	}, true)
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
	}, true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("grant naming an unknown drive: err = %v, want ErrNotFound", err)
	}
	if _, err := st.DeleteUserDriveGrant(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete unknown grant id: err = %v, want ErrNotFound", err)
	}
}

// TestPG_UserDriveGrant_OneDirectoryNamePerDrive pins the uniqueness rule the
// natural key does NOT carry, against the real statement rather than a Go
// re-implementation of it.
//
// UNIQUE(subject_type, subject) makes one grant per subject; nothing makes one
// home_override per drive. A home_override names ONE PERSON'S directory — the
// stated reason a group or all row may not carry one — and two user rows with
// one override is that same loss spelled with two rows. On a MANAGED drive it
// is the last remaining way to point two principals at one object Wardyn itself
// mints, since types.ValidateUserDrive refuses a claim home_template there and
// a hash home folds the subject.
func TestPG_UserDriveGrant_OneDirectoryNamePerDrive(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	drive := seedUserDrive(t, st, "test-home-uniq-a-"+uuid.NewString())
	other := seedUserDrive(t, st, "test-home-uniq-b-"+uuid.NewString())
	bob := "test-user-bob-" + uuid.NewString()
	alice := "test-user-alice-" + uuid.NewString()
	home := "home-" + uuid.NewString()[:8]

	seedUserDriveGrant(t, st, types.UserDriveGrant{
		SubjectType: types.CapabilitySubjectUser, Subject: bob, DriveID: drive.ID,
		HomeOverride: home, CreatedBy: "admin@example.com",
	})

	// THE REFUSAL, and it is the WRITE that fails rather than a later surprise
	// at resolve time.
	_, err := st.UpsertUserDriveGrant(ctx, types.UserDriveGrant{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: alice, DriveID: drive.ID,
		HomeOverride: home, Enabled: true, CreatedBy: "admin@example.com",
	}, true)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second subject on one directory: err = %v, want ErrConflict — both would mount one object", err)
	}
	// …and it wrote NOTHING. Allocating alice to the same drive under a name
	// nobody holds must be an INSERT — the store returns the EXISTING row's id
	// on a conflict, so getting the candidate's id back is the proof that the
	// refused write left no half-applied row behind.
	candidate := uuid.New()
	fresh, err := st.UpsertUserDriveGrant(ctx, types.UserDriveGrant{
		ID: candidate, SubjectType: types.CapabilitySubjectUser, Subject: alice, DriveID: drive.ID,
		HomeOverride: home + "-other", Enabled: true, CreatedBy: "admin@example.com",
	}, true)
	if err != nil {
		t.Fatalf("alice under a free name: %v", err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUserDriveGrant(ctx, fresh.ID) })
	if fresh.ID != candidate {
		t.Errorf("id = %s, want the candidate %s — the refused write left a row behind", fresh.ID, candidate)
	}

	// SCOPED. The holder repointing its OWN row is not a clash with itself —
	// this is the case a naive "does any row hold this name" check breaks.
	repointed, err := st.UpsertUserDriveGrant(ctx, types.UserDriveGrant{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: bob, DriveID: drive.ID,
		HomeOverride: home, Priority: 7, Enabled: true, CreatedBy: "admin@example.com",
	}, true)
	if err != nil {
		t.Fatalf("the holder repointing its own row: %v", err)
	}
	if repointed.Priority != 7 || repointed.HomeOverride != home {
		t.Errorf("repointed = %+v, want the edit applied", repointed)
	}
	t.Cleanup(func() { _, _ = st.DeleteUserDriveGrant(ctx, repointed.ID) })

	// The same name on ANOTHER drive is another object entirely — the rule is
	// per drive, not global, or one person's directory name would reserve that
	// word across the deployment.
	erin := "test-user-erin-" + uuid.NewString()
	seedUserDriveGrant(t, st, types.UserDriveGrant{
		SubjectType: types.CapabilitySubjectUser, Subject: erin, DriveID: other.ID,
		HomeOverride: home, CreatedBy: "admin@example.com",
	})

	// And an EMPTY override never collides, however many carry it: each of
	// those grants derives its own home from its own subject.
	carol := "test-user-carol-" + uuid.NewString()
	dave := "test-user-dave-" + uuid.NewString()
	seedUserDriveGrant(t, st, types.UserDriveGrant{
		SubjectType: types.CapabilitySubjectUser, Subject: carol, DriveID: drive.ID, CreatedBy: "admin@example.com",
	})
	seedUserDriveGrant(t, st, types.UserDriveGrant{
		SubjectType: types.CapabilitySubjectUser, Subject: dave, DriveID: drive.ID, CreatedBy: "admin@example.com",
	})
}

// TestPG_ResolveUserDrive_TieHasATotalOrder pins the LAST key, and the reason
// it is not the drive's name.
//
// drives.name is UNIQUE, so `d.name ASC` totally orders two DISTINCT drives.
// The tie it cannot break is two grants naming the SAME drive:
// UNIQUE(subject_type, subject) is per SUBJECT, so two groups one member
// belongs to may each be granted one drive, and priority DEFAULTS to 0 on both.
// Every key above then ties and LIMIT 1 falls to whichever row the plan reached
// first.
//
// It decides something, because THIS RESOLVER RETURNS THE GRANT: the grant
// carries writable_override, size_mib_override, home_override and enabled. The
// failing direction is an admin's explicit read-only narrowing silently NOT
// applying — and the answer changing across a re-write of the rows, with no
// admin action in between.
//
// The test flips the PHYSICAL insert order and demands one answer, which is the
// shape that catches a plan-dependent LIMIT 1; asserting a single resolve would
// pass on an unordered query half the time.
func TestPG_ResolveUserDrive_TieHasATotalOrder(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	shared := seedUserDrive(t, st, "test-tie-shared-"+uuid.NewString())
	// Two groups ONE member is in, both allocated the SAME drive at the default
	// priority. "aaa" sorts before "zzz", so the rule has a nameable answer.
	first := "test-grp-aaa-" + uuid.NewString()
	second := "test-grp-zzz-" + uuid.NewString()
	no := false

	// The narrowing an admin wrote lives on the LOSING row in one insert order
	// and the WINNING row in the other — which is the whole point.
	grants := map[string]types.UserDriveGrant{
		first: {
			SubjectType: types.CapabilitySubjectGroup, Subject: first, DriveID: shared.ID,
			WritableOverride: &no, CreatedBy: "admin@example.com",
		},
		second: {
			SubjectType: types.CapabilitySubjectGroup, Subject: second, DriveID: shared.ID,
			CreatedBy: "admin@example.com",
		},
	}
	resolveWith := func(t *testing.T, order []string) types.UserDriveGrant {
		t.Helper()
		var ids []uuid.UUID
		for _, subject := range order {
			saved, err := st.UpsertUserDriveGrant(ctx, grants[subject], true)
			if err != nil {
				t.Fatalf("seed %s: %v", subject, err)
			}
			ids = append(ids, saved.ID)
		}
		defer func() {
			for _, id := range ids {
				_, _ = st.DeleteUserDriveGrant(ctx, id)
			}
		}()
		d, g, tier, err := st.ResolveUserDrive(ctx, nil, []string{first, second})
		if err != nil {
			t.Fatalf("resolve (%v): %v", order, err)
		}
		if d.ID != shared.ID || tier != types.CapabilitySubjectGroup {
			t.Fatalf("resolved drive/tier = %s/%q, want the shared drive at the group tier", d.ID, tier)
		}
		return *g
	}

	forward := resolveWith(t, []string{first, second})
	reverse := resolveWith(t, []string{second, first})

	if forward.Subject != reverse.Subject {
		t.Fatalf("insert order decided the winner: %q then %q — the ORDER BY is not a total order, "+
			"so the same principal resolves differently across a re-write of the rows",
			forward.Subject, reverse.Subject)
	}
	// …and the winner is the one the rule NAMES, not merely a stable one: a
	// deterministic answer nobody can predict is not an explanation.
	if forward.Subject != first {
		t.Errorf("winner = %q, want the alphabetically first subject %q", forward.Subject, first)
	}
	// THE CONSEQUENCE, asserted rather than implied: the grant that wins is the
	// one carrying the admin's read-only narrowing, in BOTH insert orders.
	for _, g := range []types.UserDriveGrant{forward, reverse} {
		if g.WritableOverride == nil || *g.WritableOverride {
			t.Errorf("writable_override = %v, want the admin's explicit false to have survived the tie",
				g.WritableOverride)
		}
	}
}

// TestPG_ListUserDriveGrantsPage pins the bounded read against the REAL query
// plan's contract: the same order as the whole list, a window that honours limit
// and offset, and a limit of 0 meaning unbounded (Page's own rule).
//
// WHY IT IS BOUNDED AT ALL. user_drive_grants holds one row per SUBJECT and
// capabilitySubjects yields two per person, so this table's size is the
// deployment's headcount — and its ORDER BY has no index. Unbounded on this
// deployment's own PostgreSQL at 50,000 allocations that is `external merge
// Disk: 5584kB` and 149.7 ms; with the caller's LIMIT it is a top-N heapsort in
// `Memory: 301kB` and 37.7 ms, because Postgres keeps the best k rows instead of
// sorting all n. The page is what changes the plan, not just the payload.
func TestPG_ListUserDriveGrantsPage(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	drive := seedUserDrive(t, st, "test-page-"+uuid.NewString())
	// Seeded on the GROUP tier so these rows sort together, after every
	// user-tier row the shared substrate holds: the assertions below are then
	// about THIS test's rows in THIS test's order.
	mine := map[string]bool{}
	for _, suffix := range []string{"a", "b", "c", "d"} {
		g := seedUserDriveGrant(t, st, types.UserDriveGrant{
			SubjectType: types.CapabilitySubjectGroup,
			Subject:     "test-page-" + suffix + "-" + uuid.NewString(),
			DriveID:     drive.ID, CreatedBy: "admin@example.com",
		})
		mine[g.Subject] = true
	}

	// The page's order IS the whole list's order, restricted to this test's
	// rows. A page ordered differently would omit rows a caller had seen and
	// repeat others across offsets, which is the failure a second ORDER BY makes
	// silently — hence one shared const behind both reads.
	all, err := st.ListUserDriveGrants(ctx)
	if err != nil {
		t.Fatalf("ListUserDriveGrants: %v", err)
	}
	var want []string
	for _, g := range all {
		if mine[g.Subject] {
			want = append(want, g.Subject)
		}
	}
	if len(want) != 4 {
		t.Fatalf("seeded 4 grants, the whole list shows %d", len(want))
	}

	// Walk the deployment in pages of 2 and keep only this test's rows: the
	// concatenation must reproduce the whole list's order exactly once.
	var got []string
	for offset := 0; ; offset += 2 {
		page, err := st.ListUserDriveGrantsPage(ctx, store.Page{Limit: 2, Offset: offset})
		if err != nil {
			t.Fatalf("page at offset %d: %v", offset, err)
		}
		if len(page) > 2 {
			t.Fatalf("page returned %d rows for Limit 2 — the LIMIT is what bounds the sort", len(page))
		}
		for _, g := range page {
			if mine[g.Subject] {
				got = append(got, g.Subject)
			}
		}
		if len(page) < 2 {
			break
		}
	}
	if !slices.Equal(got, want) {
		t.Errorf("paged walk = %v,\nwhole list = %v — the two reads must share one order", got, want)
	}

	// Page's own rule, relied on by every internal caller that wants everything.
	unbounded, err := st.ListUserDriveGrantsPage(ctx, store.Page{})
	if err != nil {
		t.Fatalf("unbounded page: %v", err)
	}
	if len(unbounded) != len(all) {
		t.Errorf("Limit 0 returned %d rows, want the whole list's %d — Limit<=0 means unbounded", len(unbounded), len(all))
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

	// The MIXED case, and the one the two arms above cannot make between them:
	// "precedence among disabled rows" pauses BOTH rows, so a WHERE that
	// excluded disabled rows would leave nothing to resolve and the sub-test
	// would fail loudly. Here the wider row is LIVE — which is exactly the
	// shape the exclusion would silently succeed on, handing the member a drive
	// no admin decided they should have and calling it an answer. The paused row
	// must still win its tier, and must still come back with its bit off.
	t.Run("a paused row beats an ENABLED row one tier down", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			pausedTier types.CapabilitySubjectType
			liveTier   types.CapabilitySubjectType
		}{
			{"paused user over enabled group", types.CapabilitySubjectUser, types.CapabilitySubjectGroup},
			{"paused group over enabled all", types.CapabilitySubjectGroup, types.CapabilitySubjectAll},
		} {
			t.Run(tc.name, func(t *testing.T) {
				who := string(tc.pausedTier) + "-mixed-" + uniq
				// The LIVE row is handed the other levers — the alphabetically
				// FIRST name of the pair, and priority where this arm can set one
				// — so a win for the paused row is the tier rule and the enabled
				// bit, and nothing else.
				pausedDrive := seedUserDrive(t, st, "zzz-mixed-paused-"+string(tc.pausedTier)+"-"+uniq)
				seedDisabledUserDriveGrant(t, st, types.UserDriveGrant{
					SubjectType: tc.pausedTier, Subject: who, DriveID: pausedDrive.ID,
				})
				var users, groups []string
				switch tc.pausedTier {
				case types.CapabilitySubjectUser:
					// The live row one tier down is a GROUP row this caller is in.
					users = []string{who}
					liveGroup := "mixed-live-grp-" + uniq
					groups = []string{liveGroup}
					liveDrive := seedUserDrive(t, st, "aaa-mixed-live-"+uniq)
					seedUserDriveGrant(t, st, types.UserDriveGrant{
						SubjectType: tc.liveTier, Subject: liveGroup, DriveID: liveDrive.ID, Priority: 1000,
					})
				default:
					// The live row one tier down is the EVERYONE row — UNIQUE per
					// database (subject_type 'all' keys on the empty subject), so
					// it is the one this test seeded once at the top rather than a
					// second one no schema would allow. It carries the
					// alphabetically FIRST name of the pair, so the win below is
					// the tier rule and the enabled bit rather than name ASC.
					users, groups = []string{"nobody-" + uniq}, []string{who}
				}

				d, g, tier, err := st.ResolveUserDrive(ctx, users, groups)
				if err != nil {
					t.Fatalf("resolve: %v", err)
				}
				if d.ID != pausedDrive.ID || tier != tc.pausedTier {
					t.Fatalf("resolve = %q at tier %q, want the PAUSED %s row's %q — falling through to the live %s row is the widening DESIGN forbids",
						d.Name, tier, tc.pausedTier, pausedDrive.Name, tc.liveTier)
				}
				if g.Enabled {
					t.Error("the winning grant came back ENABLED; the paused row is indistinguishable from a live one")
				}
			})
		}
	})

	// THE CALL SHAPE driveWithUnusableGroups makes: user subjects only, groups
	// nil, on a deployment that HAS group-tier rows. The store answers the
	// everyone row and reports the tier honestly — it does not, and must not,
	// pretend the caller matched nothing, because "no groups were supplied" and
	// "this caller is in no group" are the same query and only the caller knows
	// which one it is. That is precisely why HasGroupTierDriveGrants exists and
	// why the API's refusal leans on it: without that second read the resolver
	// would serve this row to a member whose group grant the snapshot dropped.
	t.Run("the unusable-groups call shape still matches the everyone row", func(t *testing.T) {
		groupDrive := seedUserDrive(t, st, "zzz-unusable-grp-"+uniq)
		seedUserDriveGrant(t, st, types.UserDriveGrant{
			SubjectType: types.CapabilitySubjectGroup, Subject: "unusable-grp-" + uniq, DriveID: groupDrive.ID,
		})

		d, _, tier, err := st.ResolveUserDrive(ctx, []string{"nobody-" + uniq}, nil)
		if err != nil {
			t.Fatalf("resolve(users, nil): %v", err)
		}
		if d.ID != dAll.ID || tier != types.CapabilitySubjectAll {
			t.Fatalf("resolve = %q at tier %q, want the everyone row %q at tier all", d.Name, tier, dAll.Name)
		}
		has, err := st.HasGroupTierDriveGrants(ctx)
		if err != nil || !has {
			t.Fatalf("HasGroupTierDriveGrants = %v, %v; want true — it is the only thing that can tell this answer from a complete one", has, err)
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

// driveGroupTierWindowLockKey serializes the emptied-group-tier window below
// with any other process running this package against the same database. Same
// shape as db.migrateAdvisoryLockKey (ASCII, stable, blocking): "WARDYDGT" —
// Wardyn Drive Group Tier.
const driveGroupTierWindowLockKey int64 = 0x5741524459_444754

// hasGroupTierDriveGrantsWithNoGroupRow answers HasGroupTierDriveGrants against
// a database that has NO group-tier grant at all, and leaves the database
// exactly as it found it.
//
// The false direction cannot be asserted by deleting only this test's own row:
// these tests share one substrate (both drive tables are global, there is no
// per-run scoping — see this file's header), so a sibling's group-tier row is
// the normal case and a bare `want false` would be a flake. So the window is
// MADE. Under a session advisory lock on one checked-out connection — a second
// `go test` against the same DSN blocks on it rather than observing half a
// table — every group-tier row is parked in a temp table on that same
// connection, deleted, the gate is asked, and the rows go back. `SELECT *` in
// and `INSERT ... SELECT *` out carry every column and every id without a
// column list that the next migration would silently rot, so the sibling tests'
// by-id cleanups still find their rows afterwards.
//
// Every step's undo is DEFERRED: t.Fatal unwinds through deferred calls, so a
// failure inside the window still restores the table.
func hasGroupTierDriveGrantsWithNoGroupRow(t *testing.T, pool *pgxpool.Pool, st store.PG) bool {
	t.Helper()
	ctx := context.Background()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the group-tier window: %v", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, driveGroupTierWindowLockKey); err != nil {
		t.Fatalf("take the group-tier window lock: %v", err)
	}
	defer func() {
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, driveGroupTierWindowLockKey); err != nil {
			t.Errorf("release the group-tier window lock: %v", err)
		}
	}()

	// IF EXISTS: the pool hands back connections, so a run whose DROP below
	// failed must not poison the next one with a leftover parking table.
	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS pg_temp.parked_group_grants`); err != nil {
		t.Fatalf("clear a leftover parking table: %v", err)
	}
	if _, err := conn.Exec(ctx, `CREATE TEMP TABLE parked_group_grants AS SELECT * FROM user_drive_grants WHERE subject_type = 'group'`); err != nil {
		t.Fatalf("park the group-tier rows: %v", err)
	}
	defer func() {
		if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS pg_temp.parked_group_grants`); err != nil {
			t.Errorf("drop the parking table: %v", err)
		}
	}()

	if _, err := conn.Exec(ctx, `DELETE FROM user_drive_grants WHERE subject_type = 'group'`); err != nil {
		t.Fatalf("empty the group tier: %v", err)
	}
	defer func() {
		if _, err := conn.Exec(ctx, `INSERT INTO user_drive_grants SELECT * FROM parked_group_grants`); err != nil {
			t.Errorf("restore the parked group-tier rows: %v — this database has LOST every group-tier grant it held; drop and re-migrate it before trusting another run", err)
		}
	}()

	has, err := st.HasGroupTierDriveGrants(ctx)
	if err != nil {
		t.Fatalf("has group tier (no group-tier row in the database): %v", err)
	}
	return has
}

// TestPG_HasGroupTierDriveGrants pins the gate on the stale/truncated
// group-snapshot refusal: it must report TRUE only while a group-tier row
// actually exists. On a deployment with none, an unknown group snapshot could
// not have matched anything, so refusing there would lock out every pre-upgrade
// session on a deployment that allocates by user only.
//
// BOTH directions are ASSERTED, and the false one is the load-bearing half: it
// is the only thing that can fail a gate answering true unconditionally, and
// that gate is what turns every caller with a nil or truncated group snapshot
// into a 403 on a deployment that never allocated by group. A log line in its
// place fails nothing.
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

	// FALSE first, while the user-tier row is the only grant this test has
	// seeded: asked after the group row exists, the "a user row does not flip
	// the answer" claim would never be observed.
	if hasGroupTierDriveGrantsWithNoGroupRow(t, pool, st) {
		t.Error("HasGroupTierDriveGrants = true with only a user-tier row in the database, want false — that answer refuses every caller whose group snapshot is nil on a user-only deployment")
	}

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

	// And false again once that row is gone: the gate follows the table, it
	// does not latch on the first group grant a deployment ever wrote.
	if _, err := st.DeleteUserDriveGrant(ctx, group.ID); err != nil {
		t.Fatalf("delete group grant: %v", err)
	}
	if hasGroupTierDriveGrantsWithNoGroupRow(t, pool, st) {
		t.Error("HasGroupTierDriveGrants = true after the last group-tier row was deleted, want false")
	}
}
