// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RetirementSuccessorBound is how many fresh-ID successors of deferred actions
// one retirement obligation gets, both phases counted, before it is abandoned.
const RetirementSuccessorBound = 16

// Why a retirement obligation closed without a settlement (runner_retirements.reason).
const (
	RetirementDeferredExhausted = "deferred_exhausted"
	RetirementRunnerRevoked     = "runner_revoked"
	RetirementRunnerRefused     = "runner_refused"
)

// What one AdvanceRunnerRetirement call did.
const (
	RetirementIdle      = ""
	RetirementKill      = "kill"
	RetirementRetire    = "retire"
	RetirementSuccessor = "successor"
	RetirementAbandoned = "abandoned"
)

// runnerRetirement is a runner_retirements row: the binding snapshot of one
// retirement obligation, captured under the run lock before its ref is cleared.
type runnerRetirement struct {
	runID, runnerID, obligationID, generation uuid.UUID
	ref, execID, state                        string
	successors                                int
}

func (t runnerRetirement) open() bool { return t.state == "kill" || t.state == "retire" }

// phaseKind is the action kind of t's open phase.
func (t runnerRetirement) phaseKind() types.RunnerActionKind {
	if t.state == "retire" {
		return types.RunnerActionRetire
	}
	return types.RunnerActionKill
}

// retirementPhase is the snapshot state an action of kind belongs to, or "".
func retirementPhase(kind types.RunnerActionKind) string {
	switch kind {
	case types.RunnerActionKill:
		return "kill"
	case types.RunnerActionRetire:
		return "retire"
	}
	return ""
}

// InsertRunnerOutputWatermark records, at dispatch, whether a runner run's
// create asks the runner to keep its output. Without the row the run is never
// retired.
func (s PG) InsertRunnerOutputWatermark(ctx context.Context, runID uuid.UUID, capture bool) error {
	if _, err := s.Pool.Exec(ctx, `INSERT INTO runner_output_watermarks (run_id, capture) VALUES ($1, $2) ON CONFLICT (run_id) DO NOTHING`, runID, capture); err != nil {
		return fmt.Errorf("store: insert runner output watermark: %w", err)
	}
	return nil
}

// AdvanceRunnerOutputWatermark records that the org accepted runID's output up
// to accepted. It only grows, so a repeated or late commit is harmless; a run
// with no row (dispatched before the table) is left alone.
func (s PG) AdvanceRunnerOutputWatermark(ctx context.Context, runID uuid.UUID, accepted int64) error {
	if accepted < 0 {
		return errors.New("store: negative runner output watermark")
	}
	if _, err := s.Pool.Exec(ctx, `UPDATE runner_output_watermarks SET accepted_end = GREATEST(accepted_end, $2), updated_at = now() WHERE run_id = $1`, runID, accepted); err != nil {
		return fmt.Errorf("store: advance runner output watermark: %w", err)
	}
	return nil
}

