// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MacDiskHoldStore is the live record of which run holds a person's disk drive (mac_disk_holds). Optional: a
// store without it (no PG) keeps the Mac's own lock as the only guard.
type MacDiskHoldStore interface {
	// HoldMacDisk records that run holds drive: inserted, kept when run already holds it, or taken over from a
	// run that is finished. A hold of another run that is not finished is left as it is, and ok is false.
	HoldMacDisk(ctx context.Context, drive, run uuid.UUID) (ok bool, err error)
	// MacDiskHolder is the run that holds drive and is not finished, or uuid.Nil.
	MacDiskHolder(ctx context.Context, drive uuid.UUID) (uuid.UUID, error)
}

var _ MacDiskHoldStore = PG{}

func (s PG) HoldMacDisk(ctx context.Context, drive, run uuid.UUID) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `INSERT INTO mac_disk_holds (drive_id, run_id) VALUES ($1, $2)
		ON CONFLICT (drive_id) DO UPDATE SET run_id = EXCLUDED.run_id, held_at = now()
		 WHERE mac_disk_holds.run_id = EXCLUDED.run_id
		    OR NOT EXISTS (SELECT 1 FROM agent_runs k WHERE k.id = mac_disk_holds.run_id AND k.state IN (`+nonTerminalRunStates+`))`,
		drive, run)
	if err != nil {
		return false, fmt.Errorf("store: hold mac disk: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s PG) MacDiskHolder(ctx context.Context, drive uuid.UUID) (uuid.UUID, error) {
	var run uuid.UUID
	err := s.Pool.QueryRow(ctx, `SELECT h.run_id FROM mac_disk_holds h JOIN agent_runs k ON k.id = h.run_id
		 WHERE h.drive_id = $1 AND k.state IN (`+nonTerminalRunStates+`)`, drive).Scan(&run)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, nil
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("store: mac disk holder: %w", err)
	}
	return run, nil
}
