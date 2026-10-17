// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AcceptRunnerExit atomically commits the generation-bound terminal transition,
// audit and receipt. It deliberately does not perform terminal side effects;
// RetryRunnerExitTail owns their serialized, retryable completion.
func (s PG) AcceptRunnerExit(ctx context.Context, runnerID, runID uuid.UUID, exit runnerwire.AgentExit, prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (bool, error) {
	if prepare == nil || exit.Generation == uuid.Nil || exit.ExecID == "" || exit.Ref == "" || exit.ObservedAt.IsZero() {
		return false, ErrRunnerExecutionRefused
	}
	ctx, unlock, err := s.Locker().Lock(ctx, db.NewLockKey(db.RunOpLockClass, runID.String()), db.LockWait)
	if err != nil {
		return false, err
	}
	defer unlock()
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed tx is already closed
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return false, err
	}
	run, err := runnerExecutionRun(ctx, tx, runnerID, runID)
	if err != nil {
		return false, err
	}
	payload, hash := runnerExitHash(exit)
	accepted, err := runnerExitAccepted(ctx, tx, runID, exit.Generation, hash)
	if err != nil {
		return false, err
	}
	if accepted {
		return false, tx.Commit(ctx)
	}
	current, err := readRunnerExecution(ctx, tx, runID)
	if err != nil {
		return false, err
	}
	if current.RunnerID != runnerID || current.Generation != exit.Generation || current.ExecID != exit.ExecID || run.SandboxRef != exit.Ref {
		return false, ErrRunnerExecutionRefused
	}
	// LOST is kept metadata, not a second state enum. Only a headless local
	// outage leaves an agent whose later real exit may finish this incarnation.
	if run.State != types.RunRunning || (run.LostAt != nil && (run.Interactive || run.LostReason != types.LostOutage)) {
		return false, ErrRunnerExecutionRefused
	}
	terminal, outcome := types.RunCompleted, "success"
	if exit.Code != 0 {
		terminal, outcome = types.RunFailed, "failure"
	}
	var arrival time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&arrival); err != nil {
		return false, err
	}
	data, _ := json.Marshal(map[string]any{"exit_code": exit.Code, "state": terminal, "generation": exit.Generation, "exec_id": exit.ExecID, "observed_at": exit.ObservedAt, "evidence_source": types.RunEvidenceRunnerAsserted})
	ev, err := prepare(ctx, types.AuditEvent{ID: uuid.New(), RunID: &runID, Time: arrival, ActorType: types.ActorAgent, Actor: "runner:" + runnerID.String(), Action: "run.complete", Target: runID.String(), Outcome: outcome, Data: data})
	if err != nil {
		return false, err
	}
	if err := InsertAuditEventTx(ctx, tx, &ev); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_runs SET state=$2,updated_at=clock_timestamp(),ended_at=clock_timestamp() WHERE id=$1`, runID, terminal); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO runner_execution_exits(run_id,generation,runner_id,payload,payload_hash,accepted_at) VALUES($1,$2,$3,$4,$5,clock_timestamp()) ON CONFLICT(run_id,generation) DO UPDATE SET accepted_at=EXCLUDED.accepted_at`, runID, exit.Generation, runnerID, payload, hash); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	if s.SIEM != nil {
		_ = s.SIEM.Emit(context.WithoutCancel(ctx), ev)
	}
	return true, nil
}

func runnerExitAccepted(ctx context.Context, tx pgx.Tx, runID, generation uuid.UUID, hash string) (bool, error) {
	var oldHash string
	var acceptedAt *time.Time
	var receiptOnly bool
	err := tx.QueryRow(ctx, `SELECT payload_hash,accepted_at,receipt_only FROM runner_execution_exits WHERE run_id=$1 AND generation=$2`, runID, generation).Scan(&oldHash, &acceptedAt, &receiptOnly)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if oldHash != hash || receiptOnly {
		return false, ErrRunnerExecutionRefused
	}
	return acceptedAt != nil, nil
}

func runnerExitHash(exit runnerwire.AgentExit) ([]byte, string) {
	b, _ := json.Marshal(exit)
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}

// RetryRunnerExitTail holds the run-operation lock over validation, destructive
// effects and receipt completion. BeginRunnerExecution uses the same barrier;
// a completed old receipt never touches a newer incarnation.
func (s PG) RetryRunnerExitTail(ctx context.Context, runnerID, runID, generation uuid.UUID, tail func(context.Context, types.AgentRun, runnerwire.AgentExit) error) error {
	if tail == nil {
		return ErrRunnerExecutionRefused
	}
	ctx, unlock, err := s.Locker().Lock(ctx, db.NewLockKey(db.RunOpLockClass, runID.String()), db.LockWait)
	if err != nil {
		return err
	}
	defer unlock()
	run, exit, complete, err := s.runnerExitTail(ctx, runnerID, runID, generation)
	if err != nil || complete {
		return err
	}
	if err := db.LockHeld(ctx); err != nil {
		return err
	}
	if err := tail(ctx, run, exit); err != nil {
		return err
	}
	if err := db.LockHeld(ctx); err != nil {
		return err
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE runner_execution_exits SET tail_complete=true WHERE run_id=$1 AND generation=$2 AND EXISTS(SELECT 1 FROM runner_executions WHERE run_id=$1 AND generation=$2)`, runID, generation)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrRunnerExecutionRefused
	}
	return nil
}

func (s PG) runnerExitTail(ctx context.Context, runnerID, runID, generation uuid.UUID) (types.AgentRun, runnerwire.AgentExit, bool, error) {
	var run types.AgentRun
	var exit runnerwire.AgentExit
	var complete bool
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return run, exit, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed tx is already closed
	run, err = runnerExecutionRun(ctx, tx, runnerID, runID)
	if err != nil {
		return run, exit, false, err
	}
	var payload []byte
	var acceptedAt *time.Time
	var receiptOnly bool
	if err := tx.QueryRow(ctx, `SELECT payload,tail_complete,accepted_at,receipt_only FROM runner_execution_exits WHERE run_id=$1 AND generation=$2 AND runner_id=$3`, runID, generation, runnerID).Scan(&payload, &complete, &acceptedAt, &receiptOnly); err != nil {
		return run, exit, false, err
	}
	if acceptedAt == nil || receiptOnly {
		return run, exit, false, ErrRunnerExecutionRefused
	}
	if complete {
		return run, exit, true, tx.Commit(ctx)
	}
	current, err := readRunnerExecution(ctx, tx, runID)
	if err != nil {
		return run, exit, false, err
	}
	if current.Generation != generation || (run.State != types.RunCompleted && run.State != types.RunFailed) {
		return run, exit, false, ErrRunnerExecutionRefused
	}
	if err := json.Unmarshal(payload, &exit); err != nil {
		return run, exit, false, err
	}
	return run, exit, false, tx.Commit(ctx)
}
