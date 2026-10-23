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

// DesktopVMSignIns is a VM run's sign-in store (migration 0229): what its guest made of the store at
// boot, and who paused the agent to sign in, since when, while it is paused.
type DesktopVMSignIns struct {
	Store    string // "memory" or "none"
	PausedBy string
	PausedAt *time.Time
}

// DesktopVMSignInStore keeps it.
type DesktopVMSignInStore interface {
	// RecordDesktopVMSignIns writes the run's store at boot.
	RecordDesktopVMSignIns(ctx context.Context, runID uuid.UUID, store string) error
	// DesktopVMSignIns is the run's row; found is false for a run with none.
	DesktopVMSignIns(ctx context.Context, runID uuid.UUID) (row DesktopVMSignIns, found bool, err error)
	// PauseDesktopVMAgent marks the agent paused by who at at, on a 'memory' run not already paused;
	// false when nothing changed.
	PauseDesktopVMAgent(ctx context.Context, runID uuid.UUID, by string, at time.Time) (bool, error)
	// ResumeDesktopVMAgent clears the mark and answers what it was; found is false when the agent
	// was not marked paused, so only one resume of a pause sees it.
	ResumeDesktopVMAgent(ctx context.Context, runID uuid.UUID) (by string, at time.Time, found bool, err error)
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
	err := s.Pool.QueryRow(ctx, `SELECT store, paused_by, paused_at FROM desktop_vm_sign_ins WHERE run_id = $1`, runID).
		Scan(&row.Store, &by, &row.PausedAt)
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

// PauseDesktopVMAgent — see DesktopVMSignInStore.
func (s PG) PauseDesktopVMAgent(ctx context.Context, runID uuid.UUID, by string, at time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE desktop_vm_sign_ins SET paused_by = $2, paused_at = $3
		WHERE run_id = $1 AND store = 'memory' AND paused_at IS NULL`, runID, by, at)
	if err != nil {
		return false, fmt.Errorf("store: pause desktop VM agent: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ResumeDesktopVMAgent — see DesktopVMSignInStore.
func (s PG) ResumeDesktopVMAgent(ctx context.Context, runID uuid.UUID) (string, time.Time, bool, error) {
	var by string
	var at time.Time
	err := s.Pool.QueryRow(ctx, `UPDATE desktop_vm_sign_ins d SET paused_by = NULL, paused_at = NULL
		FROM desktop_vm_sign_ins old WHERE d.run_id = $1 AND old.run_id = d.run_id AND d.paused_at IS NOT NULL
		RETURNING old.paused_by, old.paused_at`, runID).Scan(&by, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, fmt.Errorf("store: resume desktop VM agent: %w", err)
	}
	return by, at, true, nil
}
