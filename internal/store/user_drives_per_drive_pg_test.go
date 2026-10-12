// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
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
	// the same drive is switched off and must win. Both are second drives of
	// their subject, so they are added beside the first.
	addUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectGroup, Subject: group, DriveID: paused.ID, Enabled: true, CreatedBy: "admin@example.com"})
	off := addUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectUser, Subject: sub, DriveID: paused.ID, CreatedBy: "admin@example.com"})

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

// addUserDriveGrant allocates g beside the subject's other drives and
// registers its cleanup.
func addUserDriveGrant(t *testing.T, st store.PG, g types.UserDriveGrant) types.UserDriveGrant {
	t.Helper()
	saved, err := st.AddUserDriveGrant(context.Background(), g, true)
	if err != nil {
		t.Fatalf("add grant %s %s: %v", g.SubjectType, g.Subject, err)
	}
	t.Cleanup(func() { _, _ = st.DeleteUserDriveGrant(context.Background(), saved.ID) })
	return saved
}

func subjectGrants(t *testing.T, st store.PG, subject string) []types.UserDriveGrant {
	t.Helper()
	all, err := st.ListUserDriveGrants(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var out []types.UserDriveGrant
	for _, g := range all {
		if g.Subject == subject {
			out = append(out, g)
		}
	}
	return out
}

// TestPG_AddAndReplaceUserDriveGrants: the additive verb keeps a subject's
// first drive beside a second (migration 0169) and updates the row it already
// has for a drive; the replacing verb leaves the subject with exactly the drive
// it names, repointing the earliest row and removing the rest.
func TestPG_AddAndReplaceUserDriveGrants(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	uniq := uuid.NewString()[:8]
	a := seedUserDrive(t, st, "per-drive-a-"+uniq)
	b := seedUserDrive(t, st, "per-drive-b-"+uniq)
	c := seedUserDrive(t, st, "per-drive-c-"+uniq)
	sub := "sub-" + uniq
	grant := func(d types.UserDrive, priority int) types.UserDriveGrant {
		return types.UserDriveGrant{SubjectType: types.CapabilitySubjectUser, Subject: sub, DriveID: d.ID, Priority: priority, Enabled: true, CreatedBy: "admin@example.com"}
	}

	first := addUserDriveGrant(t, st, grant(a, 0))
	second := addUserDriveGrant(t, st, grant(b, 0))
	if first.ID == second.ID || len(subjectGrants(t, st, sub)) != 2 {
		t.Fatalf("adding a second drive replaced the first: %+v", subjectGrants(t, st, sub))
	}
	again, err := st.AddUserDriveGrant(ctx, grant(a, 7), true)
	if err != nil || again.ID != first.ID || again.Priority != 7 {
		t.Errorf("re-adding drive a = %+v, %v; want the existing row %s updated to 7", again, err, first.ID)
	}

	replaced, removed, err := st.ReplaceUserDriveGrant(ctx, grant(c, 3), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || !slices.Contains(removed, a.ID) || !slices.Contains(removed, b.ID) {
		t.Errorf("removed = %v, want drives a and b, the ones the subject lost", removed)
	}
	t.Cleanup(func() { _, _ = st.DeleteUserDriveGrant(ctx, replaced.ID) })
	rows := subjectGrants(t, st, sub)
	if len(rows) != 1 || rows[0].DriveID != c.ID || replaced.ID != first.ID {
		t.Errorf("replacing with drive c left %+v (returned id %s); want one row, drive c, the earliest row %s repointed", rows, replaced.ID, first.ID)
	}
}

// TestPG_ConcurrentReplacesLeaveOneDrive: replacing writes for a subject with
// no allocation yet, racing, still leave the subject exactly one drive (the
// per-subject advisory lock); without it both insert.
func TestPG_ConcurrentReplacesLeaveOneDrive(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	uniq := uuid.NewString()[:8]
	a := seedUserDrive(t, st, "race-a-"+uniq)
	b := seedUserDrive(t, st, "race-b-"+uniq)
	for trial := range 20 {
		sub := fmt.Sprintf("race-%s-%d", uniq, trial)
		var wg sync.WaitGroup
		for _, d := range []types.UserDrive{a, b} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := st.UpsertUserDriveGrant(ctx, types.UserDriveGrant{SubjectType: types.CapabilitySubjectUser, Subject: sub, DriveID: d.ID, Enabled: true}, true); err != nil {
					t.Errorf("replace %s: %v", sub, err)
				}
			}()
		}
		wg.Wait()
		rows := subjectGrants(t, st, sub)
		for _, g := range rows {
			t.Cleanup(func() { _, _ = st.DeleteUserDriveGrant(ctx, g.ID) })
		}
		if len(rows) != 1 {
			t.Fatalf("trial %d: subject holds %d drives after two racing replaces, want 1", trial, len(rows))
		}
	}
}
