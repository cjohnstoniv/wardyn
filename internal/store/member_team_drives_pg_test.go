// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// A person's own team drive (#3459) against a throwaway Postgres database. Guarded by WARDYN_TEST_PG; skipped
// cleanly when unset.

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func teamDriveUnit(t *testing.T, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO governance_units (id, name, ceiling, limits, created_by) VALUES ($1,$2,'{}','{}','t')`, id, name); err != nil {
		t.Fatalf("insert unit %s: %v", name, err)
	}
	return id
}

// TestCreateMemberTeamDrive_PG: the row is a member drive with team on and the unit set, it counts toward the
// person's limit, and an empty owner is refused.
func TestCreateMemberTeamDrive_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	unit := teamDriveUnit(t, pool, "platform")

	d, err := st.CreateMemberTeamDrive(ctx, memberDrive("alice", "shared"), unit, 2)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !d.Team || d.OwnerSubject != "alice" {
		t.Fatalf("created row: team=%v owner=%q, want a team row owned by alice", d.Team, d.OwnerSubject)
	}
	got, err := st.GetMemberTeamDrive(ctx, d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UnitID == nil || *got.UnitID != unit || got.MembersCanWrite {
		t.Fatalf("team facts: unit=%v write=%v, want %s and false", got.UnitID, got.MembersCanWrite, unit)
	}
	if _, err := st.CreateMemberDrive(ctx, memberDrive("alice", "own"), 2); err != nil {
		t.Fatalf("second drive: %v", err)
	}
	if _, err := st.CreateMemberTeamDrive(ctx, memberDrive("alice", "third"), unit, 2); !errors.Is(err, store.ErrMemberDriveLimit) {
		t.Fatalf("third drive over a limit of 2: err=%v, want ErrMemberDriveLimit", err)
	}
	if _, err := st.CreateMemberTeamDrive(ctx, memberDrive("", "none"), unit, 2); err == nil {
		t.Fatal("an empty owner made a team drive")
	}
	plain, err := st.CreateMemberDrive(ctx, memberDrive("bob", "plain"), 2)
	if err != nil {
		t.Fatalf("plain drive: %v", err)
	}
	if _, err := st.GetMemberTeamDrive(ctx, plain.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a plain member drive read as a team drive: err=%v", err)
	}
}

// TestListReachableMemberTeamDrives_PG: a person reaches the team drives of the units given and their own, never
// another unit's, a plain member drive, or one being deleted.
func TestListReachableMemberTeamDrives_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	mine, other := teamDriveUnit(t, pool, "platform"), teamDriveUnit(t, pool, "sales")

	inUnit, err := st.CreateMemberTeamDrive(ctx, memberDrive("bob", "a-unit"), mine, 5)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	own, err := st.CreateMemberTeamDrive(ctx, memberDrive("alice", "b-own"), other, 5)
	if err != nil {
		t.Fatalf("create own: %v", err)
	}
	if _, err := st.CreateMemberTeamDrive(ctx, memberDrive("carol", "c-other"), other, 5); err != nil {
		t.Fatalf("create other: %v", err)
	}
	if _, err := st.CreateMemberDrive(ctx, memberDrive("bob", "d-plain"), 5); err != nil {
		t.Fatalf("create plain: %v", err)
	}
	gone, err := st.CreateMemberTeamDrive(ctx, memberDrive("bob", "e-deleting"), mine, 5)
	if err != nil {
		t.Fatalf("create deleting: %v", err)
	}
	if _, _, err := st.BeginMemberDriveDelete(ctx, "bob", gone.ID); err != nil {
		t.Fatalf("tombstone: %v", err)
	}

	rows, err := st.ListReachableMemberTeamDrives(ctx, "alice", []uuid.UUID{mine})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var ids []uuid.UUID
	for _, r := range rows {
		ids = append(ids, r.Drive.ID)
	}
	if len(ids) != 2 || ids[0] != inUnit.ID || ids[1] != own.ID {
		t.Fatalf("reached %v, want [%s %s] (the unit's drive, then alice's own)", ids, inUnit.ID, own.ID)
	}
	if _, err := st.ListReachableMemberTeamDrives(ctx, "", []uuid.UUID{mine}); err == nil {
		t.Fatal("an empty owner listed team drives")
	}
}

// TestSetMemberTeamDriveWrite_PG: the bit is set and returned with its value before; a drive being deleted is
// busy and keeps its bit; a plain member drive is not found.
func TestSetMemberTeamDriveWrite_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	unit := teamDriveUnit(t, pool, "platform")
	d, err := st.CreateMemberTeamDrive(ctx, memberDrive("alice", "shared"), unit, 5)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	before, after, err := st.SetMemberTeamDriveWrite(ctx, d.ID, true)
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if before.MembersCanWrite || !after.MembersCanWrite {
		t.Fatalf("before=%v after=%v, want false then true", before.MembersCanWrite, after.MembersCanWrite)
	}
	if _, _, err := st.BeginMemberDriveDelete(ctx, "alice", d.ID); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	if _, _, err := st.SetMemberTeamDriveWrite(ctx, d.ID, false); !errors.Is(err, store.ErrMemberDriveBusy) {
		t.Fatalf("a deleting drive: err=%v, want ErrMemberDriveBusy", err)
	}
	if got, err := st.GetMemberTeamDrive(ctx, d.ID); err != nil || !got.MembersCanWrite {
		t.Fatalf("a busy refusal changed the bit: write=%v err=%v", got.MembersCanWrite, err)
	}
	plain, err := st.CreateMemberDrive(ctx, memberDrive("bob", "plain"), 5)
	if err != nil {
		t.Fatalf("plain: %v", err)
	}
	if _, _, err := st.SetMemberTeamDriveWrite(ctx, plain.ID, true); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a plain member drive: err=%v, want ErrNotFound", err)
	}
}

// TestMemberTeamDrive_UnitDeleteSetsNull_PG: deleting the unit leaves the drive with no unit, so no one else
// reaches it, and its owner still does.
func TestMemberTeamDrive_UnitDeleteSetsNull_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	unit := teamDriveUnit(t, pool, "platform")
	d, err := st.CreateMemberTeamDrive(ctx, memberDrive("alice", "shared"), unit, 5)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, _, err := st.SetMemberTeamDriveWrite(ctx, d.ID, true); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM governance_units WHERE id = $1`, unit); err != nil {
		t.Fatalf("delete unit: %v", err)
	}
	got, err := st.GetMemberTeamDrive(ctx, d.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UnitID != nil {
		t.Fatalf("unit after its delete = %v, want nil", got.UnitID)
	}
	if rows, err := st.ListReachableMemberTeamDrives(ctx, "bob", []uuid.UUID{unit}); err != nil || len(rows) != 0 {
		t.Fatalf("bob reached %d rows (err %v) of a deleted unit, want 0", len(rows), err)
	}
	if rows, err := st.ListReachableMemberTeamDrives(ctx, "alice", nil); err != nil || len(rows) != 1 {
		t.Fatalf("the owner reached %d rows (err %v), want 1", len(rows), err)
	}
}