// ClearSandboxRefRetaining clears a run's sandbox ref, as SetSandboxRef(id, "")
// does. For a runner run it first snapshots, in the same transaction and under
// the runner → run → execution lock order, the binding a later retirement
// needs: the ref, generation and exec id the runner holds the run under.
func (s PG) ClearSandboxRefRetaining(ctx context.Context, runID uuid.UUID) error {
	tx, err := beginRunnerActionTx(ctx, s)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	var runnerID *uuid.UUID
	var ref string
	err = tx.QueryRow(ctx, `SELECT runner_id, sandbox_ref FROM agent_runs WHERE id = $1`, runID).Scan(&runnerID, &ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("store: read run ref: %w", err)
	}
	if runnerID != nil && ref != "" {
		_, run, _, err := lockRunnerActionRun(ctx, tx, *runnerID, runID)
		switch {
		case err == nil:
			if _, err := openRetirementTx(ctx, tx, run); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		case !errors.Is(err, ErrNotFound) && !errors.Is(err, ErrRunnerActionRefused):
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_runs SET sandbox_ref = '', updated_at = now() WHERE id = $1`, runID); err != nil {
		return fmt.Errorf("store: clear sandbox ref: %w", err)
	}
	return tx.Commit(ctx)
}

// openRetirementTx inserts run's snapshot in state kill from its live ref, or
// finds the one already there. The caller holds lockRunnerActionRun. A run
// with no ref or no watermark row is ErrNotFound: it is never retired.
func openRetirementTx(ctx context.Context, tx pgx.Tx, run types.AgentRun) (runnerRetirement, error) {
	t, err := readRetirementTx(ctx, tx, run.ID)
	if !errors.Is(err, ErrNotFound) {
		return t, err
	}
	var watermarked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runner_output_watermarks WHERE run_id = $1)`, run.ID).Scan(&watermarked); err != nil {
		return t, err
	}
	if run.SandboxRef == "" || !watermarked {
		return t, ErrNotFound
	}
	var generation *uuid.UUID
	execID := ""
	cur, err := readRunnerExecution(ctx, tx, run.ID)
	switch {
	case err == nil:
		if cur.Generation != uuid.Nil {
			generation = &cur.Generation
		}
		execID = cur.ExecID
	case !errors.Is(err, pgx.ErrNoRows):
		return t, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO runner_retirements (run_id, runner_id, obligation_id, ref, generation, exec_id)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (run_id) DO NOTHING`, run.ID, *run.RunnerID, uuid.New(), run.SandboxRef, generation, execID); err != nil {
		return t, fmt.Errorf("store: open runner retirement: %w", err)
	}
	return readRetirementTx(ctx, tx, run.ID)
}

func readRetirementTx(ctx context.Context, tx pgx.Tx, runID uuid.UUID) (runnerRetirement, error) {
	t := runnerRetirement{runID: runID}
	var generation *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT runner_id, obligation_id, ref, generation, exec_id, state, successors
		FROM runner_retirements WHERE run_id = $1 FOR UPDATE`, runID).Scan(&t.runnerID, &t.obligationID, &t.ref, &generation, &t.execID, &t.state, &t.successors)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, fmt.Errorf("store: read runner retirement: %w", err)
	}
	if generation != nil {
		t.generation = *generation
	}
	return t, nil
}

// RunnerRetirementCandidate is one run the retirement producer may advance,
// with the runner that holds it.
type RunnerRetirementCandidate struct {
	RunID, RunnerID uuid.UUID
}

// RunnerRetirementCandidates lists the terminal runner runs the retirement
// producer looks at: a claimed, unrevoked runner whose caps said it settles
// retire_run (retire_capable), a watermark row, a spool cursor, and a snapshot
// still open or none yet with a live ref. A closed snapshot makes a run
// permanently ineligible. A run whose obligation has an action still waiting
// for the runner is left out until the runner answers it.
func (s PG) RunnerRetirementCandidates(ctx context.Context) ([]RunnerRetirementCandidate, error) {
	return collect(ctx, s.Pool, "list", "runner retirement candidates", `SELECT r.id, r.runner_id FROM runner_output_watermarks w
		JOIN agent_runs r ON r.id = w.run_id
		JOIN runner_spool_cursors c ON c.run_id = r.id AND c.runner_id = r.runner_id
		JOIN runners rn ON rn.id = r.runner_id AND rn.state = 'claimed' AND rn.revoked_at IS NULL AND rn.retire_capable
		LEFT JOIN runner_retirements t ON t.run_id = r.id
		WHERE NOT (r.state = ANY($1)) AND (t.state IN ('kill', 'retire') OR (t.run_id IS NULL AND r.sandbox_ref <> ''))
			AND NOT EXISTS (SELECT 1 FROM runner_pending_actions p WHERE p.run_id = r.id AND p.obligation_id = t.obligation_id AND p.applied_at IS NULL)
		ORDER BY r.id`, []any{nonTerminalStates()}, func(row pgx.Row) (RunnerRetirementCandidate, error) {
		var c RunnerRetirementCandidate
		return c, row.Scan(&c.RunID, &c.RunnerID)
	})
}

