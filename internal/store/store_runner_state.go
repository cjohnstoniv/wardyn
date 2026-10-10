// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrRunnerReconciliationRequired refuses READY without issuing an RPC from
// the checked STATE handler. H3 persists returned containment actions first.
var ErrRunnerReconciliationRequired = errors.New("runner lifecycle reconciliation required")

// RunnerContainment identifies the exact kept incarnation requiring H3's
// durable stop_proxy queue. Only an exact action_result may settle that action.
type RunnerContainment struct {
	RunnerID, RunID, Generation uuid.UUID
	Ref                         string
	LostAt                      time.Time
}

const runnerStateLimit = 1000

// ReconcileRunnerState validates runner assertions against org authority. It
// does not renew credentials, bind handles, clear lost marks or finalize runs.
// Transactions are short; no run-operation lock or remote RPC is taken here.
func (s PG) ReconcileRunnerState(ctx context.Context, runnerID uuid.UUID, rows []runnerwire.RunState) ([]RunnerContainment, error) {
	if len(rows) > runnerStateLimit {
		return nil, ErrRunnerExecutionRefused
	}
	seen := make(map[uuid.UUID]bool, len(rows))
	var requests []RunnerContainment
	needsLoss := false
	for _, row := range rows {
		if row.RunID == uuid.Nil || seen[row.RunID] || row.SpoolDepth < 0 || row.SpoolDrops < 0 {
			return nil, ErrRunnerExecutionRefused
		}
		seen[row.RunID] = true
		request, lapsed, err := s.reconcileRunnerStateRow(ctx, runnerID, row)
		if err != nil {
			return nil, err
		}
		needsLoss = needsLoss || lapsed
		if request != nil {
			requests = append(requests, *request)
		}
	}
	// An omitted not-kept live run cannot silently become ready. Kept runs
	// remain fenced by lifecycle and H3 pending actions, not STATE completeness. The bounded query also
	// validates identity for an empty STATE, rather than trusting its caller.
	ids, err := s.runnerStateIDs(ctx, runnerID)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if !seen[id] {
			needsLoss = true
		}
	}
	if needsLoss || len(requests) > 0 {
		return requests, ErrRunnerReconciliationRequired
	}
	return nil, nil
}

func (s PG) reconcileRunnerStateRow(ctx context.Context, runnerID uuid.UUID, row runnerwire.RunState) (*RunnerContainment, bool, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed tx is already closed
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout = '2s'`); err != nil {
		return nil, false, err
	}
	run, err := runnerExecutionRun(ctx, tx, runnerID, row.RunID)
	if err != nil {
		return nil, false, err
	}
	current, err := readRunnerExecution(ctx, tx, row.RunID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	if row.Execution != nil && (row.Execution.Generation != current.Generation || row.Execution.Operation != current.Operation || !row.Execution.Attempted || (current.ExecID != "" && current.ExecID != row.Execution.ExecID)) {
		return nil, false, ErrRunnerExecutionRefused
	}
	var lapsed bool
	if err = tx.QueryRow(ctx, `SELECT token_renewed_at < clock_timestamp()-$2::interval FROM agent_runs WHERE id=$1`, row.RunID, identity.RunTokenLapseAfter.String()).Scan(&lapsed); err != nil {
		return nil, false, err
	}
	var request *RunnerContainment
	if run.State == types.RunRunning && run.LostAt != nil && run.LostReason == types.LostOutage && row.SandboxPresent && row.ProxyStoppedAt == nil {
		request = &RunnerContainment{RunnerID: runnerID, RunID: run.ID, Generation: current.Generation, Ref: run.SandboxRef, LostAt: *run.LostAt}
	}
	// A timestamp is an assertion, never an ACK: H3's exact action reconciliation
	// alone clears pending_runner. Live missing sandboxes also require recovery.
	needsLoss := run.State == types.RunRunning && run.LostAt == nil && (lapsed || !row.SandboxPresent)
	return request, needsLoss, tx.Commit(ctx)
}

func (s PG) runnerStateIDs(ctx context.Context, runnerID uuid.UUID) ([]uuid.UUID, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed tx is already closed
	var valid bool
	if err = tx.QueryRow(ctx, `SELECT state='claimed' AND revoked_at IS NULL AND owner<>'' FROM runners WHERE id=$1 FOR SHARE`, runnerID).Scan(&valid); err != nil {
		return nil, err
	}
	if !valid {
		return nil, ErrRunnerExecutionRefused
	}
	rows, err := tx.Query(ctx, `SELECT a.id FROM agent_runs a JOIN runners r ON r.id=a.runner_id WHERE r.id=$1 AND a.created_by=r.owner AND a.placement='local' AND a.state='RUNNING' AND a.lost_at IS NULL AND a.sandbox_ref<>'' ORDER BY a.id LIMIT 1001`, runnerID)
	if err != nil {
		return nil, err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, err
	}
	if len(ids) > runnerStateLimit {
		return nil, ErrRunnerExecutionRefused
	}
	return ids, tx.Commit(ctx)
}

// ListRunnerLapsedRuns is a bounded page for the offline callback. The loss
// claim rechecks all authority and DB lapse in MarkRunnerRunLost; this list is
// only a candidate scan and grants no authority to mutate a run.
func (s PG) ListRunnerLapsedRuns(ctx context.Context, runnerID, after uuid.UUID) ([]types.AgentRun, error) {
	return collect(ctx, s.Pool, "list", "runner lapsed runs", `SELECT `+runCols+` FROM agent_runs WHERE runner_id=$1 AND id>$2 AND placement='local' AND state='RUNNING' AND lost_at IS NULL AND token_renewed_at<clock_timestamp()-$3::interval ORDER BY id LIMIT 100`, []any{runnerID, after, identity.RunTokenLapseAfter.String()}, scanRun)
}

// MarkRunnerRunLost uses the existing loss CAS, with authenticated ownership
// checked in the same committing transaction. A fresh drop never claims loss.
func (s PG) MarkRunnerRunLost(ctx context.Context, runnerID, runID uuid.UUID, expectedRef string, at time.Time) (bool, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed tx is already closed
	run, err := runnerExecutionRun(ctx, tx, runnerID, runID)
	if err != nil {
		return false, err
	}
	if expectedRef == "" || run.SandboxRef != expectedRef {
		return false, ErrRunnerExecutionRefused
	}
	tag, err := tx.Exec(ctx, markRunLostSQL, runID, at, string(types.LostOutage), string(types.RunRunning), identity.RunTokenLapseAfter.String(), string(types.LostReboot))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, tx.Commit(ctx)
}
