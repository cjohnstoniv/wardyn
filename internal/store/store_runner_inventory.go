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

// RunnerInventoryResult describes committed intake, not permission to dispatch.
// Reconciliation requirements are durable even if the caller loses its reply.
type RunnerInventoryResult struct {
	Complete            bool
	NeedsReconciliation bool
}

// BeginRunnerInventory captures original candidates before fresh PENDING/STATE.
// The authenticated Hub invokes it under its per-runner session admission lock.
// Repeating the same session never resets a partially consumed inventory.
func (s PG) BeginRunnerInventory(ctx context.Context, runnerID uuid.UUID, sessionID string) error {
	session, err := uuid.Parse(sessionID)
	if err != nil || session == uuid.Nil {
		return ErrRunnerExecutionRefused
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed transaction is closed
	owner, err := inventoryOwner(ctx, tx, runnerID)
	if err != nil {
		return err
	}
	var current uuid.UUID
	err = tx.QueryRow(ctx, `SELECT session_id FROM runner_inventories WHERE runner_id=$1 FOR UPDATE`, runnerID).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if current == session {
		return tx.Commit(ctx)
	}
	if _, err = tx.Exec(ctx, `DELETE FROM runner_inventories WHERE runner_id=$1`, runnerID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO runner_inventories(runner_id,session_id,owner) VALUES($1,$2,$3)`, runnerID, session, owner); err != nil {
		return err
	}
	// xmin is a conservative snapshot version: even an unrelated committed run
	// change prevents an omission from being attributed to the old incarnation.
	// No entire candidate set is materialized in application memory.
	_, err = tx.Exec(ctx, `INSERT INTO runner_inventory_candidates(runner_id,run_id,run_version,ref,generation)
		SELECT $1,a.id,a.xmin::text,a.sandbox_ref,COALESCE(e.generation,$3)
		FROM agent_runs a LEFT JOIN runner_executions e ON e.run_id=a.id
		WHERE a.runner_id=$1 AND a.created_by=$2 AND a.placement='local'
		AND a.state='RUNNING' AND a.lost_at IS NULL AND a.sandbox_ref<>''`, runnerID, owner, uuid.Nil)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func inventoryOwner(ctx context.Context, tx pgx.Tx, runnerID uuid.UUID) (string, error) {
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout='2s'`); err != nil {
		return "", err
	}
	var owner string
	err := tx.QueryRow(ctx, `SELECT owner FROM runners WHERE id=$1 AND state='claimed' AND revoked_at IS NULL AND owner<>'' FOR SHARE`, runnerID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrRunnerExecutionRefused
	}
	return owner, err
}

// IntakeRunnerStatePage commits a bounded page and its cursor together. A
// partial page never checks omissions. This path performs no wire call,
// credential renewal, lifecycle transition or accepted-exit acknowledgement.
func (s PG) IntakeRunnerStatePage(ctx context.Context, runnerID uuid.UUID, sessionID string, page runnerwire.StatePage) (RunnerInventoryResult, error) {
	var out RunnerInventoryResult
	session, err := uuid.Parse(sessionID)
	if err != nil || session == uuid.Nil {
		return out, ErrRunnerExecutionRefused
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed transaction is closed
	owner, err := inventoryOwner(ctx, tx, runnerID)
	if err != nil {
		return out, err
	}
	var cursor runnerwire.StateInventory
	err = tx.QueryRow(ctx, `SELECT COALESCE(inventory_id,$4),last_run_id,complete,needs_reconciliation
		FROM runner_inventories WHERE runner_id=$1 AND session_id=$2 AND owner=$3 FOR UPDATE`, runnerID, session, owner, uuid.Nil).
		Scan(&cursor.ID, &cursor.Last, &cursor.Complete, &out.NeedsReconciliation)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrRunnerExecutionRefused
	}
	if err != nil {
		return out, err
	}
	next, err := cursor.Next(page)
	if err != nil {
		return out, err
	}
	for _, row := range page.Runs {
		needs, err := s.intakeRunnerStateRow(ctx, tx, runnerID, row)
		if err != nil {
			return out, err
		}
		out.NeedsReconciliation = out.NeedsReconciliation || needs
	}
	if next.Complete {
		var omitted bool
		if err := tx.QueryRow(ctx, inventoryOmissionsSQL, runnerID, owner, uuid.Nil).Scan(&omitted); err != nil {
			return out, err
		}
		out.NeedsReconciliation = out.NeedsReconciliation || omitted
	}
	_, err = tx.Exec(ctx, `UPDATE runner_inventories SET inventory_id=$2,last_run_id=$3,complete=$4,needs_reconciliation=$5 WHERE runner_id=$1`, runnerID, next.ID, next.Last, next.Complete, out.NeedsReconciliation)
	if err != nil {
		return out, err
	}
	out.Complete = next.Complete
	return out, tx.Commit(ctx)
}

func (s PG) intakeRunnerStateRow(ctx context.Context, tx pgx.Tx, runnerID uuid.UUID, row runnerwire.RunState) (bool, error) {
	if row.SpoolDepth < 0 || row.SpoolDrops < 0 {
		return false, ErrRunnerExecutionRefused
	}
	containment, needsLoss, err := s.reconcileRunnerStateRowTx(ctx, tx, runnerID, row)
	if err != nil {
		return false, err
	}
	assertion, err := json.Marshal(row)
	if err != nil {
		return false, err
	}
	var proof []byte
	if containment != nil {
		proof, err = json.Marshal(containment)
		if err != nil {
			return false, err
		}
		// The stop_proxy commits with the page, so a refused Final always
		// leaves it durable for the next fresh PENDING.
		lostAt := containment.LostAt
		if _, _, err = queueRunnerActionTx(ctx, tx, types.RunnerPendingAction{ID: uuid.New(), RunnerID: runnerID, RunID: row.RunID,
			Kind: types.RunnerActionStopProxy, Ref: containment.Ref, Generation: containment.Generation, LostAt: &lostAt}); err != nil {
			return false, err
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO runner_inventory_observations(runner_id,run_id,assertion,containment,needs_loss) VALUES($1,$2,$3,$4,$5)`, runnerID, row.RunID, assertion, proof, needsLoss); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `UPDATE runner_inventory_candidates SET seen=true WHERE runner_id=$1 AND run_id=$2`, runnerID, row.RunID)
	return needsLoss || containment != nil, err
}

const inventoryOmissionsSQL = `SELECT EXISTS (
	SELECT 1 FROM runner_inventory_candidates c JOIN agent_runs a ON a.id=c.run_id
	LEFT JOIN runner_executions e ON e.run_id=a.id
	WHERE c.runner_id=$1 AND NOT c.seen AND a.runner_id=$1 AND a.created_by=$2
	AND a.placement='local' AND a.state='RUNNING' AND a.lost_at IS NULL
	AND a.sandbox_ref=c.ref AND a.xmin::text=c.run_version AND COALESCE(e.generation,$3)=c.generation
)`
