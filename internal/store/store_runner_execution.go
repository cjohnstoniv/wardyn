// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrRunnerExecutionRefused covers foreign, stale and incomplete incarnations.
var ErrRunnerExecutionRefused = errors.New("runner execution refused")

// RunnerExecution is an org-issued execution intent. An empty ExecID requires
// explicit reconciliation of the ambiguous command result before exit ingestion.
type RunnerExecution struct {
	RunID      uuid.UUID
	RunnerID   uuid.UUID
	Generation uuid.UUID
	Operation  string
	ExecID     string
}

// BeginRunnerExecution persists the H3-selected generation before LEASE and
// Exec/Start. Retries retain generation; a new incarnation supplies the previous
// generation as a CAS. The existing run-operation lock serializes its entire
// dispatch with exit teardown when H3 holds the returned lock context upstream.
func (s PG) BeginRunnerExecution(ctx context.Context, runnerID, runID, previous, generation uuid.UUID, operation string) (RunnerExecution, error) {
	var out RunnerExecution
	if generation == uuid.Nil || (operation != runnerwire.MethodExec && operation != runnerwire.MethodStart) {
		return out, ErrRunnerExecutionRefused
	}
	ctx, unlock, err := s.Locker().Lock(ctx, db.NewLockKey(db.RunOpLockClass, runID.String()), db.LockWait)
	if err != nil {
		return out, err
	}
	defer unlock()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed tx is already closed
	run, err := runnerExecutionRun(ctx, tx, runnerID, runID)
	if err != nil {
		return out, err
	}
	if (run.State != types.RunStarting && run.State != types.RunRunning) || run.LostAt != nil {
		return out, ErrRunnerExecutionRefused
	}
	current, err := readRunnerExecution(ctx, tx, runID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if err == nil && current.Generation == generation {
		if current.RunnerID != runnerID || current.Operation != operation {
			return out, ErrRunnerExecutionRefused
		}
		var accepted bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runner_execution_exits WHERE run_id=$1 AND generation=$2)`, runID, generation).Scan(&accepted); err != nil {
			return out, err
		}
		if accepted {
			return out, ErrRunnerExecutionRefused
		}
		return current, tx.Commit(ctx)
	}
	if current.Generation != previous || (current.Generation != uuid.Nil && current.ExecID == "") {
		return out, ErrRunnerExecutionRefused
	}
	var incomplete bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runner_execution_exits WHERE run_id=$1 AND NOT tail_complete)`, runID).Scan(&incomplete); err != nil {
		return out, err
	}
	if incomplete {
		return out, ErrRunnerExecutionRefused
	}
	out = RunnerExecution{RunID: runID, RunnerID: runnerID, Generation: generation, Operation: operation}
	_, err = tx.Exec(ctx, `INSERT INTO runner_executions(run_id,runner_id,generation,operation) VALUES($1,$2,$3,$4) ON CONFLICT(run_id) DO UPDATE SET runner_id=EXCLUDED.runner_id,generation=EXCLUDED.generation,operation=EXCLUDED.operation,exec_id=''`, runID, runnerID, generation, operation)
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

// BindRunnerExecution accepts only H3's correlated Exec/Start result or its
// explicit generation-bound reconciliation. It must never be called with a
// first-seen agent_exit handle as proof of which execution the org dispatched.
func (s PG) BindRunnerExecution(ctx context.Context, runnerID, runID, generation uuid.UUID, execID string) error {
	if generation == uuid.Nil || execID == "" {
		return ErrRunnerExecutionRefused
	}
	ctx, unlock, err := s.Locker().Lock(ctx, db.NewLockKey(db.RunOpLockClass, runID.String()), db.LockWait)
	if err != nil {
		return err
	}
	defer unlock()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed tx is already closed
	if _, err := runnerExecutionRun(ctx, tx, runnerID, runID); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE runner_executions SET exec_id=$4 WHERE run_id=$1 AND runner_id=$2 AND generation=$3 AND (exec_id='' OR exec_id=$4)`, runID, runnerID, generation, execID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrRunnerExecutionRefused
	}
	return tx.Commit(ctx)
}

func readRunnerExecution(ctx context.Context, tx pgx.Tx, runID uuid.UUID) (RunnerExecution, error) {
	var out RunnerExecution
	err := tx.QueryRow(ctx, `SELECT run_id,runner_id,generation,operation,exec_id FROM runner_executions WHERE run_id=$1 FOR UPDATE`, runID).Scan(&out.RunID, &out.RunnerID, &out.Generation, &out.Operation, &out.ExecID)
	return out, err
}

func runnerExecutionRun(ctx context.Context, tx pgx.Tx, runnerID, runID uuid.UUID) (types.AgentRun, error) {
	var owner, state string
	var revoked *time.Time
	err := tx.QueryRow(ctx, `SELECT owner,state,revoked_at FROM runners WHERE id=$1 FOR SHARE`, runnerID).Scan(&owner, &state, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.AgentRun{}, ErrRunnerExecutionRefused
	}
	if err != nil {
		return types.AgentRun{}, err
	}
	if state != string(types.RunnerClaimed) || revoked != nil {
		return types.AgentRun{}, ErrRunnerExecutionRefused
	}
	run, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM agent_runs WHERE id=$1 FOR UPDATE`, runID))
	if err != nil {
		return run, err
	}
	if run.RunnerID == nil || *run.RunnerID != runnerID || run.CreatedBy != owner || run.Placement != types.PlacementLocal {
		return run, ErrRunnerExecutionRefused
	}
	return run, nil
}
