// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunnerDrainStore keeps administrators' drains of registered runners (the runner_drains table).
type RunnerDrainStore interface {
	// DrainRunner sets runnerID's drain. set is false when it was already drained; the stored drain is
	// returned unchanged then.
	DrainRunner(ctx context.Context, runnerID uuid.UUID, d types.RunnerDrain) (stored types.RunnerDrain, set bool, err error)
	// UndrainRunner deletes the drain and returns it; cleared is false when there was none.
	UndrainRunner(ctx context.Context, runnerID uuid.UUID) (was types.RunnerDrain, cleared bool, err error)
	// RunnerDrains is the drains of ids, keyed by runner; a runner with none is absent.
	RunnerDrains(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]types.RunnerDrain, error)
}

var _ RunnerDrainStore = PG{}

// DrainRunner drains a claimed runner. ErrNotFound: the runner is not claimed (or not there).
func (s PG) DrainRunner(ctx context.Context, runnerID uuid.UUID, d types.RunnerDrain) (types.RunnerDrain, bool, error) {
	var stored types.RunnerDrain
	err := s.Pool.QueryRow(ctx, `INSERT INTO runner_drains (runner_id, created_by, created_at, reason)
SELECT $1, $2, $3, $4 FROM runners WHERE id = $1 AND state = 'claimed'
ON CONFLICT (runner_id) DO NOTHING RETURNING created_by, created_at, reason`,
		runnerID, d.CreatedBy, d.CreatedAt, d.Reason).Scan(&stored.CreatedBy, &stored.CreatedAt, &stored.Reason)
	if err == nil {
		return stored, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return types.RunnerDrain{}, false, fmt.Errorf("drain runner: %w", err)
	}
	err = s.Pool.QueryRow(ctx, `SELECT created_by, created_at, reason FROM runner_drains WHERE runner_id = $1`,
		runnerID).Scan(&stored.CreatedBy, &stored.CreatedAt, &stored.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunnerDrain{}, false, ErrNotFound
	}
	if err != nil {
		return types.RunnerDrain{}, false, fmt.Errorf("read runner drain: %w", err)
	}
	return stored, false, nil
}

// UndrainRunner deletes runnerID's drain.
func (s PG) UndrainRunner(ctx context.Context, runnerID uuid.UUID) (types.RunnerDrain, bool, error) {
	var was types.RunnerDrain
	err := s.Pool.QueryRow(ctx, `DELETE FROM runner_drains WHERE runner_id = $1 RETURNING created_by, created_at, reason`,
		runnerID).Scan(&was.CreatedBy, &was.CreatedAt, &was.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunnerDrain{}, false, nil
	}
	if err != nil {
		return types.RunnerDrain{}, false, fmt.Errorf("undrain runner: %w", err)
	}
	return was, true, nil
}

// RunnerDrains is the drains of ids, without a lock; a runner with none is absent.
func (s PG) RunnerDrains(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]types.RunnerDrain, error) {
	rows, err := s.Pool.Query(ctx, `SELECT runner_id, created_by, created_at, reason FROM runner_drains WHERE runner_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("list runner drains: %w", err)
	}
	defer rows.Close()
	out := make(map[uuid.UUID]types.RunnerDrain, len(ids))
	for rows.Next() {
		var id uuid.UUID
		var d types.RunnerDrain
		if err := rows.Scan(&id, &d.CreatedBy, &d.CreatedAt, &d.Reason); err != nil {
			return nil, fmt.Errorf("scan runner drain: %w", err)
		}
		out[id] = d
	}
	return out, rows.Err()
}