// AdvanceRunnerRetirement takes one step of runID's retirement, in one
// transaction under the runner → run → execution lock order, and says which:
//   - no snapshot: open one from the live ref (state kill);
//   - kill, no action yet: queue the obligation's first kill;
//   - kill, its latest kill succeeded: move to retire and queue the retire_run
//     with a receipt computed now, recorded as runner.retire;
//   - the latest action deferred: queue a fresh-ID successor in the same phase
//     (a retire_run with a recomputed receipt), or abandon the obligation once
//     RetirementSuccessorBound successors were spent.
//
// Anything else (an action still open, another obligation fencing the run, a
// run not terminal or not eligible, a closed snapshot) is RetirementIdle.
func (s PG) AdvanceRunnerRetirement(ctx context.Context, runID uuid.UUID,
	prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (string, error) {
	if prepare == nil {
		return RetirementIdle, ErrRunnerActionRefused
	}
	tx, err := beginRunnerActionTx(ctx, s)
	if err != nil {
		return RetirementIdle, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	var runnerID *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT runner_id FROM agent_runs WHERE id = $1`, runID).Scan(&runnerID)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && runnerID == nil) {
		return RetirementIdle, ErrNotFound
	}
	if err != nil {
		return RetirementIdle, err
	}
	owner, run, _, err := lockRunnerActionRun(ctx, tx, *runnerID, runID)
	if err != nil {
		return RetirementIdle, err
	}
	step, err := advanceRetirementTx(ctx, tx, owner, run, prepare)
	if err != nil {
		return RetirementIdle, err
	}
	return step, tx.Commit(ctx)
}

func advanceRetirementTx(ctx context.Context, tx pgx.Tx, owner string, run types.AgentRun,
	prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (string, error) {
	var eligible bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runner_spool_cursors WHERE run_id = $1 AND runner_id = $2)`, run.ID, *run.RunnerID).Scan(&eligible); err != nil {
		return RetirementIdle, err
	}
	if !run.State.IsTerminal() || !eligible {
		return RetirementIdle, nil
	}
	t, err := openRetirementTx(ctx, tx, run)
	if errors.Is(err, ErrNotFound) || (err == nil && !t.open()) {
		return RetirementIdle, nil
	}
	if err != nil {
		return RetirementIdle, err
	}
	var fenced bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runner_pending_actions WHERE run_id = $1
		AND (applied_at IS NULL OR outcome = 'deferred') AND settled_at IS NULL AND COALESCE(obligation_id, id) <> $2)`, run.ID, t.obligationID).Scan(&fenced); err != nil {
		return RetirementIdle, err
	}
	if fenced {
		return RetirementIdle, nil
	}
	tip, err := scanAction(tx.QueryRow(ctx, `SELECT `+actionCols+` FROM runner_pending_actions
		WHERE run_id = $1 AND obligation_id = $2 ORDER BY queue_seq DESC LIMIT 1 FOR UPDATE`, run.ID, t.obligationID))
	switch {
	case errors.Is(err, ErrNotFound):
		if t.state != "kill" {
			return RetirementIdle, nil
		}
		return RetirementKill, insertRetirementActionTx(ctx, tx, owner, t, types.RunnerActionKill, nil, nil)
	case err != nil:
		return RetirementIdle, err
	case tip.AppliedAt == nil || tip.Kind != t.phaseKind():
		return RetirementIdle, nil
	case tip.Outcome == types.RunnerActionDeferred:
		if t.successors >= RetirementSuccessorBound {
			return RetirementAbandoned, abandonRetirementTx(ctx, tx, t, RetirementDeferredExhausted, prepare)
		}
		if _, err := tx.Exec(ctx, `UPDATE runner_pending_actions SET superseded = true WHERE id = $1`, tip.ID); err != nil {
			return RetirementIdle, fmt.Errorf("store: supersede deferred retirement action: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE runner_retirements SET successors = successors + 1 WHERE run_id = $1`, run.ID); err != nil {
			return RetirementIdle, err
		}
		if tip.Kind == types.RunnerActionKill {
			return RetirementSuccessor, insertRetirementActionTx(ctx, tx, owner, t, types.RunnerActionKill, nil, &tip.ID)
		}
		return RetirementSuccessor, queueRetireTx(ctx, tx, owner, t, &tip.ID, prepare)
	case tip.Outcome == types.RunnerActionSuccess && t.state == "kill":
		// The retire_run is the kill's success successor: never queued beside
		// a kill, so it always meets a containment the runner settled.
		if _, err := tx.Exec(ctx, `UPDATE runner_retirements SET state = 'retire' WHERE run_id = $1 AND state = 'kill'`, run.ID); err != nil {
			return RetirementIdle, err
		}
		t.state = "retire"
		return RetirementRetire, queueRetireTx(ctx, tx, owner, t, nil, prepare)
	}
	return RetirementIdle, nil
}

// insertRetirementActionTx queues one action of t's obligation, bound to the
// snapshot (never the run's current ref, which is cleared).
func insertRetirementActionTx(ctx context.Context, tx pgx.Tx, owner string, t runnerRetirement,
	kind types.RunnerActionKind, proof []byte, supersedes *uuid.UUID) error {
	var generation *uuid.UUID
	if t.generation != uuid.Nil {
		generation = &t.generation
	}
	if _, err := tx.Exec(ctx, `INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref, binding_version, owner,
		execution_generation, proof, obligation_id, supersedes) VALUES ($1, $2, $3, $4, $5, 1, $6, $7, $8, $9, $10)`,
		uuid.New(), t.runnerID, t.runID, string(kind), t.ref, owner, generation, proof, t.obligationID, supersedes); err != nil {
		return fmt.Errorf("store: queue runner retirement %s: %w", kind, err)
	}
	return nil
}

