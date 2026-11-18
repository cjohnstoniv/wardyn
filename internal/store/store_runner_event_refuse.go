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
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
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
}

// RunnerEventRefusalSpool is where a refused spool record says it sits in its run's stream, in the runner's word.
type RunnerEventRefusalSpool struct {
	RunID, SpoolID uuid.UUID
	Seq            int64
	Hash           string
	Count, DropEnd uint64 // an overflow summary's (Seq 0): how many refusals it reports, and the total they reach
}

// runnerEventRefuseRows runner.event.refuse rows are written per runner per runnerEventRefuseWindow, whichever
// replica takes its events; the next carries suppressed, and the rest of the window is counted for a summary row,
// written by the runner's first refusal of a later window. A window opens at the first refusal after the last one
// ended, on the database's clock.
const (
	runnerEventRefuseRows   = 32
	runnerEventRefuseWindow = time.Hour
)

// refusalSummary is how many refusals a previous window left without a row of their own.
type refusalSummary struct {
	suppressed int
	start, end time.Time
}

// RefuseRunnerEvent records, in one READ COMMITTED transaction, an event the organisation refused for good, before
// the event is acknowledged (DR-0436): an action_result's open action of this runner is settled as refused with its
// runner.action.refuse receipt (reason org_refused:<code>), a spool record next in its run's stream moves the run's
// cursor past it (an overflow summary, the run's overflow mark), then the summary row a previous window owes, then
// the event's own runner.event.refuse row unless the runner's window suppressed it (refusalWindowTx). It takes the
// runner's refusal window first, then the shared order runner, run, action (lockRunnerActionRun), the spool cursor
// (as IngestRunnerDecision takes it after the run), then the audit chain. It reports whether it settled an action
// and, when the runner's window was full so the event got no row of its own, how long that window has left on the
// database's clock (a duration, so a replica adds it to its own clock and the two clocks' skew cancels); zero when
// the event's own row was written.
func (s PG) RefuseRunnerEvent(ctx context.Context, runnerID uuid.UUID, r RunnerEventRefusal, prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (settled bool, fullFor time.Duration, err error) {
	if prepare == nil {
		return false, 0, errors.New("store: refuse runner event: no audit policy")
	}
	tx, err := beginRunnerActionTx(ctx, s)
	if err != nil {
		return false, 0, err
	}
	defer db.Rollback(ctx, tx) // a no-op after Commit
	row, marker, summary, left, err := refusalWindowTx(ctx, tx, runnerID)
	if err != nil {
		return false, 0, err
	}
	if r.ActionID != nil {
		if settled, err = s.settleRefusedActionTx(ctx, tx, runnerID, *r.ActionID, "org_refused:"+r.Code, prepare); err != nil {
			return false, 0, err
		}
	}
	markMoved := false
	if r.Spool != nil {
		if markMoved, err = advanceRefusedSpoolCursorTx(ctx, tx, runnerID, *r.Spool); err != nil {
			return false, 0, err
		}
	}
	var rows []types.AuditEvent
	if row || summary != nil {
		if rows, err = writeRefusalRowsTx(ctx, tx, runnerID, r, row, marker, markMoved, summary, prepare); err != nil {
			return false, 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, 0, err
	}
	if s.SIEM != nil {
		for _, ev := range rows {
			_ = s.SIEM.Emit(context.WithoutCancel(ctx), ev)
		}
	}
	if !row {
		fullFor = left
	}
	return settled, fullFor, nil
}

// refusalWindowTx counts one refusal of runnerID in its window and says what the refusal writes: its own row (the
// last of the window marked suppressed), and the summary a previous window that suppressed rows still owes. The
// window row is locked for the rest of the transaction, so two replicas refusing for one runner at once count one
// after the other and the rows written always match the count. Only a refusal takes this lock, and first, so it
// orders before every other lock the refusal takes. A runner keeps one row, deleted with the runner. left is how
// long the window this refusal was counted in has to run, on the database's clock: more than zero, at most the window.
func refusalWindowTx(ctx context.Context, tx pgx.Tx, runnerID uuid.UUID) (row, marker bool, summary *refusalSummary, left time.Duration, err error) {
	if _, err := tx.Exec(ctx, `INSERT INTO runner_event_refusal_windows (runner_id, window_start) VALUES ($1, clock_timestamp())
 ON CONFLICT (runner_id) DO NOTHING`, runnerID); err != nil {
		return false, false, nil, 0, fmt.Errorf("store: open refusal window: %w", err)
	}
	var start, now time.Time
	var written, suppressed int
	if err := tx.QueryRow(ctx, `SELECT window_start, written, suppressed, clock_timestamp() FROM runner_event_refusal_windows
 WHERE runner_id = $1 FOR UPDATE`, runnerID).Scan(&start, &written, &suppressed, &now); err != nil {
		return false, false, nil, 0, fmt.Errorf("store: lock refusal window: %w", err)
	}
	if now.Sub(start) >= runnerEventRefuseWindow {
		if suppressed > 0 {
			summary = &refusalSummary{suppressed: suppressed, start: start, end: start.Add(runnerEventRefuseWindow)}
		}
		start, written, suppressed = now, 0, 0
	}
	row, marker = written <= runnerEventRefuseRows, written == runnerEventRefuseRows
	if row {
		written++
	} else {
		suppressed++
	}
	if _, err := tx.Exec(ctx, `UPDATE runner_event_refusal_windows SET window_start = $2, written = $3, suppressed = $4 WHERE runner_id = $1`,
		runnerID, start, written, suppressed); err != nil {
		return false, false, nil, 0, fmt.Errorf("store: count refusal: %w", err)
	}
	return row, marker, summary, start.Add(runnerEventRefuseWindow).Sub(now), nil
}

// writeRefusalRowsTx writes the summary row a previous window owes, then the refusal's own row when its window has
// room, under the audit chain lock, and returns them for the audit sinks.
func writeRefusalRowsTx(ctx context.Context, tx pgx.Tx, runnerID uuid.UUID, r RunnerEventRefusal, row, marker, markMoved bool, summary *refusalSummary,
	prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) ([]types.AuditEvent, error) {
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return nil, err
	}
	target, runID, claimed, err := refusalTargetTx(ctx, tx, runnerID, r.RunID)
	if err != nil {
		return nil, err
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
	if summary != nil {
		if err := write(map[string]any{"suppressed_count": summary.suppressed, "window_start": summary.start,
			"window_end": summary.end}); err != nil {
			return nil, err
		}
	}
	if row {
		fields := map[string]any{"kind": r.Kind, "code": r.Code, "sentinel": r.Sentinel, "payload_sha256": r.PayloadSHA256,
			"payload_bytes": r.PayloadBytes}
		if marker {
			fields["suppressed"] = true
		}
		if markMoved { // the row carries count and drop_end only when an overflow summary moved the mark
			fields["count"], fields["drop_end"] = r.Spool.Count, r.Spool.DropEnd
		}
		if claimed != "" {
			fields["claimed_run_id"] = claimed
		}
		if err := write(fields); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// advanceRefusedSpoolCursorTx moves a run's spool cursor past a refused record. The runner acknowledges a refused
// record and deletes it, so a cursor left behind would refuse every later record as a gap and fall short of the
// runner's spool seal (runnerspool Seal). The CAS moves it only for the record right after it, on the cursor of this
// runner's run and spool; a gap, a repeat, a hash with no place in a receipt or another spool's record moves nothing.
// A missing cursor is made as ingest makes it (lockRunnerSpool), only for the stream's first record (the one next
// after none) and a run that is this runner's. The move keeps ingest's replay window (spoolReceipt.accept): only the
// last runnerspool.ReplayBatch hashes stay, so a runner refused record after record cannot grow the cursor.
//
// An overflow summary (Seq 0) moves the overflow mark the same way, to the total the runner's own mark reaches when it
// acknowledges the summary: only a summary whose interval starts at the mark, as accept requires of one it ingests.
// Its hash is kept so that a replay of it is a duplicate there too. It reports whether it moved the overflow mark.
func advanceRefusedSpoolCursorTx(ctx context.Context, tx pgx.Tx, runnerID uuid.UUID, sp RunnerEventRefusalSpool) (moved bool, err error) {
	if sp.Seq < 0 || len(sp.Hash) != 64 {
		return false, nil
	}
	overflow := sp.Seq == 0
	if overflow && (sp.Count == 0 || sp.DropEnd < sp.Count) {
		return false, nil
	}
	if sp.Seq == 1 || (overflow && sp.Count == sp.DropEnd) {
		if _, err := tx.Exec(ctx, `INSERT INTO runner_spool_cursors(run_id,runner_id,spool_id) SELECT $1::uuid,$2::uuid,$3::uuid
 WHERE EXISTS (SELECT 1 FROM agent_runs WHERE id=$1::uuid AND runner_id=$2::uuid) ON CONFLICT(run_id) DO NOTHING`, sp.RunID, runnerID, sp.SpoolID); err != nil {
			return false, err
		}
	}
	if overflow {
		tag, err := tx.Exec(ctx, `UPDATE runner_spool_cursors SET overflow_end=$4::text::numeric,overflow_hash=$5::text
 WHERE run_id=$1::uuid AND runner_id=$2::uuid AND spool_id=$3::uuid AND overflow_end=$6::text::numeric`,
			sp.RunID, runnerID, sp.SpoolID, strconv.FormatUint(sp.DropEnd, 10), sp.Hash, strconv.FormatUint(sp.DropEnd-sp.Count, 10))
		return tag.RowsAffected() == 1, err
	}
	_, err = tx.Exec(ctx, `UPDATE runner_spool_cursors SET last_seq=$4::bigint,recent_hashes=(recent_hashes||jsonb_build_object($5::text,$6::text))
 - ARRAY(SELECT k FROM jsonb_object_keys(recent_hashes) AS k WHERE k::bigint <= $4::bigint-$8::bigint)
 WHERE run_id=$1::uuid AND runner_id=$2::uuid AND spool_id=$3::uuid AND last_seq=$7::bigint`,
		sp.RunID, runnerID, sp.SpoolID, sp.Seq, strconv.FormatInt(sp.Seq, 10), sp.Hash, sp.Seq-1, int64(runnerspool.ReplayBatch))
	return false, err
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
