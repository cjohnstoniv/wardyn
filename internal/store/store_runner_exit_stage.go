// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// StageRunnerExit is the checked read-loop path. It stores one immutable,
// bounded candidate for the issued generation without binding its asserted
// handle or changing lifecycle/audit. The durable stage is sufficient for ACK.
func (s PG) StageRunnerExit(ctx context.Context, runnerID, runID uuid.UUID, exit runnerwire.AgentExit) error {
	payload, hash := runnerExitHash(exit)
	if len(payload) > 4096 || exit.Generation == uuid.Nil || exit.ExecID == "" || exit.Ref == "" || exit.ObservedAt.IsZero() {
		return ErrRunnerExecutionRefused
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed tx is already closed
	run, err := runnerExecutionRun(ctx, tx, runnerID, runID)
	if err != nil {
		return err
	}
	var oldHash string
	err = tx.QueryRow(ctx, `SELECT payload_hash FROM runner_execution_exits WHERE run_id=$1 AND generation=$2`, runID, exit.Generation).Scan(&oldHash)
	if err == nil {
		if oldHash != hash {
			return ErrRunnerExecutionRefused
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	current, err := readRunnerExecution(ctx, tx, runID)
	if err != nil {
		return err
	}
	if current.RunnerID != runnerID || current.Generation != exit.Generation || run.SandboxRef != exit.Ref {
		return ErrRunnerExecutionRefused
	}
	// The org already settled this run (terminal, or ended at its lease): the
	// exit is acknowledged without a receipt, so it can make no transition.
	if run.State.IsTerminal() || (run.State == types.RunRunning && run.LostAt != nil && run.LostReason == types.LostEnded) {
		return tx.Commit(ctx)
	}
	if run.State != types.RunRunning || (run.LostAt != nil && (run.Interactive || run.LostReason != types.LostOutage)) {
		return ErrRunnerExecutionRefused
	}
	_, err = tx.Exec(ctx, `INSERT INTO runner_execution_exits(run_id,generation,runner_id,payload,payload_hash) VALUES($1,$2,$3,$4,$5)`, runID, exit.Generation, runnerID, payload, hash)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RunnerExitWork is durable staged or accepted work. Only a bound matching
// handle can be applied; ambiguous results wait for H3 reconciliation.
type RunnerExitWork struct {
	RunnerID uuid.UUID
	RunID    uuid.UUID
	Exit     runnerwire.AgentExit
}

// ListRunnerExitWork pages by run UUID. Generation cannot advance while one
// incomplete receipt exists, so a run has at most one item in this work list.
func (s PG) ListRunnerExitWork(ctx context.Context, after uuid.UUID) ([]RunnerExitWork, error) {
	return collect(ctx, s.Pool, "list", "runner exit work", `SELECT x.runner_id,x.run_id,x.payload FROM runner_execution_exits x
 JOIN runner_executions e ON e.run_id=x.run_id AND e.generation=x.generation
 WHERE NOT x.tail_complete AND x.run_id>$1 AND e.exec_id<>'' AND e.exec_id=x.payload->>'exec_id'
 ORDER BY x.run_id LIMIT 100`, []any{after}, func(row pgx.Row) (RunnerExitWork, error) {
		var out RunnerExitWork
		var payload []byte
		err := row.Scan(&out.RunnerID, &out.RunID, &payload)
		if err != nil {
			return out, err
		}
		err = json.Unmarshal(payload, &out.Exit)
		return out, err
	})
}
