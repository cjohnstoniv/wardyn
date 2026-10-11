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

// RunnerContainmentProof contains org-persisted action bindings, not values
// selected by a runner's action_result or inferred from the latest run. H3
// stores LostAt/Generation with the queued action and supplies them unchanged.
// Nil Generation explicitly means no execution had been issued; the committing
// transaction must still find no execution row, never infer a latest generation.
type RunnerContainmentProof struct {
	RunnerContainment
	ActionID   uuid.UUID
	ObservedAt time.Time
}

// RecordRunnerContainmentTx is called exactly once after H3's first successful
// exact action-result CAS, inside that same READ COMMITTED transaction. A
// duplicate CAS must skip this hook. H3 owns pending clearance and commits only
// if this hook succeeds. No lifecycle or credential state is restored here.
func (s PG) RecordRunnerContainmentTx(ctx context.Context, tx pgx.Tx, proof RunnerContainmentProof, prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) error {
	if prepare == nil || proof.ActionID == uuid.Nil || proof.LostAt.IsZero() || proof.ObservedAt.IsZero() || proof.Ref == "" {
		return ErrRunnerExecutionRefused
	}
	var isolation string
	if err := tx.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
		return err
	}
	if isolation != "read committed" {
		return ErrRunnerExecutionRefused
	}
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return err
	}
	run, err := runnerExecutionRun(ctx, tx, proof.RunnerID, proof.RunID)
	if err != nil {
		return err
	}
	if run.State != types.RunRunning || run.LostAt == nil || !run.LostAt.Equal(proof.LostAt) || run.LostReason != types.LostOutage || run.SandboxRef != proof.Ref {
		return ErrRunnerExecutionRefused
	}
	current, err := readRunnerExecution(ctx, tx, proof.RunID)
	if err != nil && !(errors.Is(err, pgx.ErrNoRows) && proof.Generation == uuid.Nil) {
		return err
	}
	if err == nil && (current.Generation != proof.Generation || current.RunnerID != proof.RunnerID) {
		return ErrRunnerExecutionRefused
	}
	action, err := scanAction(tx.QueryRow(ctx, `SELECT `+actionCols+` FROM runner_pending_actions WHERE id=$1 FOR UPDATE`, proof.ActionID))
	if err != nil {
		return err
	}
	if !runnerContainmentActionMatches(action, proof) {
		return ErrRunnerExecutionRefused
	}
	var arrival time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&arrival); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]any{"containment": "late", "reason": types.LostOutage, "action_id": proof.ActionID, "generation": proof.Generation, "lost_at": proof.LostAt, "observed_at": proof.ObservedAt, "evidence_source": types.RunEvidenceRunnerAsserted})
	ev, err := prepare(ctx, types.AuditEvent{ID: uuid.New(), RunID: &proof.RunID, Time: arrival, ActorType: types.ActorSystem, Actor: "runner:" + proof.RunnerID.String(), Action: "run.containment.reassert", Target: proof.RunID.String(), Outcome: "success", Data: data})
	if err != nil {
		return err
	}
	return InsertAuditEventTx(ctx, tx, &ev)
}

func runnerContainmentActionMatches(action types.RunnerPendingAction, proof RunnerContainmentProof) bool {
	return action.Bound && action.RunnerID == proof.RunnerID && action.RunID == proof.RunID &&
		action.Kind == types.RunnerActionStopProxy && action.Ref == proof.Ref &&
		action.Generation == proof.Generation && action.LostAt != nil && action.LostAt.Equal(proof.LostAt) &&
		action.AppliedAt != nil && action.Outcome == "success" &&
		action.ObservedAt != nil && action.ObservedAt.Equal(proof.ObservedAt)
}
