// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// PIN for the review finding that 0059's "one directory, one principal" did not
// hold for SHARE drives.
//
// Both halves of the mechanism keyed on drive_id — the store's NOT EXISTS guard
// and 0059's partial unique index — but a host_path object name carries no
// drive component at all: types.DriveObjectName returns
// <host_root>/<home> for host_path and wardyn-drive-<drive-slug>-<home> for
// every managed backend. So two share drives registered on ONE host_root, with
// the same home_override typed on a grant against each, handed TWO principals
// ONE absolute host directory: both bind it at /home/agent/drive, read-write
// wherever their allocation is writable, and every bind-time assertion passes
// because each one is individually legitimate.
//
// The rule is therefore stated over the namespace the OBJECT NAME actually
// occupies rather than over the row that happens to carry it. Managed backends
// keep the per-drive rule unchanged, which the negative control below pins:
// their name carries the drive slug, so one directory word does not get
// reserved across a deployment.
//
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// seedShareDrive persists a host_path ("share") drive on the given root. The
// store writes rows verbatim — types.ValidateUserDrive runs at the API boundary
// — so this is the same row an admin's accepted POST would leave behind.
func seedShareDrive(t *testing.T, st store.PG, name, hostRoot string) types.UserDrive {
	t.Helper()
	ctx := context.Background()
	d, err := st.UpsertUserDrive(ctx, types.UserDrive{
		Name:         name,
		Backend:      types.DriveBackendHostPath,
		HostRoot:     hostRoot,
		HomeTemplate: types.HomeTemplateEmailLocal,
		Reclaim:      types.DriveReclaimRetain,
		CreatedBy:    "admin@example.com",
	})
	if err != nil {
		t.Fatalf("seed share drive %q on %q: %v", name, hostRoot, err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, d.ID) })
	return d
}

func TestPG_UserDriveGrant_TwoSharesOnOneRootCannotShareADirectory(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	root := "/srv/homes-" + uuid.NewString()[:8]
	rw := seedShareDrive(t, st, "test-share-rw-"+uuid.NewString(), root)
	ro := seedShareDrive(t, st, "test-share-ro-"+uuid.NewString(), root)

	home := "home-" + uuid.NewString()[:8]

	// The premise, asserted rather than assumed: on this backend the two drives
	// resolve ONE absolute directory for one home name. If that ever stops being
	// true the rest of this test is pinning nothing.
	if a, b := types.DriveObjectName(rw, home), types.DriveObjectName(ro, home); a != b {
		t.Fatalf("DriveObjectName differs across two same-root share drives (%q vs %q); the collision this test pins is gone", a, b)
	}

	bob := "test-user-bob-" + uuid.NewString()
	alice := "test-user-alice-" + uuid.NewString()
	seedUserDriveGrant(t, st, types.UserDriveGrant{
		SubjectType: types.CapabilitySubjectUser, Subject: bob, DriveID: rw.ID,
		HomeOverride: home, CreatedBy: "admin@example.com",
	})

	_, err := st.UpsertUserDriveGrant(ctx, types.UserDriveGrant{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: alice, DriveID: ro.ID,
		HomeOverride: home, Enabled: true, CreatedBy: "admin@example.com",
	}, true)
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("a second principal on the OTHER share drive over the same root: err = %v, want ErrConflict — "+
			"a host_path object name is <host_root>/<home> with no drive component, so both principals would bind "+
			"%q read-write over each other's bytes while every bind-time assertion passes",
			err, types.DriveObjectName(ro, home))
	}

	// SCOPED, not global — three directions, so the fix cannot be a blanket ban
	// on a repeated directory name.

	// 1. Another ROOT is another tree entirely.
	otherRoot := seedShareDrive(t, st, "test-share-far-"+uuid.NewString(), "/srv/other-"+uuid.NewString()[:8])
	erin := "test-user-erin-" + uuid.NewString()
	seedUserDriveGrant(t, st, types.UserDriveGrant{
		SubjectType: types.CapabilitySubjectUser, Subject: erin, DriveID: otherRoot.ID,
		HomeOverride: home, CreatedBy: "admin@example.com",
	})

	// 2. A MANAGED drive still scopes per drive: its object name carries the
	//    drive slug, so one person's directory word must not be reserved across
	//    the deployment.
	managedA := seedUserDrive(t, st, "test-share-managed-a-"+uuid.NewString())
	managedB := seedUserDrive(t, st, "test-share-managed-b-"+uuid.NewString())
	if a, b := types.DriveObjectName(managedA, home), types.DriveObjectName(managedB, home); a == b {
		t.Fatalf("two managed drives mint ONE object name %q; the per-drive scope below is no longer the right rule", a)
	}
	frank := "test-user-frank-" + uuid.NewString()
	grace := "test-user-grace-" + uuid.NewString()
	seedUserDriveGrant(t, st, types.UserDriveGrant{
		SubjectType: types.CapabilitySubjectUser, Subject: frank, DriveID: managedA.ID,
		HomeOverride: home, CreatedBy: "admin@example.com",
	})
	seedUserDriveGrant(t, st, types.UserDriveGrant{
		SubjectType: types.CapabilitySubjectUser, Subject: grace, DriveID: managedB.ID,
		HomeOverride: home, CreatedBy: "admin@example.com",
	})

	// 3. The holder repointing its OWN row across the two same-root shares is
	//    not a clash with itself: one principal, one directory, still true.
	repointed, err := st.UpsertUserDriveGrant(ctx, types.UserDriveGrant{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectUser, Subject: bob, DriveID: ro.ID,
		HomeOverride: home, Priority: 7, Enabled: true, CreatedBy: "admin@example.com",
	}, true)
	if err != nil {
		t.Fatalf("the holder moving its own grant to the other same-root share: %v", err)
	}
	if repointed.DriveID != ro.ID || repointed.HomeOverride != home {
		t.Errorf("repointed = %+v, want drive %s and home %q", repointed, ro.ID, home)
	}
	t.Cleanup(func() { _, _ = st.DeleteUserDriveGrant(ctx, repointed.ID) })
}
