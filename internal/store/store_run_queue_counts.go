// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// PoolCounts is how busy one runner pool is: runs holding one of its slots, and runs waiting for one.
// Counts only; no run, owner or executor is named.
type PoolCounts struct {
	Used    int
	Waiting int
}

// RunnerPoolSlotCounts is every listed pool's PoolCounts in one grouped read, taken without any lock:
// advisory, since a launch counts again under the pool row's lock. Used counts what countPoolSlotHolders
// counts. Waiting leaves out a scheduled run whose start time is still ahead: it is not in line yet.
// A pool with neither is present with zeros.
func (s PG) RunnerPoolSlotCounts(ctx context.Context, poolIDs []uuid.UUID) (map[uuid.UUID]PoolCounts, error) {
	out := make(map[uuid.UUID]PoolCounts, len(poolIDs))
	if len(poolIDs) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT p.id,
			(SELECT count(*) FROM agent_runs r WHERE r.runner_pool_id = p.id AND r.state = ANY($2) AND `+HoldsSandboxSQL+`),
			(SELECT count(*) FROM run_queue q WHERE q.pool_id = p.id AND (q.start_at IS NULL OR q.start_at <= now()))
		FROM unnest($1::uuid[]) AS p(id)`, poolIDs, slotHoldingStateNames())
	if err != nil {
		return nil, fmt.Errorf("store: runner pool slot counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var c PoolCounts
		if err := rows.Scan(&id, &c.Used, &c.Waiting); err != nil {
			return nil, fmt.Errorf("store: runner pool slot counts: %w", err)
		}
		out[id] = c
	}
	return out, rows.Err()
}

// RunQueueFact is a waiting run's place in its pool's queue: Ahead is the number of waiting runs
// before it in line (runQueueLine, then run_id), a count and never whose. StartAt is a scheduled
// run's not-before time and TimeZone the zone its request gave it in.
type RunQueueFact struct {
	PoolID     uuid.UUID
	QueuedAt   time.Time
	DeadlineAt time.Time
	Ahead      int
	StartAt    *time.Time
	TimeZone   string
}

// RunQueueFacts reads the place of each of ids that is still waiting, in one read; a run that is not
// QUEUED has no entry. Only the pools of the asked runs are ranked.
func (s PG) RunQueueFacts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]RunQueueFact, error) {
	out := make(map[uuid.UUID]RunQueueFact, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT run_id, pool_id, queued_at, deadline_at, ahead, start_at, time_zone FROM (
			SELECT run_id, pool_id, queued_at, deadline_at, start_at, COALESCE(launch->'request'->>'start_time_zone', '') AS time_zone,
				row_number() OVER (PARTITION BY pool_id ORDER BY `+runQueueLine+`, run_id) - 1 AS ahead
			FROM run_queue WHERE pool_id IN (SELECT pool_id FROM run_queue WHERE run_id = ANY($1))
		) ranked WHERE run_id = ANY($1)`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: run queue facts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var f RunQueueFact
		if err := rows.Scan(&id, &f.PoolID, &f.QueuedAt, &f.DeadlineAt, &f.Ahead, &f.StartAt, &f.TimeZone); err != nil {
			return nil, fmt.Errorf("store: run queue facts: %w", err)
		}
		out[id] = f
	}
	return out, rows.Err()
}
