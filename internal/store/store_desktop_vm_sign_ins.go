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

// DesktopVMSignIns is a VM run's sign-in store (migration 0230): what its guest made of the store at
// boot, and who paused the agent to sign in, since when, while it is paused. PauseGeneration is the
// generation the run's latest pause or stop again took (migration 0231; 0 before its first), which
// each stop and continue carries to the guest so it drops one out of order.
type DesktopVMSignIns struct {
	Store           string // "memory" or "none"
	PausedBy        string
	PausedAt        *time.Time
	PauseGeneration int64
}

// DesktopVMSignInStore keeps it.
type DesktopVMSignInStore interface {
	// RecordDesktopVMSignIns writes the run's store at boot.
	RecordDesktopVMSignIns(ctx context.Context, runID uuid.UUID, store string) error
	// DesktopVMSignIns is the run's row; found is false for a run with none.
	DesktopVMSignIns(ctx context.Context, runID uuid.UUID) (row DesktopVMSignIns, found bool, err error)
	// NextDesktopVMPauseGeneration takes the run's next pause generation and answers it; the first is 1.
	NextDesktopVMPauseGeneration(ctx context.Context, runID uuid.UUID) (int64, error)
	// PauseDesktopVMAgent marks the agent paused by who at at, on a 'memory' run not already paused
	// whose generation is still gen; false when nothing changed.
	PauseDesktopVMAgent(ctx context.Context, runID uuid.UUID, by string, at time.Time, gen int64) (bool, error)
	// ResumeDesktopVMAgent clears the mark of the pause at pausedAt and answers what it was; found is
	// false when the agent was not marked paused at pausedAt with generation gen, so only one resume of
	// a pause sees it and none clears a newer pause's mark.
	ResumeDesktopVMAgent(ctx context.Context, runID uuid.UUID, pausedAt time.Time, gen int64) (by string, at time.Time, found bool, err error)
}

var _ DesktopVMSignInStore = PG{}

// RecordDesktopVMSignIns — see DesktopVMSignInStore.
func (s PG) RecordDesktopVMSignIns(ctx context.Context, runID uuid.UUID, store string) error {
	if _, err := s.Pool.Exec(ctx, `INSERT INTO desktop_vm_sign_ins (run_id, store) VALUES ($1, $2)
		ON CONFLICT (run_id) DO UPDATE SET store = EXCLUDED.store, paused_by = NULL, paused_at = NULL`, runID, store); err != nil {
		return fmt.Errorf("store: record desktop VM sign-ins: %w", err)
	}
	return nil
}

// DesktopVMSignIns — see DesktopVMSignInStore.
func (s PG) DesktopVMSignIns(ctx context.Context, runID uuid.UUID) (DesktopVMSignIns, bool, error) {
	var row DesktopVMSignIns
	var by *string
	err := s.Pool.QueryRow(ctx, `SELECT store, paused_by, paused_at, pause_generation FROM desktop_vm_sign_ins WHERE run_id = $1`, runID).
		Scan(&row.Store, &by, &row.PausedAt, &row.PauseGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return row, false, nil
	}
	if err != nil {
		return row, false, fmt.Errorf("store: desktop VM sign-ins: %w", err)
	}
	if by != nil {
		row.PausedBy = *by
	}
	return row, true, nil
}

// NextDesktopVMPauseGeneration — see DesktopVMSignInStore.
func (s PG) NextDesktopVMPauseGeneration(ctx context.Context, runID uuid.UUID) (int64, error) {
	var gen int64
	if err := s.Pool.QueryRow(ctx, `UPDATE desktop_vm_sign_ins SET pause_generation = pause_generation + 1
		WHERE run_id = $1 RETURNING pause_generation`, runID).Scan(&gen); err != nil {
		return 0, fmt.Errorf("store: next desktop VM pause generation: %w", err)
	}
	return gen, nil
}

// PauseDesktopVMAgent — see DesktopVMSignInStore.
func (s PG) PauseDesktopVMAgent(ctx context.Context, runID uuid.UUID, by string, at time.Time, gen int64) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE desktop_vm_sign_ins SET paused_by = $2, paused_at = $3
		WHERE run_id = $1 AND store = 'memory' AND paused_at IS NULL AND pause_generation = $4`, runID, by, at, gen)
	if err != nil {
		return false, fmt.Errorf("store: pause desktop VM agent: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ResumeDesktopVMAgent — see DesktopVMSignInStore.
func (s PG) ResumeDesktopVMAgent(ctx context.Context, runID uuid.UUID, pausedAt time.Time, gen int64) (string, time.Time, bool, error) {
	var by string
	var at time.Time
	err := s.Pool.QueryRow(ctx, `UPDATE desktop_vm_sign_ins d SET paused_by = NULL, paused_at = NULL
		FROM desktop_vm_sign_ins old WHERE d.run_id = $1 AND old.run_id = d.run_id AND d.paused_at = $2
		AND d.pause_generation = $3 RETURNING old.paused_by, old.paused_at`, runID, pausedAt, gen).Scan(&by, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("store: resume desktop VM agent: %w", err)
	}
	return by, at, true, nil
}
