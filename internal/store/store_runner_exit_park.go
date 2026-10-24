// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ParkRunnerExit stops the exit sweep retrying a receipt it gave up on. The row is kept with parked_at
// set, so it still fences its run as an incomplete receipt, and one runner.exit.park audit row records
// how many tries were refused and why. It holds the run-operation lock, as acceptance and the tail do.
// It reports false, writing nothing, when the receipt is already parked or no longer incomplete.
func (s PG) ParkRunnerExit(ctx context.Context, runnerID, runID, generation uuid.UUID, attempts int, cause string, prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (bool, error) {
	if prepare == nil {
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
	var arrival time.Time
	err = tx.QueryRow(ctx, `UPDATE runner_execution_exits SET parked_at=clock_timestamp()
 WHERE run_id=$1 AND generation=$2 AND runner_id=$3 AND parked_at IS NULL AND NOT tail_complete AND NOT receipt_only
 RETURNING parked_at`, runID, generation, runnerID).Scan(&arrival)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, tx.Commit(ctx)
	}
	if err != nil {
		return false, err
	}
	data, _ := json.Marshal(map[string]any{"runner_id": runnerID, "generation": generation, "attempts": attempts, "error": cause})
	ev, err := prepare(ctx, types.AuditEvent{ID: uuid.New(), RunID: &runID, Time: arrival, ActorType: types.ActorSystem, Actor: "wardynd",
		Action: "runner.exit.park", Target: runID.String(), Outcome: "success", Data: data})
	if err != nil {
		return false, err
	}
	if err := InsertAuditEventTx(ctx, tx, &ev); err != nil {
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

// ParkedRunnerExits counts runnerID's parked exit receipts.
func (s PG) ParkedRunnerExits(ctx context.Context, runnerID uuid.UUID) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM runner_execution_exits WHERE runner_id=$1 AND parked_at IS NOT NULL`, runnerID).Scan(&n)
	return n, err
}
