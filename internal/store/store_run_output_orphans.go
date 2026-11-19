// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RunOutputOrphanStore finds and ages live chunks (0129) that have no run_outputs row. The chunk append
// writes the run's pending row with its first chunk, but a server without that left chunks with no row when
// a run's dispatch-time pending insert failed and its final row was never written; then no recovery
// selected them and no retention deleted them. Optional like RunOutputChunkStore: the api layer
// type-asserts.
type RunOutputOrphanStore interface {
	// DeleteEndedRunOutputChunks deletes the chunks of every terminal run that ended longer ago than age
	// (the database's clock), whatever row the run has, and reports how many chunks went. A final row never
	// has chunks beside it. An age that is not positive deletes nothing.
	DeleteEndedRunOutputChunks(ctx context.Context, age time.Duration) (int, error)
	// ListOrphanRunOutputChunks returns up to limit terminal runs that hold chunks and have no run_outputs
	// row, in run id order. When age is positive a run that ended longer ago than age is left out: its
	// chunks are DeleteEndedRunOutputChunks', and nothing is recovered from expired output.
	ListOrphanRunOutputChunks(ctx context.Context, age time.Duration, limit int) ([]uuid.UUID, error)
}

var _ RunOutputOrphanStore = PG{}

// DeleteEndedRunOutputChunks — see RunOutputOrphanStore.
func (s PG) DeleteEndedRunOutputChunks(ctx context.Context, age time.Duration) (int, error) {
	if age <= 0 {
		return 0, nil
	}
	// Seconds, not age.String(): Go prints sub-millisecond ages in units Postgres intervals do not parse.
	tag, err := s.Pool.Exec(ctx, `
		DELETE FROM run_output_chunks c USING agent_runs r
		WHERE r.id = c.run_id AND r.state NOT IN (`+nonTerminalRunStates+`)
			AND r.ended_at < now() - make_interval(secs => $1)`, age.Seconds())
	if err != nil {
		return 0, fmt.Errorf("store: delete ended runs' output chunks: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ListOrphanRunOutputChunks — see RunOutputOrphanStore.
func (s PG) ListOrphanRunOutputChunks(ctx context.Context, age time.Duration, limit int) ([]uuid.UUID, error) {
	return collect(ctx, s.Pool, "list", "orphaned run output chunks", `
		SELECT DISTINCT c.run_id FROM run_output_chunks c JOIN agent_runs r ON r.id = c.run_id
		WHERE r.state NOT IN (`+nonTerminalRunStates+`)
			AND NOT EXISTS (SELECT 1 FROM run_outputs o WHERE o.run_id = c.run_id)
			AND ($1::double precision <= 0 OR r.ended_at IS NULL OR r.ended_at >= now() - make_interval(secs => $1))
		ORDER BY c.run_id LIMIT $2`, []any{age.Seconds(), max(1, limit)},
		func(row pgx.Row) (uuid.UUID, error) {
			var id uuid.UUID
			err := row.Scan(&id)
			return id, err
		})
}