// queueRetireTx queues t's retire_run with a receipt read now and records the
// org's runner.retire row for it in the same transaction.
func queueRetireTx(ctx context.Context, tx pgx.Tx, owner string, t runnerRetirement, supersedes *uuid.UUID,
	prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) error {
	receipt, err := retirementReceiptTx(ctx, tx, t)
	if err != nil {
		return err
	}
	proof, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err := insertRetirementActionTx(ctx, tx, owner, t, types.RunnerActionRetire, proof, supersedes); err != nil {
		return err
	}
	return recordRetirementAuditTx(ctx, tx, t.runID, "success", map[string]any{"runner_id": t.runnerID, "obligation_id": t.obligationID,
		"receipt_id": receipt.ID, "output_end": receipt.OutputEnd, "no_output": receipt.NoOutput, "spool_seq": receipt.Spool.Seq}, prepare)
}

// retirementReceiptTx is the settlement the org attests for t: the snapshot's
// execution identity, the stored exit's hash ("" when none was staged), the
// accepted output end or no_output, and the spool cursor's accepted prefix.
func retirementReceiptTx(ctx context.Context, tx pgx.Tx, t runnerRetirement) (types.RunnerRetirement, error) {
	runID := t.runID
	r := types.RunnerRetirement{ID: uuid.New(), Generation: t.generation, ExecID: t.execID}
	err := tx.QueryRow(ctx, `SELECT payload_hash FROM runner_execution_exits WHERE run_id = $1 AND generation = $2`, runID, t.generation).Scan(&r.ExitHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return r, err
	}
	var capture bool
	var end int64
	if err := tx.QueryRow(ctx, `SELECT capture, accepted_end FROM runner_output_watermarks WHERE run_id = $1`, runID).Scan(&capture, &end); err != nil {
		return r, fmt.Errorf("store: read runner output watermark: %w", err)
	}
	if capture {
		r.OutputEnd = end
	} else {
		r.NoOutput = true
	}
	var hashes []byte
	var overflow string
	if err := tx.QueryRow(ctx, `SELECT spool_id, last_seq, recent_hashes, overflow_end::text FROM runner_spool_cursors WHERE run_id = $1 AND runner_id = $2`,
		runID, t.runnerID).Scan(&r.Spool.SpoolID, &r.Spool.Seq, &hashes, &overflow); err != nil {
		return r, fmt.Errorf("store: read runner spool cursor: %w", err)
	}
	if r.Spool.ReportedDrops, err = strconv.ParseUint(overflow, 10, 64); err != nil {
		return r, err
	}
	if r.Spool.Seq > 0 {
		var recent map[string]string
		if err := json.Unmarshal(hashes, &recent); err != nil {
			return r, err
		}
		if r.Spool.Hash = recent[strconv.FormatInt(r.Spool.Seq, 10)]; r.Spool.Hash == "" {
			return r, errors.New("store: runner spool cursor lost its last hash")
		}
	}
	return r, nil
}

// closeRetirementTx closes an open snapshot; a closed one is terminal.
func closeRetirementTx(ctx context.Context, tx pgx.Tx, runID uuid.UUID, state, reason string) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE runner_retirements SET state = $2, reason = $3, settled_at = clock_timestamp()
		WHERE run_id = $1 AND state IN ('kill', 'retire')`, runID, state, reason)
	if err != nil {
		return false, fmt.Errorf("store: close runner retirement: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// abandonRetirementTx closes t unsettled: its unsettled actions stop fencing
// the run (settlement 'abandoned'), the runner keeps the output, and the
// runner.retire failure row commits with it.
func abandonRetirementTx(ctx context.Context, tx pgx.Tx, t runnerRetirement, reason string,
	prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) error {
	closed, err := closeRetirementTx(ctx, tx, t.runID, "abandoned", reason)
	if err != nil || !closed {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE runner_pending_actions SET settled_at = clock_timestamp(), settlement = 'abandoned'
		WHERE obligation_id = $1 AND settled_at IS NULL AND (applied_at IS NULL OR outcome = 'deferred')`, t.obligationID); err != nil {
		return fmt.Errorf("store: settle abandoned retirement: %w", err)
	}
	return recordRetirementAuditTx(ctx, tx, t.runID, "failure", map[string]any{"runner_id": t.runnerID, "obligation_id": t.obligationID, "reason": reason}, prepare)
}

