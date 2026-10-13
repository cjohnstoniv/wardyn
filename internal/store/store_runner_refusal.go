// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxRunnerRefusalReason bounds the reason a refused action_result carries,
// as the runner bounds it.
const maxRunnerRefusalReason = runnerwire.MaxRefusalReason

// validRunnerActionResult reports whether a result names its runner, run,
// action and observed time, its outcome is in the closed set, and a reason
// accompanies a refusal and nothing else.
func validRunnerActionResult(runnerID, runID, actionID uuid.UUID, outcome, reason string, observedAt time.Time) bool {
	if runnerID == uuid.Nil || runID == uuid.Nil || actionID == uuid.Nil || observedAt.IsZero() {
		return false
	}
	switch outcome {
	case types.RunnerActionSuccess, types.RunnerActionDeferred:
		return reason == ""
	case types.RunnerActionRefused:
		return reason != "" && len(reason) <= maxRunnerRefusalReason
	}
	return false
}

// recordRunnerActionRefusalTx writes runner.action.refuse for the first refused
// receipt of a, inside the receipt's transaction. runID is nil for an asserted
// subject, which has no run row. The run itself is left as it was.
func recordRunnerActionRefusalTx(ctx context.Context, tx pgx.Tx, a types.RunnerPendingAction, runID *uuid.UUID, reason string,
	prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) error {
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return err
	}
	var arrival time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&arrival); err != nil {
		return err
	}
	data, err := json.Marshal(map[string]any{"action": a.Kind, "run_id": a.RunID, "reason": reason})
	if err != nil {
		return err
	}
	ev, err := prepare(ctx, types.AuditEvent{ID: uuid.New(), RunID: runID, Time: arrival, ActorType: types.ActorSystem, Actor: "runner:" + a.RunnerID.String(),
		Action: "runner.action.refuse", Target: a.ID.String(), Outcome: "failure", Data: data})
	if err != nil {
		return err
	}
	return InsertAuditEventTx(ctx, tx, &ev)
}
