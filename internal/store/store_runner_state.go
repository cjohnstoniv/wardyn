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
// the checked STATE handler; page intake commits any containment action first.
var ErrRunnerReconciliationRequired = errors.New("runner lifecycle reconciliation required")

// RunnerContainment identifies the exact kept incarnation requiring H3's
// durable stop_proxy queue. Only an exact action_result may settle that action.
type RunnerContainment struct {
	RunnerID, RunID, Generation uuid.UUID
	Ref                         string
	LostAt                      time.Time
}

// reconcileRunnerStateRowTx validates one runner assertion against org
// authority. It does not renew credentials, bind handles, clear lost marks or
// finalize runs, and takes no run-operation lock or remote RPC.
func (s PG) reconcileRunnerStateRowTx(ctx context.Context, tx pgx.Tx, runnerID uuid.UUID, row runnerwire.RunState) (*RunnerContainment, bool, error) {
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
	return request, needsLoss, nil
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
	// Wait for the run row before the claim takes the runner's: runnerExecutionRun locks the runner row,
	// then the run row, and a wait there on a held run row would hold every write to the runner (its
	// reconnect's among them) behind that row's holder. The claim below still rechecks everything.
	if _, err := s.Pool.Exec(ctx, `SELECT 1 FROM agent_runs WHERE id = $1 FOR UPDATE`, runID); err != nil {
		return false, err
	}
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