func recordRetirementAuditTx(ctx context.Context, tx pgx.Tx, runID uuid.UUID, outcome string, data map[string]any,
	prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) error {
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return err
	}
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	ev := types.AuditEvent{ID: uuid.New(), RunID: &runID, ActorType: types.ActorSystem, Actor: "wardynd", Action: "runner.retire",
		Target: runID.String(), Outcome: outcome, Data: b}
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&ev.Time); err != nil {
		return err
	}
	if ev, err = prepare(ctx, ev); err != nil {
		return err
	}
	return InsertAuditEventTx(ctx, tx, &ev)
}

// AbandonRevokedRunnerRetirements closes, unsettled and audited, the open
// retirements of runners that were revoked or are no longer claimed. The
// revoke already settled their actions; the runner keeps the output.
func (s PG) AbandonRevokedRunnerRetirements(ctx context.Context, prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (int, error) {
	if prepare == nil {
		return 0, ErrRunnerActionRefused
	}
	tx, err := beginRunnerActionTx(ctx, s)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	rows, err := collect(ctx, tx, "list", "revoked runner retirements", `SELECT t.run_id, t.runner_id, t.obligation_id FROM runner_retirements t
		JOIN runners rn ON rn.id = t.runner_id WHERE t.state IN ('kill', 'retire') AND (rn.revoked_at IS NOT NULL OR rn.state <> 'claimed')
		ORDER BY t.run_id LIMIT 100 FOR UPDATE OF t SKIP LOCKED`, nil, func(row pgx.Row) (runnerRetirement, error) {
		var t runnerRetirement
		return t, row.Scan(&t.runID, &t.runnerID, &t.obligationID)
	})
	if err != nil {
		return 0, err
	}
	for _, t := range rows {
		if err := abandonRetirementTx(ctx, tx, t, RetirementRunnerRevoked, prepare); err != nil {
			return 0, err
		}
	}
	return len(rows), tx.Commit(ctx)
}

// retirementBindsResultTx is the binding a kill or retire_run result is held
// to once the run's ref was cleared: the run's snapshot, looked up by run and
// runner (never by ref alone), must be open in the action's phase and carry
// its obligation, ref and generation.
func retirementBindsResultTx(ctx context.Context, tx pgx.Tx, a types.RunnerPendingAction) (bool, error) {
	phase := retirementPhase(a.Kind)
	if phase == "" {
		return false, nil
	}
	t, err := readRetirementTx(ctx, tx, a.RunID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return t.runnerID == a.RunnerID && t.obligationID == a.ObligationID && t.state == phase && t.ref == a.Ref && t.generation == a.Generation, nil
}

// settleRetirementResultTx applies a recorded result to its retirement
// obligation, if it has one: a success settles the phase's earlier deferred
// actions ('chain'), and a retire_run success settles the obligation; a
// refusal in either phase closes it as refused, with no successor.
func settleRetirementResultTx(ctx context.Context, tx pgx.Tx, a types.RunnerPendingAction, outcome string) error {
	if retirementPhase(a.Kind) == "" || a.Asserted {
		return nil
	}
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM runner_retirements WHERE run_id = $1 AND obligation_id = $2 FOR UPDATE`, a.RunID, a.ObligationID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: read runner retirement: %w", err)
	}
	settle := func(sameKind bool) error {
		_, err := tx.Exec(ctx, `UPDATE runner_pending_actions SET settled_at = clock_timestamp(), settlement = 'chain', settled_by = $2
			WHERE obligation_id = $1 AND id <> $2 AND settled_at IS NULL AND outcome = 'deferred' AND (NOT $3 OR kind = $4)`,
			a.ObligationID, a.ID, sameKind, string(a.Kind))
		return err
	}
	switch outcome {
	case types.RunnerActionSuccess:
		if err := settle(true); err != nil {
			return err
		}
		if a.Kind == types.RunnerActionRetire {
			_, err = closeRetirementTx(ctx, tx, a.RunID, "settled", "")
		}
	case types.RunnerActionRefused:
		if err := settle(false); err != nil {
			return err
		}
		_, err = closeRetirementTx(ctx, tx, a.RunID, "refused", RetirementRunnerRefused)
	}
	return err
}
