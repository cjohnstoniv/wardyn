// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

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

// MacOSVMDisk is a macOS VM run's disk report (migration 0236): what its runner says became of the run's
// directory on the Mac. State is "removed" or "kept"; Reason, only on kept, is a closed code or "".
type MacOSVMDisk struct {
	State  string
	Reason string
	At     time.Time
}

// MacOSVMDiskStore keeps them.
type MacOSVMDiskStore interface {
	// RecordMacOSVMDisk writes runnerID's report on runID. It is kept only for a macOS VM run placed on
	// that runner, and never over a removed one: a sweep's later removal turns kept into removed, and
	// nothing turns removed back.
	RecordMacOSVMDisk(ctx context.Context, runnerID, runID uuid.UUID, d MacOSVMDisk) error
	// MacOSVMDisk is the run's report; found is false for a run with none.
	MacOSVMDisk(ctx context.Context, runID uuid.UUID) (d MacOSVMDisk, found bool, err error)
}

var _ MacOSVMDiskStore = PG{}

// RecordMacOSVMDisk — see MacOSVMDiskStore.
func (s PG) RecordMacOSVMDisk(ctx context.Context, runnerID, runID uuid.UUID, d MacOSVMDisk) error {
	if _, err := s.Pool.Exec(ctx, `INSERT INTO macos_vm_disks (run_id, state, reason, at)
		SELECT id, $3::text, $4::text, $5::timestamptz FROM agent_runs WHERE id = $2 AND runner_id = $1 AND machine_kind = $6
		ON CONFLICT (run_id) DO UPDATE SET state = EXCLUDED.state, reason = EXCLUDED.reason, at = EXCLUDED.at
		WHERE macos_vm_disks.state <> 'removed'`, runnerID, runID, d.State, d.Reason, d.At, string(types.MachineMacOSVM)); err != nil {
		return fmt.Errorf("store: record a macOS VM disk report: %w", err)
	}
	return nil
}

// MacOSVMDisk — see MacOSVMDiskStore.
func (s PG) MacOSVMDisk(ctx context.Context, runID uuid.UUID) (MacOSVMDisk, bool, error) {
	var d MacOSVMDisk
	err := s.Pool.QueryRow(ctx, `SELECT state, reason, at FROM macos_vm_disks WHERE run_id = $1`, runID).Scan(&d.State, &d.Reason, &d.At)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, false, nil
	}
	if err != nil {
		return d, false, fmt.Errorf("store: macOS VM disk report: %w", err)
	}
	return d, true, nil
}
