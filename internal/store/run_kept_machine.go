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
)

// RunKeptMachineStore keeps which kept machine a macOS VM run kept or resumed (agent_runs.kept_machine_id
// and kept_machine_mode). Dispatch sets it when it inserts or claims the machine; the run read finds the
// machine by it, and says the machine was deleted when its row is gone.
type RunKeptMachineStore interface {
	// SetRunKeptMachine records machine id and mode ("new", "resume" or "fresh") on run runID; a nil id
	// clears both. ErrNotFound when the run is gone.
	SetRunKeptMachine(ctx context.Context, runID uuid.UUID, id *uuid.UUID, mode string) error
	// RunKeptMachine returns the machine id and mode run runID records, a nil id for none, or ErrNotFound.
	RunKeptMachine(ctx context.Context, runID uuid.UUID) (*uuid.UUID, string, error)
	// ResetKeptMachineBase records the base a fresh start of machine id clones, while run runID holds it.
	// ErrNotFound when that run does not hold the machine.
	ResetKeptMachineBase(ctx context.Context, id, runID uuid.UUID, base string, preparedAt time.Time) error
}

var _ RunKeptMachineStore = PG{}

// SetRunKeptMachine — see RunKeptMachineStore.
func (s PG) SetRunKeptMachine(ctx context.Context, runID uuid.UUID, id *uuid.UUID, mode string) error {
	var m *string
	if id != nil {
		m = &mode
	}
	return s.execRun(ctx, "set run kept machine", `UPDATE agent_runs SET kept_machine_id = $2, kept_machine_mode = $3 WHERE id = $1`, runID, id, m)
}

// RunKeptMachine — see RunKeptMachineStore.
func (s PG) RunKeptMachine(ctx context.Context, runID uuid.UUID) (*uuid.UUID, string, error) {
	var id *uuid.UUID
	var mode *string
	err := s.Pool.QueryRow(ctx, `SELECT kept_machine_id, kept_machine_mode FROM agent_runs WHERE id = $1`, runID).Scan(&id, &mode)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("store: read run kept machine: %w", err)
	}
	if id == nil || mode == nil {
		return nil, "", nil
	}
	return id, *mode, nil
}

// ResetKeptMachineBase — see RunKeptMachineStore.
func (s PG) ResetKeptMachineBase(ctx context.Context, id, runID uuid.UUID, base string, preparedAt time.Time) error {
	return keptMachineExec(ctx, s, "reset base", `UPDATE kept_machines SET base = $3, base_prepared_at = $4
		WHERE id = $1 AND run_id = $2 AND state = 'in_use'`, id, runID, base, preparedAt)
}
