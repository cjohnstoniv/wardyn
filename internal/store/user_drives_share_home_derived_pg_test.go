// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// PIN for the residue of the finding that 0059's "one directory, one principal"
// does not hold for SHARE drives.
//
// The half an admin can TYPE — the same home_override on two same-root shares —
// is closed by UpsertUserDriveGrant and pinned by
// TestPG_UserDriveGrant_TwoSharesOnOneRootCannotShareADirectory. This is the
// half nobody types, which that guard's own doc named as its residual and handed
// to the drive-write boundary: with no override at all, two same-root shares
// carrying DIFFERENT home_templates derive ONE home name for TWO principals.
// Drive A on `sub` and drive B on `email_local`, a member whose sub is "alice"
// and a member whose address is alice@corp.example: both derive "alice", both
// bind /srv/homes/alice, and every bind-time assertion passes because each
// allocation is individually legitimate.
//
// The refusal is TEMPLATE DISAGREEMENT, not an equal root: two views of one tree
// stay legal (the shape driveHostRootNesting deliberately permits, and the shape
// the existing share tests seed), because with equal templates the derivation is
// injective in the principal.
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

// shareDriveOn builds (but does not persist) a host_path drive on root with the
// given template — the row an admin's accepted POST would hand the store.
func shareDriveOn(name, root string, tmpl types.HomeTemplate) types.UserDrive {
	return types.UserDrive{
		Name:         name,
		Backend:      types.DriveBackendHostPath,
		HostRoot:     root,
		HomeTemplate: tmpl,
		Reclaim:      types.DriveReclaimRetain,
		CreatedBy:    "admin@example.com",
	}
}

func TestPG_UserDrive_TwoSharesOnOneRootMustDeriveHomesTheSameWay(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)

	root := "/srv/homes-" + uuid.NewString()[:8]

	bySub, err := st.UpsertUserDrive(ctx, shareDriveOn("test-derived-sub-"+uuid.NewString(), root, types.HomeTemplateSub))
	if err != nil {
		t.Fatalf("register the first share on %q: %v", root, err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, bySub.ID) })

	// THE PREMISE, asserted rather than assumed: two principals, two drives, one
	// absolute directory — with no override typed anywhere.
	byEmail := shareDriveOn("test-derived-email-"+uuid.NewString(), root, types.HomeTemplateEmailLocal)
	byEmail.ID = uuid.New()
	subHome, err := types.DriveHomeName(bySub, "alice", "")
	if err != nil {
		t.Fatalf("derive a home on the `sub` share: %v", err)
	}
	emailHome, err := types.DriveHomeName(byEmail, "alice@corp.example", "")
	if err != nil {
		t.Fatalf("derive a home on the `email_local` share: %v", err)
	}
	if a, b := types.DriveObjectName(bySub, subHome), types.DriveObjectName(byEmail, emailHome); a != b {
		t.Fatalf("the two shares derive %q and %q for two different principals; the collision this test pins is gone", a, b)
	}

	_, err = st.UpsertUserDrive(ctx, byEmail)
	if !errors.Is(err, store.ErrDriveHomeNamespaceConflict) {
		_ = st.DeleteUserDrive(ctx, byEmail.ID)
		t.Fatalf("registering a second host_path drive on %q that derives homes by a DIFFERENT rule: err = %v, want "+
			"ErrDriveHomeNamespaceConflict. A share's object name is <host_root>/<home> with no drive component, so "+
			"the member whose sub is \"alice\" and the member whose address is alice@corp.example are both allocated "+
			"%q read-write over each other's bytes — 0059's \"one home directory belongs to one principal\" failing on "+
			"the backend its index cannot reach", root, err, types.DriveObjectName(bySub, subHome))
	}
	if !errors.Is(err, store.ErrConflict) {
		t.Errorf("the refusal does not satisfy errors.Is(err, ErrConflict); every caller that only asks \"is this a 409?\" would 500")
	}

	// SCOPED, not a blanket ban — four directions.

	// 1. Two views of ONE tree stay legal while they agree on the derivation.
	//    This is the read-write/read-only pair the nesting gate deliberately
	//    permits, and refusing it would be the finding's other remediation
	//    breaking a documented deployment shape.
	twin, err := st.UpsertUserDrive(ctx, shareDriveOn("test-derived-twin-"+uuid.NewString(), root, types.HomeTemplateSub))
	if err != nil {
		t.Fatalf("a SECOND same-root share agreeing on home_template must stay legal: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, twin.ID) })

	// 2. Another root is another tree entirely.
	far, err := st.UpsertUserDrive(ctx,
		shareDriveOn("test-derived-far-"+uuid.NewString(), "/srv/other-"+uuid.NewString()[:8], types.HomeTemplateEmailLocal))
	if err != nil {
		t.Fatalf("a differing template on ANOTHER root must stay legal: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, far.ID) })

	// 3. A MANAGED drive carries the drive slug in its object name, so two of
	//    them are two namespaces however they derive homes.
	managed, err := st.UpsertUserDrive(ctx, types.UserDrive{
		Name: "test-derived-managed-" + uuid.NewString(), Backend: types.DriveBackendDockerVolume,
		HomeTemplate: types.HomeTemplateHash, Reclaim: types.DriveReclaimRetain, CreatedBy: "admin@example.com",
	})
	if err != nil {
		t.Fatalf("a managed drive must be untouched by a host_path root rule: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, managed.ID) })

	// 4. THE UPDATE PATH, on the same terms as a create: an EDIT that moves a
	//    drive's template into disagreement with its root-mates is the same act
	//    as registering it that way, and the guard gates ON CONFLICT DO UPDATE
	//    too. It must also not trip over the row's OWN stored values.
	if _, err := st.UpsertUserDrive(ctx, func() types.UserDrive {
		d := twin
		d.HomeTemplate = types.HomeTemplateEmailLocal
		return d
	}()); !errors.Is(err, store.ErrDriveHomeNamespaceConflict) {
		t.Errorf("editing a same-root share's home_template into disagreement: err = %v, want ErrDriveHomeNamespaceConflict", err)
	}
	if _, err := st.UpsertUserDrive(ctx, twin); err != nil {
		t.Errorf("re-writing a drive unchanged must not trip the guard on its own row: %v", err)
	}
}
