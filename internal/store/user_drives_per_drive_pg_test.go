// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_ResolveUserDrives_OneWinnerPerDrive: every drive a caller's grants
// reach comes back once, won by that drive's own top grant; a disabled winner
// comes back disabled rather than falling through to the wider grant beneath
// it; and the first winner is ResolveUserDrive's one answer (the legacy drive).
// Guarded by WARDYN_TEST_PG.
func TestPG_ResolveUserDrives_OneWinnerPerDrive(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	uniq := uuid.NewString()[:8]
	// Alphabetical order is the opposite of the tier order, so a pass is the
	// tier rule's and not name ASC's.
	personal := seedUserDrive(t, st, "zz-personal-"+uniq)
	team := seedUserDrive(t, st, "mm-team-"+uniq)
	paused := seedUserDrive(t, st, "aa-paused-"+uniq)
	sub, group := "sub-"+uniq, "grp-"+uniq

	seedUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectUser, Subject: sub, DriveID: personal.ID, CreatedBy: "admin@example.com"})
	seedUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectGroup, Subject: group, DriveID: team.ID, CreatedBy: "admin@example.com"})
	// The paused drive: a group grant would mount it, the person's own grant on
	// the same drive is switched off and must win.
	seedUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectGroup, Subject: group, DriveID: paused.ID, CreatedBy: "admin@example.com"})
	off, err := st.UpsertUserDriveGrant(ctx, types.UserDriveGrant{SubjectType: types.CapabilitySubjectUser, Subject: sub, DriveID: paused.ID, CreatedBy: "admin@example.com"}, true)
	if err != nil {
		t.Fatalf("seed the disabled grant: %v", err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUserDriveGrant(ctx, off.ID) })

	ws, err := st.ResolveUserDrives(ctx, []string{sub}, []string{group}, "")
	if err != nil {
		t.Fatal(err)
	}
	byDrive := map[uuid.UUID]store.UserDriveWinner{}
	for _, w := range ws {
		if _, dup := byDrive[w.Drive.ID]; dup {
			t.Fatalf("drive %s came back twice", w.Drive.Name)
		}
		byDrive[w.Drive.ID] = w
	}
	for _, d := range []types.UserDrive{personal, team, paused} {
		if _, ok := byDrive[d.ID]; !ok {
			t.Fatalf("drive %s is missing from %+v", d.Name, ws)
		}
	}
	if w := byDrive[paused.ID]; w.Grant.ID != off.ID || w.Grant.Enabled {
		t.Errorf("paused drive won by %+v, want the disabled user grant %s (no fall-through to the group grant)", w.Grant, off.ID)
	}
	if w := byDrive[team.ID]; w.Grant.SubjectType != types.CapabilitySubjectGroup {
		t.Errorf("team drive won at tier %s, want group", w.Grant.SubjectType)
	}
	legacy, grant, _, err := st.ResolveUserDrive(ctx, []string{sub}, []string{group}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ws[0].Drive.ID != legacy.ID || ws[0].Grant.ID != grant.ID {
		t.Errorf("first winner = drive %s grant %s, want ResolveUserDrive's drive %s grant %s", ws[0].Drive.Name, ws[0].Grant.ID, legacy.Name, grant.ID)
	}

	// Another caller's grants reach none of these.
	other, err := st.ResolveUserDrives(ctx, []string{"sub-other-" + uniq}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range other {
		if w.Drive.ID == personal.ID || w.Drive.ID == team.ID || w.Drive.ID == paused.ID {
			t.Errorf("a stranger reaches %s", w.Drive.Name)
		}
	}
}

// TestPG_UpsertUserDriveGrant_PerDriveKey: a subject granted a second drive
// keeps the first grant beside it (migration 0166), and writing the same
// subject and drive again updates that row and returns its id.
func TestPG_UpsertUserDriveGrant_PerDriveKey(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	uniq := uuid.NewString()[:8]
	a := seedUserDrive(t, st, "per-drive-a-"+uniq)
	b := seedUserDrive(t, st, "per-drive-b-"+uniq)
	sub := "sub-" + uniq

	first := seedUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectUser, Subject: sub, DriveID: a.ID, CreatedBy: "admin@example.com"})
	second := seedUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectUser, Subject: sub, DriveID: b.ID, CreatedBy: "admin@example.com"})
	if first.ID == second.ID {
		t.Fatalf("the second drive repointed the first grant (%s)", first.ID)
	}
	again, err := st.UpsertUserDriveGrant(ctx, types.UserDriveGrant{SubjectType: types.CapabilitySubjectUser, Subject: sub, DriveID: a.ID, Priority: 7, Enabled: true, CreatedBy: "admin@example.com"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || again.Priority != 7 {
		t.Errorf("re-upsert = id %s priority %d, want the existing row %s updated to 7", again.ID, again.Priority, first.ID)
	}
	ws, err := st.ResolveUserDrives(ctx, []string{sub}, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	mine := 0
	for _, w := range ws {
		if w.Drive.ID == a.ID || w.Drive.ID == b.ID {
			mine++
		}
	}
	if mine != 2 {
		t.Errorf("reached %d of the subject's drives, want both", mine)
	}
}
