// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
)

// SweepTicks is the Postgres-backed sweephealth.Store: one sweep_ticks row per
// sweep, shared by every replica (migration 0120).
type SweepTicks struct{ pool *pgxpool.Pool }

// NewSweepTicks returns the shared tick record on pool.
func NewSweepTicks(pool *pgxpool.Pool) *SweepTicks { return &SweepTicks{pool: pool} }

var _ sweephealth.Store = (*SweepTicks)(nil)

// RecordTick writes an attempt, or a success, for sweep. Both columns move
// with GREATEST, so a write older than the stored time (a replica with a slow
// clock, or a delayed write) leaves it where it was. A success also counts as
// an attempt, so a row whose attempt write was lost still has both. replica
// follows the newest attempt.
func (s *SweepTicks) RecordTick(ctx context.Context, sweep, replica string, success bool, at time.Time) error {
	var succeeded *time.Time
	if success {
		succeeded = &at
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sweep_ticks AS t (sweep, attempted_at, succeeded_at, replica)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (sweep) DO UPDATE SET
			replica      = CASE WHEN EXCLUDED.attempted_at >= t.attempted_at THEN EXCLUDED.replica ELSE t.replica END,
			attempted_at = GREATEST(t.attempted_at, EXCLUDED.attempted_at),
			succeeded_at = GREATEST(t.succeeded_at, EXCLUDED.succeeded_at)`,
		sweep, at, succeeded, replica)
	if err != nil {
		return fmt.Errorf("db: record sweep tick %q: %w", sweep, err)
	}
	return nil
}

// Ticks reads every sweep's record.
func (s *SweepTicks) Ticks(ctx context.Context) (map[string]sweephealth.Tick, error) {
	rows, err := s.pool.Query(ctx, `SELECT sweep, attempted_at, succeeded_at, replica FROM sweep_ticks`)
	if err != nil {
		return nil, fmt.Errorf("db: read sweep ticks: %w", err)
	}
	defer rows.Close()
	out := map[string]sweephealth.Tick{}
	for rows.Next() {
		var name string
		var tk sweephealth.Tick
		var succeeded *time.Time
		if err := rows.Scan(&name, &tk.AttemptedAt, &succeeded, &tk.Replica); err != nil {
			return nil, fmt.Errorf("db: scan sweep tick: %w", err)
		}
		if succeeded != nil {
			tk.SucceededAt = *succeeded
		}
		out[name] = tk
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: read sweep ticks: %w", err)
	}
	return out, nil
}
