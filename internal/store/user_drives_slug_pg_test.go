// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// PIN for the finding that two distinct user_drives rows mint ONE storage-object
// name: the drive-name slug is not injective, so 0059's (drive_id,
// home_override) uniqueness does not hold in the namespace it claims.
//
// UNIQUE(name) admits "Corp NAS" and "corp nas" as two names. Both mint
// wardyn-drive-corp-nas-<home>, so two drives — each with its own size ceiling,
// writable flag and reclaim policy — address one volume or one claim, and the
// collision was caught only at mount time by the runners' wardyn.drive label
// check, as somebody's run failing. Migration 0061 puts a partial UNIQUE index
// on user_drives.name_slug, which store.UpsertUserDrive writes from
// types.DriveSlug(name), so the refusal happens at the write that causes it.
//
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mintedDrive builds (but does not persist) a drive on a backend whose object
// name Wardyn mints.
func mintedDrive(name string) types.UserDrive {
	return types.UserDrive{
		ID:           uuid.New(),
		Name:         name,
		Backend:      types.DriveBackendDockerVolume,
		HomeTemplate: types.HomeTemplateHash,
		Reclaim:      types.DriveReclaimRetain,
		CreatedBy:    "admin@example.com",
	}
}

func TestPG_UserDrive_TwoNamesThatFoldToOneObjectAreRefused(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	// A base name unique to this run, so the pair collides with each other and
	// with nothing else in the lane.
	base := "test-slug-" + uuid.NewString()[:8]

	first, err := st.UpsertUserDrive(ctx, mintedDrive(base+" nas"))
	if err != nil {
		t.Fatalf("register the first drive: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, first.ID) })

	// THE PREMISE, asserted rather than assumed: two names UNIQUE(name) admits
	// as different, minting ONE object for one home.
	clash := mintedDrive(strings.ToUpper(base[:1]) + base[1:] + "   NAS!")
	if a, b := types.DriveObjectName(first, "bsmith"), types.DriveObjectName(clash, "bsmith"); a != b {
		t.Fatalf("%q and %q mint %q and %q; the collision this test pins is gone", first.Name, clash.Name, a, b)
	}
	if first.Name == clash.Name {
		t.Fatalf("the two names are equal (%q); UNIQUE(name) would refuse this and the test would prove nothing", first.Name)
	}

	_, err = st.UpsertUserDrive(ctx, clash)
	if !errors.Is(err, store.ErrDriveSlugConflict) {
		_ = st.DeleteUserDrive(ctx, clash.ID)
		t.Fatalf("registering %q while %q exists: err = %v, want ErrDriveSlugConflict. Both mint %q, so the two "+
			"drives hand ONE storage object to two sets of members with different size ceilings, writability and "+
			"reclaim policy — and nothing but the runner's mount-time label check would have said so",
			clash.Name, first.Name, err, types.DriveObjectName(first, "bsmith"))
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("the refusal does not satisfy errors.Is(err, ErrConflict); every caller that only asks %q would 500",
			"is this a 409?")
	}

	// SCOPED, not a blanket ban — three directions.

	// 1. A name that folds to something ELSE is untouched.
	far, err := st.UpsertUserDrive(ctx, mintedDrive(base+" other"))
	if err != nil {
		t.Fatalf("a drive whose name folds differently must stay legal: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, far.ID) })

	// 2. host_path carries NO slug in its object name (<host_root>/<home>), so
	//    two shares whose names fold together collide with nothing and must be
	//    accepted. This is the half a non-partial index would have broken.
	root := "/srv/slug-" + uuid.NewString()[:8]
	shareA, err := st.UpsertUserDrive(ctx, shareDriveOn(base+" share", root, types.HomeTemplateSub))
	if err != nil {
		t.Fatalf("register the first share: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, shareA.ID) })
	shareB := shareDriveOn(strings.ToUpper(base[:1])+base[1:]+"   SHARE!", root, types.HomeTemplateSub)
	shareB.ID = uuid.New()
	saved, err := st.UpsertUserDrive(ctx, shareB)
	if err != nil {
		t.Fatalf("two host_path drives whose NAMES fold together must stay legal — a share's object name is "+
			"<host_root>/<home> and carries no slug at all: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, saved.ID) })

	// 3. Re-writing a drive under its OWN name is not a collision with itself:
	//    the index is on the row, and the upsert's ON CONFLICT (id) path has to
	//    carry the slug through.
	first.SizeMiB = 4096
	if _, err := st.UpsertUserDrive(ctx, first); err != nil {
		t.Fatalf("editing a drive in place must not collide with its own name_slug: %v", err)
	}
}

// TestPG_UserDrive_RenameOntoAnotherDrivesFoldIsRefused covers the UPDATE arm:
// the create path is not the only way to reach the collision, and ON CONFLICT
// (id) DO UPDATE writes name_slug too.
func TestPG_UserDrive_RenameOntoAnotherDrivesFoldIsRefused(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	base := "test-rename-" + uuid.NewString()[:8]
	held, err := st.UpsertUserDrive(ctx, mintedDrive(base+" nas"))
	if err != nil {
		t.Fatalf("register the drive that holds the fold: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, held.ID) })

	other, err := st.UpsertUserDrive(ctx, mintedDrive(base+" other"))
	if err != nil {
		t.Fatalf("register the drive to be renamed: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, other.ID) })

	other.Name = strings.ToUpper(base[:1]) + base[1:] + "   NAS!"
	if _, err := st.UpsertUserDrive(ctx, other); !errors.Is(err, store.ErrDriveSlugConflict) {
		t.Fatalf("renaming %q onto %q's fold: err = %v, want ErrDriveSlugConflict — a rename re-homes every member "+
			"of the renamed drive onto the object the other drive already owns", other.ID, held.Name, err)
	}
}
