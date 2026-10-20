// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

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

// TestPG_TeamDrive_StoresTheFlagAndReadOnlyByDefault: team survives a write and
// every read path, and an allocation that does not say members_can_write reads
// back false (#1988: read-only is the allocation default).
func TestPG_TeamDrive_StoresTheFlagAndReadOnlyByDefault(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	uniq := uuid.NewString()[:8]
	d := newUserDrive("team-" + uniq)
	d.Team, d.Writable = true, true
	team, err := st.UpsertUserDrive(ctx, d, false)
	if err != nil {
		t.Fatalf("write a team drive: %v", err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, team.ID) })
	if !team.Team {
		t.Fatal("the write answered team=false")
	}
	if got, err := st.GetUserDrive(ctx, team.ID); err != nil || !got.Team {
		t.Fatalf("GetUserDrive = %+v, %v; want team", got, err)
	}
	group := "grp-" + uniq
	ro := addUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectGroup, Subject: group,
		DriveID: team.ID, Enabled: true, CreatedBy: "admin@example.com"})
	if ro.MembersCanWrite {
		t.Fatal("an allocation that never said members_can_write can write")
	}
	var col bool
	if err := pool.QueryRow(ctx, `SELECT members_can_write FROM user_drive_grants WHERE id = $1`, ro.ID).Scan(&col); err != nil || col {
		t.Fatalf("members_can_write column = %v, %v; want false", col, err)
	}
	ws, err := st.ResolveUserDrives(ctx, []string{"sub-" + uniq}, []string{group}, "")
	if err != nil || len(ws) != 1 || !ws[0].Drive.Team || ws[0].Grant.MembersCanWrite {
		t.Fatalf("ResolveUserDrives = %+v, %v; want the team drive, read-only", ws, err)
	}
	ro.MembersCanWrite = true
	rw, err := st.UpsertUserDriveGrant(ctx, ro, true)
	if err != nil {
		t.Fatalf("tick members_can_write: %v", err)
	}
	if !rw.MembersCanWrite {
		t.Fatal("members_can_write did not stick")
	}
	one, g, _, err := st.ResolveUserDrive(ctx, []string{"sub-" + uniq}, []string{group}, "")
	if err != nil || !one.Team || !g.MembersCanWrite {
		t.Fatalf("ResolveUserDrive = %+v %+v, %v; want team and members_can_write", one, g, err)
	}
	items, err := st.ListUserDrives(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.ID == team.ID && !it.Team {
			t.Fatal("ListUserDrives dropped team")
		}
	}
}

// TestPG_TeamDrive_NonMemberGetsNoMount: a team drive allocated to one group
// reaches its members and nobody else.
func TestPG_TeamDrive_NonMemberGetsNoMount(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	uniq := uuid.NewString()[:8]
	d := newUserDrive("team-" + uniq)
	d.Team = true
	team, err := st.UpsertUserDrive(ctx, d, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, team.ID) })
	addUserDriveGrant(t, st, types.UserDriveGrant{SubjectType: types.CapabilitySubjectGroup, Subject: "grp-" + uniq,
		DriveID: team.ID, Enabled: true, CreatedBy: "admin@example.com"})
	ws, err := st.ResolveUserDrives(ctx, []string{"sub-out-" + uniq}, []string{"other-" + uniq}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws {
		if w.Drive.ID == team.ID {
			t.Fatalf("a non-member reached the team drive: %+v", w)
		}
	}
}

// TestPG_TeamDrive_CheckKeepsItAnAdminShare: the schema refuses a team flag on a
// person's own drive (so the leaver purge, which deletes a person's own drives,
// never reaches one) and on a ReadWriteOnce k8s_pvc claim.
func TestPG_TeamDrive_CheckKeepsItAnAdminShare(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	uniq := uuid.NewString()[:8]
	d := seedUserDrive(t, st, "plain-"+uniq)
	for name, q := range map[string]string{
		"a team k8s_pvc row":           `UPDATE user_drives SET team = true, backend = 'k8s_pvc' WHERE id = $1`,
		"a team row owned by a person": `UPDATE user_drives SET team = true, owner_subject = 'sub-` + uniq + `' WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, q, d.ID); err == nil || !strings.Contains(err.Error(), "user_drives_team_shape") {
			t.Errorf("%s: err = %v, want the user_drives_team_shape check", name, err)
		}
	}
	owned, err := st.ListMemberDrivesOf(ctx, "sub-"+uniq)
	if err != nil || len(owned) != 0 {
		t.Fatalf("ListMemberDrivesOf = %+v, %v; want none", owned, err)
	}
}

// TestPG_TeamDrive_AdditionalReSendUpdatesMembersCanWrite: re-sending a group's
// allocation of a drive with additional updates members_can_write in place, and
// below a super admin newly setting it is refused as a widening.
func TestPG_TeamDrive_AdditionalReSendUpdatesMembersCanWrite(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	uniq := uuid.NewString()[:8]
	d := newUserDrive("team-" + uniq)
	d.Team, d.Writable = true, true
	team, err := st.UpsertUserDrive(ctx, d, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.DeleteUserDrive(ctx, team.ID) })
	g := types.UserDriveGrant{SubjectType: types.CapabilitySubjectGroup, Subject: "grp-" + uniq, DriveID: team.ID, Enabled: true, CreatedBy: "admin@example.com"}
	first := addUserDriveGrant(t, st, g)
	var widens *store.UserDriveGrantWidensError
	g.MembersCanWrite = true
	if _, _, err := st.UpsertUserDriveGrantWithinTier(ctx, g, true, true); !errors.As(err, &widens) || widens.Field != "members_can_write" {
		t.Fatalf("security-admin tier set members_can_write: err = %v, want a members_can_write widening", err)
	}
	again, err := st.AddUserDriveGrant(ctx, g, true)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || !again.MembersCanWrite {
		t.Fatalf("re-send = %+v, want row %s updated to members_can_write", again, first.ID)
	}
}
