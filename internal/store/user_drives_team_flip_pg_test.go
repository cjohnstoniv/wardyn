// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A change of a drive's team mode against an allocation, in both commit orders and for a group's and a person's allocation.
// A test transaction stands in for the writer that is already in flight, holding the drive as that writer holds it, so
// the other writer waits behind it and then sees what committed:
//
//   - allocation first: the grant's insert holds the drive (FOR SHARE) with its row inserted but not committed. The edit
//     waits, then finds the allocation and is refused with ErrDriveAllocated, whatever the re-home guard answered
//     (refuseIfAllocated either way) and whoever the allocation names, so no run's folder changes.
//   - edit first: the edit holds the drive (FOR UPDATE, team changed) and the grant waits behind it, then judges the
//     allocation against the mode the drive now has: a person's allocation of a team drive and a members_can_write
//     allocation of an ordinary drive are refused with ErrDriveGrantTeamRule, and a plain group's, which both modes take,
//     is stored on the new mode.
//
// Each order has one outcome and the drive ends in the state that outcome names.
func TestPG_DriveTeamFlip_OrderedAgainstAllocation(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	for _, c := range []struct {
		label       string
		subjectType types.CapabilitySubjectType
		subject     string
		startTeam   bool
		membersCan  bool
		// editFirstRefused: the grant that waits behind a committed flip is refused, not stored.
		editFirstRefused bool
	}{
		{"a group's allocation, ordinary drive made a team drive", types.CapabilitySubjectGroup, "platform", false, false, false},
		{"a person's allocation, ordinary drive made a team drive", types.CapabilitySubjectUser, "alice@example.com", false, false, true},
		{"a group's members_can_write allocation, team drive made ordinary", types.CapabilitySubjectGroup, "platform", true, true, true},
	} {
		seed := func(t *testing.T) types.UserDrive {
			t.Helper()
			name := "flip-" + uuid.NewString()[:8]
			d := seedUserDrive(t, st, name)
			if c.startTeam {
				d.Team, d.Writable = true, true
				var err error
				if d, err = st.UpsertUserDrive(ctx, d, false); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { _, _ = pool.Exec(ctx, `DELETE FROM user_drive_grants WHERE drive_id = $1`, d.ID) })
			return d
		}
		grant := func(d types.UserDrive) types.UserDriveGrant {
			return types.UserDriveGrant{SubjectType: c.subjectType, Subject: c.subject, DriveID: d.ID, Enabled: true,
				CreatedBy: "admin@example.com", MembersCanWrite: c.membersCan}
		}
		for _, refuseIfAllocated := range []bool{false, true} {
			t.Run(c.label+"/allocation first/refuseIfAllocated="+boolWord(refuseIfAllocated), func(t *testing.T) {
				d := seed(t)
				edit := d
				edit.Team = !c.startTeam
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback(ctx) //nolint:errcheck // already committed on the happy path
				if _, err := tx.Exec(ctx, `SELECT 1 FROM user_drives WHERE id = $1 FOR SHARE`, d.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, `INSERT INTO user_drive_grants (id, subject_type, subject, drive_id, enabled, created_by, members_can_write)
					VALUES ($1, $2, $3, $4, true, 'x', $5)`, uuid.New(), string(c.subjectType), c.subject, d.ID, c.membersCan); err != nil {
					t.Fatal(err)
				}
				blocked, result := parked(func() error { _, err := st.UpsertUserDrive(ctx, edit, refuseIfAllocated); return err })
				if !blocked {
					_ = tx.Commit(ctx)
					t.Fatal("the edit did not wait for the allocation in flight")
				}
				waitForPortalLock(t, "the edit waiting on the drive row", func() bool { return backendsWaiting(t, pool, false, "FROM user_drives") > 0 })
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				if err := result(); !errors.Is(err, store.ErrDriveAllocated) {
					t.Errorf("the edit behind a committed allocation: err = %v, want ErrDriveAllocated", err)
				}
				if got, err := st.GetUserDrive(ctx, d.ID); err != nil || got.Team != c.startTeam {
					t.Errorf("drive = %+v, %v; want team %v unchanged", got, err, c.startTeam)
				}
			})
		}
		t.Run(c.label+"/edit first", func(t *testing.T) {
			d := seed(t)
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(ctx) //nolint:errcheck // already committed on the happy path
			if _, err := tx.Exec(ctx, `SELECT 1 FROM user_drives WHERE id = $1 FOR UPDATE`, d.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `UPDATE user_drives SET team = $2 WHERE id = $1`, d.ID, !c.startTeam); err != nil {
				t.Fatal(err)
			}
			blocked, result := parked(func() error { _, err := st.AddUserDriveGrant(ctx, grant(d), true); return err })
			if !blocked {
				_ = tx.Commit(ctx)
				t.Fatal("the allocation did not wait for the edit in flight")
			}
			waitForPortalLock(t, "the allocation waiting on the drive row", func() bool { return backendsWaiting(t, pool, false, "FROM user_drives") > 0 })
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			err = result()
			rows := rowCount(t, pool, `SELECT count(*) FROM user_drive_grants WHERE drive_id = $1`, d.ID)
			switch {
			case c.editFirstRefused && (!errors.Is(err, store.ErrDriveGrantTeamRule) || rows != 0):
				t.Errorf("the allocation behind a committed flip: err = %v, %d row(s); want ErrDriveGrantTeamRule and none", err, rows)
			case !c.editFirstRefused && (err != nil || rows != 1):
				t.Errorf("the allocation behind a committed flip: err = %v, %d row(s); want it stored on the new mode", err, rows)
			}
			if got, err := st.GetUserDrive(ctx, d.ID); err != nil || got.Team == c.startTeam {
				t.Errorf("drive = %+v, %v; want the flip kept", got, err)
			}
		})
	}
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
