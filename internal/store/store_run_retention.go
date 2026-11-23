// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// retentionTerminalStates is the states a retention sweep may delete. POSITIVE, as NonTerminalRunStates is: a
// state added later and forgotten here is kept, never deleted.
var retentionTerminalStates = []string{
	string(types.RunCompleted), string(types.RunFailed), string(types.RunKilled),
	string(types.RunStopped), string(types.RunArchived),
}

// runRetentionHeldSQL is TRUE for a run something still holds. The first rows refuse the run's delete
// (ON DELETE RESTRICT or no action: 0182, 0183, 0288, 0085 through the run's approvals, and 0155's two
// self-references on runner_pending_actions, where another run's action, or an asserted subject's with no
// run, supersedes or settles one of this run's); the last name the run as live state (0250 kept machines and
// 0253's unconfirmed fresh start, 0241 workloads, 0286 teardown holds, and 0189's containment pending: pods
// deleted with no proof that the processes stopped).
const runRetentionHeldSQL = `(
	EXISTS (SELECT 1 FROM runner_output_watermarks x WHERE x.run_id = r.id) OR
	EXISTS (SELECT 1 FROM runner_retirements x WHERE x.run_id = r.id) OR
	EXISTS (SELECT 1 FROM runner_resident_runs x WHERE x.run_id = r.id) OR
	EXISTS (SELECT 1 FROM mcp_call_sends x WHERE x.run_id = r.id) OR
	EXISTS (SELECT 1 FROM approvals a JOIN push_content_paths p ON p.approval_id = a.id WHERE a.run_id = r.id) OR
	EXISTS (SELECT 1 FROM approvals a JOIN mcp_call_sends m ON m.approval_id = a.id WHERE a.run_id = r.id) OR
	EXISTS (SELECT 1 FROM runner_pending_actions a JOIN runner_pending_actions b ON b.supersedes = a.id OR b.settled_by = a.id
		WHERE a.run_id = r.id AND b.run_id IS DISTINCT FROM r.id) OR
	EXISTS (SELECT 1 FROM kept_machines x WHERE x.run_id = r.id) OR
	EXISTS (SELECT 1 FROM kept_machines x WHERE x.reset_run_id = r.id) OR
	EXISTS (SELECT 1 FROM workloads x WHERE x.current_run_id = r.id) OR
	EXISTS (SELECT 1 FROM run_teardown_holds x WHERE x.run_id = r.id) OR
	EXISTS (SELECT 1 FROM run_containment_pending x WHERE x.run_id = r.id))`

// runRetentionStatementTimeout bounds one batch statement, as the recording sweep bounds its delete: a batch
// that cannot finish in it is cancelled, and the next tick tries again.
var runRetentionStatementTimeout = "5min"

// DeleteExpiredRuns deletes up to limit ended runs whose end time is older than olderThan, with every row that
// cascades from them, and records a run_history_tombstones row for each in the same statement. A run held by
// runRetentionHeldSQL is kept, and so is one whose sandbox was never confirmed gone (sandbox_ref set): the
// sandbox sweeps find it through its row. The statement runs under runRetentionStatementTimeout. It returns how many runs
// it deleted.
func (s PG) DeleteExpiredRuns(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	q := `WITH doomed AS (
		SELECT r.id FROM agent_runs r
		WHERE r.state = ANY($1) AND r.sandbox_ref = '' AND ` + runEndTimeR + ` < now() - make_interval(secs => $2::bigint)
		  AND NOT ` + runRetentionHeldSQL + `
		ORDER BY r.id LIMIT $3
		FOR UPDATE OF r SKIP LOCKED
	), gone AS (
		DELETE FROM agent_runs r USING doomed d WHERE r.id = d.id
		RETURNING r.id, r.unit_id, r.created_at, ` + runEndTimeR + ` AS ended_at
	)
	INSERT INTO run_history_tombstones (run_id, unit_id, created_at, ended_at)
	SELECT id, unit_id, created_at, ended_at FROM gone`
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, fmt.Errorf("store: delete expired runs: %w", err)
	}
	defer db.Rollback(ctx, tx) // no-op after commit
	if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`, runRetentionStatementTimeout); err != nil {
		return 0, fmt.Errorf("store: delete expired runs: %w", err)
	}
	tag, err := tx.Exec(ctx, q, retentionTerminalStates, int64(olderThan.Seconds()), limit)
	if err != nil {
		return 0, fmt.Errorf("store: delete expired runs: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: delete expired runs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// runEndTimeR is runEndTimeSQL over the alias r.
const runEndTimeR = `CASE WHEN r.lost_reason = 'ended' THEN r.lost_at ELSE r.ended_at END`
