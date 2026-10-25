// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
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
	ActionID             *uuid.UUID               // an action_result's action: settled as refused when it is this runner's and open
	Spool                *RunnerEventRefusalSpool // a spool record's place: the run's cursor moves past it when it is next
	PayloadSHA256        string
	PayloadBytes         int
	Row                  bool                       // write the runner.event.refuse row (false: suppressed by the caller's bound)
	Marker               bool                       // the row is the last of its window: it carries suppressed: true
	Summary              *RunnerEventRefusalSummary // a previous window's summary row, when the caller owes one
}

// RunnerEventRefusalSpool is where a refused spool record says it sits in its run's stream, in the runner's word.
type RunnerEventRefusalSpool struct {
	RunID, SpoolID uuid.UUID
	Seq            int64
	Hash           string
}

// RunnerEventRefusalSummary is how many refusals a previous window left without a row of their own.
type RunnerEventRefusalSummary struct {
	Suppressed             int
	WindowStart, WindowEnd time.Time
}

// RefuseRunnerEvent records, in one READ COMMITTED transaction, an event the organisation refused for good, before
// the event is acknowledged (DR-0436): an action_result's open action of this runner is settled as refused with its
// runner.action.refuse receipt (reason org_refused:<code>), a spool record next in its run's stream moves the run's
// cursor past it, then the summary row the caller owes, then the event's own runner.event.refuse row unless the
// caller's bound suppressed it. It takes the shared order runner, run, action (lockRunnerActionRun), the spool cursor
// (as IngestRunnerDecision takes it after the run), then the audit chain. It reports whether it settled an action;
// with nothing to write, settle or move it returns (false, nil) without a transaction.
func (s PG) RefuseRunnerEvent(ctx context.Context, runnerID uuid.UUID, r RunnerEventRefusal, prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (bool, error) {
	if prepare == nil {
		return false, errors.New("store: refuse runner event: no audit policy")
	}
	if r.ActionID == nil && r.Spool == nil && !r.Row && r.Summary == nil {
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
	if r.Spool != nil {
		if err := advanceRefusedSpoolCursorTx(ctx, tx, runnerID, *r.Spool); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return false, err
	}
	target, runID, claimed, err := refusalTargetTx(ctx, tx, runnerID, r.RunID)
	if err != nil {
		return false, err
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
		ev, err := prepare(ctx, types.AuditEvent{ID: uuid.New(), RunID: runID, Time: at, ActorType: types.ActorSystem,
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
		if claimed != "" {
			fields["claimed_run_id"] = claimed
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

// advanceRefusedSpoolCursorTx moves a run's spool cursor past a refused record. The runner acknowledges a refused
// record and deletes it, so a cursor left behind would refuse every later record as a gap and fall short of the
// runner's spool seal (runnerspool Seal). The CAS moves it only for the record right after it, on the cursor of this
// runner's run and spool; a gap, a repeat, a hash with no place in a receipt or another spool's record moves nothing.
// A missing cursor is made as ingest makes it (lockRunnerSpool), only for the stream's first record (the one next
// after none) and a run that is this runner's.
func advanceRefusedSpoolCursorTx(ctx context.Context, tx pgx.Tx, runnerID uuid.UUID, sp RunnerEventRefusalSpool) error {
	if sp.Seq <= 0 || len(sp.Hash) != 64 {
		return nil
	}
	if sp.Seq == 1 {
		if _, err := tx.Exec(ctx, `INSERT INTO runner_spool_cursors(run_id,runner_id,spool_id) SELECT $1::uuid,$2::uuid,$3::uuid
 WHERE EXISTS (SELECT 1 FROM agent_runs WHERE id=$1::uuid AND runner_id=$2::uuid) ON CONFLICT(run_id) DO NOTHING`, sp.RunID, runnerID, sp.SpoolID); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE runner_spool_cursors SET last_seq=$4::bigint,recent_hashes=recent_hashes||jsonb_build_object($5::text,$6::text)
 WHERE run_id=$1::uuid AND runner_id=$2::uuid AND spool_id=$3::uuid AND last_seq=$7::bigint`,
		sp.RunID, runnerID, sp.SpoolID, sp.Seq, strconv.FormatInt(sp.Seq, 10), sp.Hash, sp.Seq-1)
	return err
}

// refusalTargetTx names a refusal row's run. The run id is the runner's word: the row joins a run's trail only when
// that run is this runner's, else it names the runner and keeps the claim for its data.
func refusalTargetTx(ctx context.Context, tx pgx.Tx, runnerID uuid.UUID, claim *uuid.UUID) (target string, runID *uuid.UUID, claimed string, err error) {
	if claim == nil {
		return runnerID.String(), nil, "", nil
	}
	var own bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_runs WHERE id = $1 AND runner_id = $2)`, *claim, runnerID).Scan(&own); err != nil {
		return "", nil, "", err
	}
	if own {
		return claim.String(), claim, "", nil
	}
	return runnerID.String(), nil, claim.String(), nil
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
		if _, err := tx.Exec(ctx, `SELECT 1 FROM runner_pending_actions WHERE id = $1 FOR UPDATE`, actionID); err != nil {
			return false, err
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
