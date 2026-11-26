// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The reclaim fence: drive_reclaims holds one row per storage object a super admin's reclaim has claimed, keyed by
// the substrate's own name for it, and no run attaches an object while its row exists. The claim, issue and settle
// rules are a member drive's delete's (member_drives.go): an odd issue count says a call may still be outstanding.
// drive_object_holds records each run dispatched with an object a reclaim could destroy; a hold counts while its run
// has not ended.
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

// ErrDriveReclaimBusy is returned by BeginDriveReclaim when another reclaim of the object started under a minute ago.
var ErrDriveReclaimBusy = fmt.Errorf("%w: a reclaim of this object is already in flight", ErrConflict)

// DriveReclaimFenceStore is the reclaim fence (drive_reclaims) and the runs that hold an object (drive_object_holds).
// Optional: a store without it reclaims nothing (handleReclaimUserDrive refuses), and its runs attach unfenced.
type DriveReclaimFenceStore interface {
	// BeginDriveReclaim claims object unless a claim younger than a minute holds it (ErrDriveReclaimBusy). A claim taken
	// over keeps its issue count. It returns the claim's pair.
	BeginDriveReclaim(ctx context.Context, backend types.DriveBackend, object string, drive uuid.UUID) (started time.Time, issues int, err error)
	// IssueDriveReclaim records, before the call, that one is about to be made under the claim (started, n): the count
	// becomes odd and is returned. A pair that no longer matches is ErrNotFound and no call may be made.
	IssueDriveReclaim(ctx context.Context, backend types.DriveBackend, object string, started time.Time, n int) (int, error)
	// ClearDriveReclaim removes a claim no call was made under, only over an even count.
	ClearDriveReclaim(ctx context.Context, backend types.DriveBackend, object string, started time.Time, n int) error
	// ClearDriveReclaimIssued removes a claim whose one call was refused before it changed anything, only when the claim
	// was taken over an even count.
	ClearDriveReclaimIssued(ctx context.Context, backend types.DriveBackend, object string, started time.Time, claimN, issued int) error
	// FinishDriveReclaim removes the claim once the storage confirmed the object gone.
	FinishDriveReclaim(ctx context.Context, backend types.DriveBackend, object string, started time.Time, n int) error
	// DriveReclaimFenced reports whether any claim, fresh, stale or with a call outstanding, holds object.
	DriveReclaimFenced(ctx context.Context, backend types.DriveBackend, object string) (bool, error)
	// HoldDriveObject records that run was dispatched with object. Idempotent.
	HoldDriveObject(ctx context.Context, backend types.DriveBackend, object string, run uuid.UUID) error
	// DriveObjectHeld reports whether a run that has not ended holds object.
	DriveObjectHeld(ctx context.Context, backend types.DriveBackend, object string) (bool, error)
}

var _ DriveReclaimFenceStore = PG{}

// BeginDriveReclaim implements DriveReclaimFenceStore.
func (s PG) BeginDriveReclaim(ctx context.Context, backend types.DriveBackend, object string, drive uuid.UUID) (time.Time, int, error) {
	var started time.Time
	var issues int
	err := s.Pool.QueryRow(ctx, `INSERT INTO drive_reclaims (backend, object_name, drive_id, started_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (backend, object_name) DO UPDATE SET started_at = now(), drive_id = EXCLUDED.drive_id
		WHERE drive_reclaims.started_at < now() - interval '`+memberDriveTakeover+`' RETURNING started_at, issues`,
		string(backend), object, drive).Scan(&started, &issues)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, 0, ErrDriveReclaimBusy
	}
	if err != nil {
		return time.Time{}, 0, fmt.Errorf("store: begin drive reclaim: %w", err)
	}
	return started, issues, nil
}

// IssueDriveReclaim implements DriveReclaimFenceStore.
func (s PG) IssueDriveReclaim(ctx context.Context, backend types.DriveBackend, object string, started time.Time, n int) (int, error) {
	var issued int
	err := s.Pool.QueryRow(ctx, `UPDATE drive_reclaims SET issues = issues + 1 + (issues % 2)
		WHERE backend = $1 AND object_name = $2 AND started_at = $3 AND issues = $4 RETURNING issues`,
		string(backend), object, started, n).Scan(&issued)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("store: issue drive reclaim: %w", err)
	}
	return issued, nil
}

// ClearDriveReclaim implements DriveReclaimFenceStore.
func (s PG) ClearDriveReclaim(ctx context.Context, backend types.DriveBackend, object string, started time.Time, n int) error {
	return driveReclaimExec(ctx, s, "clear",
		`DELETE FROM drive_reclaims WHERE backend = $1 AND object_name = $2 AND started_at = $3 AND issues = $4 AND issues % 2 = 0`,
		string(backend), object, started, n)
}

// ClearDriveReclaimIssued implements DriveReclaimFenceStore.
func (s PG) ClearDriveReclaimIssued(ctx context.Context, backend types.DriveBackend, object string, started time.Time, claimN, issued int) error {
	return driveReclaimExec(ctx, s, "clear",
		`DELETE FROM drive_reclaims WHERE backend = $1 AND object_name = $2 AND started_at = $3 AND issues = $5
		 AND $4::int % 2 = 0 AND $5::int = $4::int + 1`,
		string(backend), object, started, claimN, issued)
}

// FinishDriveReclaim implements DriveReclaimFenceStore.
func (s PG) FinishDriveReclaim(ctx context.Context, backend types.DriveBackend, object string, started time.Time, n int) error {
	return driveReclaimExec(ctx, s, "finish",
		`DELETE FROM drive_reclaims WHERE backend = $1 AND object_name = $2 AND started_at = $3 AND issues = $4`,
		string(backend), object, started, n)
}

// DriveReclaimFenced implements DriveReclaimFenceStore.
func (s PG) DriveReclaimFenced(ctx context.Context, backend types.DriveBackend, object string) (bool, error) {
	var fenced bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM drive_reclaims WHERE backend = $1 AND object_name = $2)`,
		string(backend), object).Scan(&fenced); err != nil {
		return false, fmt.Errorf("store: read drive reclaim: %w", err)
	}
	return fenced, nil
}

// HoldDriveObject implements DriveReclaimFenceStore.
func (s PG) HoldDriveObject(ctx context.Context, backend types.DriveBackend, object string, run uuid.UUID) error {
	if _, err := s.Pool.Exec(ctx, `INSERT INTO drive_object_holds (backend, object_name, run_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		string(backend), object, run); err != nil {
		return fmt.Errorf("store: hold drive reclaim: %w", err)
	}
	return nil
}

// DriveObjectHeld implements DriveReclaimFenceStore.
func (s PG) DriveObjectHeld(ctx context.Context, backend types.DriveBackend, object string) (bool, error) {
	var held bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM drive_object_holds h JOIN agent_runs k ON k.id = h.run_id
		WHERE h.backend = $1 AND h.object_name = $2 AND k.state IN (`+nonTerminalRunStates+`))`,
		string(backend), object).Scan(&held); err != nil {
		return false, fmt.Errorf("store: read held drive reclaim: %w", err)
	}
	return held, nil
}

// driveReclaimExec runs one settle statement: 0 rows affected is ErrNotFound (the claim's pair no longer matches).
func driveReclaimExec(ctx context.Context, s PG, verb, q string, args ...any) error {
	tag, err := s.Pool.Exec(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("store: %s drive reclaim: %w", verb, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
