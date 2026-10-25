// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RunnerEventRefusal is what RefuseRunnerEvent records for one event the organisation refused for good.
type RunnerEventRefusal struct {
	Kind, Code, Sentinel string
	RunID                *uuid.UUID
	ActionID             *uuid.UUID // an action_result's action: settled as refused when it is this runner's and open
	PayloadSHA256        string
	PayloadBytes         int
	Row                  bool                       // write the runner.event.refuse row (false: suppressed by the caller's bound)
	Marker               bool                       // the row is the last of its window: it carries suppressed: true
	Summary              *RunnerEventRefusalSummary // a previous window's summary row, when the caller owes one
}

// RunnerEventRefusalSummary is how many refusals a previous window left without a row of their own.
type RunnerEventRefusalSummary struct {
	Suppressed             int
	WindowStart, WindowEnd time.Time
}

// RefuseRunnerEvent records, in one READ COMMITTED transaction, an event the organisation refused for good, before
// the event is acknowledged (DR-0436): an action_result's open action of this runner is settled as refused with its
// runner.action.refuse receipt (reason org_refused:<code>), then the summary row the caller owes, then the event's
// own runner.event.refuse row unless the caller's bound suppressed it. It takes the shared order runner, run, action
// (lockRunnerActionRun), then the audit chain, as RecordRunnerActionResult does. It reports whether it settled an
// action; with nothing to write and nothing to settle it returns (false, nil) without a transaction.
func (s PG) RefuseRunnerEvent(ctx context.Context, runnerID uuid.UUID, r RunnerEventRefusal, prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (bool, error) {
	if prepare == nil {
		return false, errors.New("store: refuse runner event: no audit policy")
	}
	if r.ActionID == nil && !r.Row && r.Summary == nil {
		return false, nil
	}
	tx, err := beginRunnerActionTx(ctx, s)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	settled := false
	if r.ActionID != nil {
		if settled, err = s.settleRefusedActionTx(ctx, tx, runnerID, *r.ActionID, "org_refused:"+r.Code, prepare); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return false, err
	}
	target := runnerID.String()
	if r.RunID != nil {
		target = r.RunID.String()
	}
	var rows []types.AuditEvent
	write := func(fields map[string]any) error {
		var at time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
			return err
		}
		data, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		ev, err := prepare(ctx, types.AuditEvent{ID: uuid.New(), RunID: r.RunID, Time: at, ActorType: types.ActorSystem,
			Actor: "runner:" + runnerID.String(), Action: "runner.event.refuse", Target: target, Outcome: "denied", Data: data})
		if err != nil {
			return err
		}
		if err := InsertAuditEventTx(ctx, tx, &ev); err != nil {
			return err
		}
		rows = append(rows, ev)
		return nil
	}
	if r.Summary != nil {
		if err := write(map[string]any{"suppressed_count": r.Summary.Suppressed, "window_start": r.Summary.WindowStart,
			"window_end": r.Summary.WindowEnd}); err != nil {
			return false, err
		}
	}
	if r.Row {
		fields := map[string]any{"kind": r.Kind, "code": r.Code, "sentinel": r.Sentinel, "payload_sha256": r.PayloadSHA256,
			"payload_bytes": r.PayloadBytes}
		if r.Marker {
			fields["suppressed"] = true
		}
		if err := write(fields); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	if s.SIEM != nil {
		for _, ev := range rows {
			_ = s.SIEM.Emit(context.WithoutCancel(ctx), ev)
		}
	}
	return settled, nil
}

// settleRefusedActionTx closes actionID as refused when it is runnerID's, still open, and bound as
// RecordRunnerActionResult would require of the runner's own refused answer; anything else settles nothing. A run
// subject is locked runner, run, then action, as every receipt is; an asserted subject runner, then action.
func (s PG) settleRefusedActionTx(ctx context.Context, tx pgx.Tx, runnerID, actionID uuid.UUID, reason string,
	prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (bool, error) {
	// The subject and run are immutable, so reading them before any lock is safe (RecordRunnerActionResult).
	var asserted bool
	var runID *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT subject = 'asserted', run_id FROM runner_pending_actions WHERE id = $1 AND runner_id = $2 AND applied_at IS NULL`,
		actionID, runnerID).Scan(&asserted, &runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var owner string
	var run types.AgentRun
	if asserted {
		var claimed bool
		err := tx.QueryRow(ctx, `SELECT true FROM runners WHERE id = $1 AND state = 'claimed' AND revoked_at IS NULL FOR SHARE`, runnerID).Scan(&claimed)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("store: lock runner: %w", err)
		}
	} else {
		if runID == nil {
			return false, nil
		}
		owner, run, _, err = lockRunnerActionRun(ctx, tx, runnerID, *runID)
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrRunnerActionRefused) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	a, err := scanAction(tx.QueryRow(ctx, `SELECT `+actionCols+` FROM runner_pending_actions WHERE id = $1 AND runner_id = $2 AND applied_at IS NULL FOR UPDATE`,
		actionID, runnerID))
	if errors.Is(err, ErrNotFound) {
		return false, nil // a receipt settled it first
	}
	if err != nil {
		return false, err
	}
	if !a.Bound || a.Asserted != asserted || a.Owner != owner || (!asserted && a.RunID != *runID) {
		return false, nil
	}
	a, err = scanAction(tx.QueryRow(ctx, `UPDATE runner_pending_actions SET applied_at = clock_timestamp(), outcome = $2, observed_at = clock_timestamp()
		WHERE id = $1 AND applied_at IS NULL RETURNING `+actionCols, actionID, types.RunnerActionRefused))
	if err != nil {
		return false, err
	}
	if asserted {
		// An asserted subject has no run row for the audit row to name.
		return true, recordRunnerActionRefusalTx(ctx, tx, a, nil, reason, prepare)
	}
	return true, s.recordRunnerActionReceiptTx(ctx, tx, a, run, reason, *a.ObservedAt, prepare)
}
