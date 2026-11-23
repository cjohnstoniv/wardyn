// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// A run's recorded sandbox spec (run_sandbox_specs): the agent half dispatch created the run's sandbox with,
// sealed by the server, so a move can create the sandbox again at a destination.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RunSandboxSpecs stores each run's sealed sandbox spec. The caller seals and opens it; the store never sees it in
// the clear. Optional for RunLeaser's reason: the api layer type-asserts, and production is always PG.
type RunSandboxSpecs interface {
	// PutRunSandboxSpec writes run id's sealed spec once. A second write for the run is an error: a run is
	// dispatched once, and its record never changes.
	PutRunSandboxSpec(ctx context.Context, id uuid.UUID, sealed []byte) error
	// GetRunSandboxSpec reads it; ErrNotFound when there is none.
	GetRunSandboxSpec(ctx context.Context, id uuid.UUID) ([]byte, error)
}

var _ RunSandboxSpecs = PG{}

// PutRunSandboxSpec — see RunSandboxSpecs.
func (s PG) PutRunSandboxSpec(ctx context.Context, id uuid.UUID, sealed []byte) error {
	if _, err := s.Pool.Exec(ctx, `INSERT INTO run_sandbox_specs (run_id, sealed) VALUES ($1, $2)`, id, sealed); err != nil {
		return fmt.Errorf("store: put run sandbox spec: %w", err)
	}
	return nil
}

// GetRunSandboxSpec — see RunSandboxSpecs.
func (s PG) GetRunSandboxSpec(ctx context.Context, id uuid.UUID) ([]byte, error) {
	var sealed []byte
	err := s.Pool.QueryRow(ctx, `SELECT sealed FROM run_sandbox_specs WHERE run_id = $1`, id).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("store: get run sandbox spec: %w", err)
	}
	return sealed, nil
}