// TestMemberTeamDrive_ShapeRefusesK8sPVC_PG: a person's own team drive is a Docker volume only, and the team
// columns stay off an admin row and a plain member drive.
func TestMemberTeamDrive_ShapeRefusesK8sPVC_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	unit := teamDriveUnit(t, pool, "platform")

	pvc := memberDrive("alice", "claim")
	pvc.Backend, pvc.SizeMiB = types.DriveBackendK8sPVC, 1024
	if _, err := st.CreateMemberTeamDrive(ctx, pvc, unit, 5); err == nil {
		t.Fatal("a k8s_pvc member drive became a team drive")
	}
	if rows, err := st.ListMemberDrivesOf(ctx, "alice"); err != nil || len(rows) != 0 {
		t.Fatalf("the refused create left %d rows (err %v), want 0", len(rows), err)
	}
	plain, err := st.CreateMemberDrive(ctx, memberDrive("bob", "plain"), 5)
	if err != nil {
		t.Fatalf("plain: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE user_drives SET team_unit_id = $2 WHERE id = $1`, plain.ID, unit); err == nil {
		t.Fatal("a plain member drive took a team unit")
	}
	if _, err := pool.Exec(ctx, `UPDATE user_drives SET team_members_can_write = true WHERE id = $1`, plain.ID); err == nil {
		t.Fatal("a plain member drive took the team write bit")
	}
}
