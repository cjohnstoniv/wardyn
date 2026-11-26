// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// A person's own team drive (#3459): a member drive (owner_subject set) with team on, shared with the governance
// unit in team_unit_id and the units below it. The team facts are read by their own statements, so driveRowCols
// and every other drive read stay as they are.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// MemberTeamDrive is a person's own team drive with its unit and its write bit.
type MemberTeamDrive struct {
	Drive types.UserDrive
	// UnitID is the governance unit the drive belongs to; nil once that unit is deleted.
	UnitID *uuid.UUID
	// MembersCanWrite is a super admin's tick that lets the unit write.
	MembersCanWrite bool
}

// MemberTeamDriveStore keeps people's own team drives. It is not part of MemberDriveStore, so a store without it
// keeps no team drives.
type MemberTeamDriveStore interface {
	CreateMemberTeamDrive(ctx context.Context, d types.UserDrive, unit uuid.UUID, limit int) (types.UserDrive, error)
	ListReachableMemberTeamDrives(ctx context.Context, owner string, units []uuid.UUID) ([]MemberTeamDrive, error)
	GetMemberTeamDrive(ctx context.Context, id uuid.UUID) (MemberTeamDrive, error)
	AnyRunStarting(ctx context.Context, leaseFresh time.Duration) (bool, error)
	MemberTeamDriveFacts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]MemberTeamDrive, error)
	SetMemberTeamDriveWrite(ctx context.Context, id uuid.UUID, on bool) (before, after MemberTeamDrive, err error)
}

var _ MemberTeamDriveStore = PG{}

const memberTeamDriveCols = driveRowCols + ", team_unit_id, team_members_can_write"

func scanMemberTeamDrive(row pgx.Row) (MemberTeamDrive, error) {
	var t MemberTeamDrive
	err := row.Scan(append(driveRowDest(&t.Drive), &t.UnitID, &t.MembersCanWrite)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return MemberTeamDrive{}, ErrNotFound
	}
	if err != nil {
		return MemberTeamDrive{}, fmt.Errorf("store: scan member team drive: %w", err)
	}
	return t, nil
}

// CreateMemberTeamDrive is CreateMemberDrive for a team drive of unit: the same lock, count and insert, then the
// row is made a team drive of unit, in one READ COMMITTED transaction. A check violation is returned, never retried.
func (s PG) CreateMemberTeamDrive(ctx context.Context, d types.UserDrive, unit uuid.UUID, limit int) (types.UserDrive, error) {
	if d.OwnerSubject == "" {
		return types.UserDrive{}, errMemberDriveNoOwner
	}
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.UserDrive{}, fmt.Errorf("store: begin member team drive create: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path
	inserted, err := insertMemberDrive(ctx, tx, d, limit)
	if err != nil {
		return types.UserDrive{}, err
	}
	out, err := scanDriveRow(tx.QueryRow(ctx,
		`UPDATE user_drives SET team = true, team_unit_id = $2 WHERE id = $1 RETURNING `+driveRowCols, inserted.ID, unit))
	if err != nil {
		return types.UserDrive{}, fmt.Errorf("store: make member drive a team drive: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return types.UserDrive{}, fmt.Errorf("store: commit member team drive create: %w", err)
	}
	return out, nil
}

// ListReachableMemberTeamDrives returns the team drives of units and owner's own, by name, leaving out a drive
// being deleted.
func (s PG) ListReachableMemberTeamDrives(ctx context.Context, owner string, units []uuid.UUID) ([]MemberTeamDrive, error) {
	if owner == "" {
		return nil, errMemberDriveNoOwner
	}
	return collect(ctx, s.Pool, "list", "member team drives",
		`SELECT `+memberTeamDriveCols+` FROM user_drives WHERE team AND owner_subject <> '' AND (team_unit_id = ANY($2) OR owner_subject = $1)
		AND delete_started_at IS NULL AND delete_issues % 2 = 0 ORDER BY name, id`,
		[]any{owner, units}, scanMemberTeamDrive)
}

// GetMemberTeamDrive returns one person's own team drive, or ErrNotFound.
func (s PG) GetMemberTeamDrive(ctx context.Context, id uuid.UUID) (MemberTeamDrive, error) {
	return scanMemberTeamDrive(s.Pool.QueryRow(ctx,
		`SELECT `+memberTeamDriveCols+` FROM user_drives WHERE id = $1 AND team AND owner_subject <> ''`, id))
}

// AnyRunStarting is MemberDriveOwnerStarting for every person: a run in STARTING, or a run with no sandbox
// recorded whose watcher lease is held and younger than leaseFresh.
func (s PG) AnyRunStarting(ctx context.Context, leaseFresh time.Duration) (bool, error) {
	var starting bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_runs
		WHERE (state = 'STARTING' OR (sandbox_ref = '' AND watcher_owner <> '' AND watcher_heartbeat > now() - $1::interval)))`,
		max(leaseFresh, 0).String()).Scan(&starting)
	if err != nil {
		return false, fmt.Errorf("store: read the starting runs: %w", err)
	}
	return starting, nil
}

// MemberTeamDriveFacts returns the team facts of the people's own team drives among ids, by id.
func (s PG) MemberTeamDriveFacts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]MemberTeamDrive, error) {
	rows, err := collect(ctx, s.Pool, "read", "member team drives",
		`SELECT `+memberTeamDriveCols+` FROM user_drives WHERE id = ANY($1) AND team AND owner_subject <> ''`,
		[]any{ids}, scanMemberTeamDrive)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]MemberTeamDrive, len(rows))
	for _, t := range rows {
		out[t.Drive.ID] = t
	}
	return out, nil
}

// SetMemberTeamDriveWrite sets a person's own team drive's write bit and returns the row before and after. A drive
// being deleted returns ErrMemberDriveBusy and is not written.
func (s PG) SetMemberTeamDriveWrite(ctx context.Context, id uuid.UUID, on bool) (before, after MemberTeamDrive, err error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return before, after, fmt.Errorf("store: begin member team drive write: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path
	if before, err = scanMemberTeamDrive(tx.QueryRow(ctx,
		`SELECT `+memberTeamDriveCols+` FROM user_drives WHERE id = $1 AND team AND owner_subject <> '' FOR UPDATE`, id)); err != nil {
		return MemberTeamDrive{}, MemberTeamDrive{}, err
	}
	if before.Drive.Deleting() {
		return MemberTeamDrive{}, MemberTeamDrive{}, ErrMemberDriveBusy
	}
	if after, err = scanMemberTeamDrive(tx.QueryRow(ctx,
		`UPDATE user_drives SET team_members_can_write = $2, updated_at = now() WHERE id = $1 RETURNING `+memberTeamDriveCols,
		id, on)); err != nil {
		return MemberTeamDrive{}, MemberTeamDrive{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return MemberTeamDrive{}, MemberTeamDrive{}, fmt.Errorf("store: commit member team drive write: %w", err)
	}
	return before, after, nil
}
