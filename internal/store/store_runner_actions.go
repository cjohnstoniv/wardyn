// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrRunnerActionRefused covers a pending action or action_result whose runner,
// owner, run, ref, generation or loss does not match the org's current record.
var ErrRunnerActionRefused = errors.New("runner pending action refused")

// runnerActionPageLimit bounds one PENDING payload. Applied rows leave the open
// set, so each fresh session reads the next oldest page.
const runnerActionPageLimit = 100

const actionCols = `id, runner_id, COALESCE(run_id, asserted_run_id), kind, ref, queued_at, applied_at, outcome, observed_at,
	binding_version, owner, execution_generation, containment_lost_at, proof, subject = 'asserted', COALESCE(obligation_id, id)`

// runnerActionUnsettledSQL is the run fence: an open action, or a deferred
// receipt, whose obligation nothing has settled. $1 is the run.
const runnerActionUnsettledSQL = `SELECT EXISTS (SELECT 1 FROM runner_pending_actions
	WHERE run_id = $1 AND (applied_at IS NULL OR outcome = 'deferred') AND settled_at IS NULL)`

func scanAction(row pgx.Row) (types.RunnerPendingAction, error) {
	var a types.RunnerPendingAction
	var kind string
	var version int16
	var generation *uuid.UUID
	var proof []byte
	err := row.Scan(&a.ID, &a.RunnerID, &a.RunID, &kind, &a.Ref, &a.QueuedAt, &a.AppliedAt, &a.Outcome, &a.ObservedAt,
		&version, &a.Owner, &generation, &a.LostAt, &proof, &a.Asserted, &a.ObligationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunnerPendingAction{}, ErrNotFound
	}
	if err != nil {
		return types.RunnerPendingAction{}, fmt.Errorf("store: scan runner action: %w", err)
	}
	a.Kind = types.RunnerActionKind(kind)
	a.Bound = version == 1
	if generation != nil {
		a.Generation = *generation
	}
	return a, decodeRunnerActionProof(&a, proof)
}

// decodeRunnerActionProof reads the one proof the kind names, strictly.
func decodeRunnerActionProof(a *types.RunnerPendingAction, raw []byte) error {
	if raw == nil {
		return nil
	}
	var dst any
	switch a.Kind {
	case types.RunnerActionStopProxy:
		// A stop_proxy's proof is the node record it carries; the runner decodes it strictly.
		a.NodeRecord = append(json.RawMessage(nil), raw...)
		return nil
	case types.RunnerActionRecoverMutation, types.RunnerActionRecoverContainment:
		a.Recovery = new(types.RunnerMutationFailure)
		dst = a.Recovery
	case types.RunnerActionQuarantine:
		a.Quarantine = new(types.RunnerQuarantine)
		dst = a.Quarantine
	case types.RunnerActionRetire:
		a.Retirement = new(types.RunnerRetirement)
		dst = a.Retirement
	default:
		return ErrRunnerActionRefused
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("store: decode runner action proof: %w", err)
	}
	return nil
}

func equalPtr[T comparable](a, b *T) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// sameRunnerAction compares every immutable binding except the action ID.
func sameRunnerAction(a, b types.RunnerPendingAction) bool {
	lost := (a.LostAt == nil) == (b.LostAt == nil) && (a.LostAt == nil || a.LostAt.Equal(*b.LostAt))
	return a.Bound && b.Bound && a.Asserted == b.Asserted && a.RunnerID == b.RunnerID && a.RunID == b.RunID && a.Kind == b.Kind &&
		a.Ref == b.Ref && a.Owner == b.Owner && a.Generation == b.Generation && lost &&
		equalPtr(a.Recovery, b.Recovery) && equalPtr(a.Quarantine, b.Quarantine) && equalPtr(a.Retirement, b.Retirement) &&
		SameJSON(a.NodeRecord, b.NodeRecord)
}

// lockRunnerActionRun takes the shared order runner FOR SHARE → run FOR UPDATE
// → execution FOR UPDATE. The action's owner is the run's creator: on a
// person's runner that must be the stored owner, and an org runner (owner
// empty) binds to whoever created the run. A runner that is not claimed, or a
// run that is gone, is ErrNotFound; a run bound to another runner, owner or
// placement is ErrRunnerActionRefused.
func lockRunnerActionRun(ctx context.Context, tx pgx.Tx, runnerID, runID uuid.UUID) (string, types.AgentRun, uuid.UUID, error) {
	var owner, ownerKind string
	err := tx.QueryRow(ctx, `SELECT owner, owner_kind FROM runners WHERE id = $1 AND state = 'claimed' AND revoked_at IS NULL
		AND (owner <> '' OR owner_kind = 'org') FOR SHARE`, runnerID).Scan(&owner, &ownerKind)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", types.AgentRun{}, uuid.Nil, ErrNotFound
	}
	if err != nil {
		return "", types.AgentRun{}, uuid.Nil, fmt.Errorf("store: lock runner: %w", err)
	}
	run, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM agent_runs WHERE id = $1 FOR UPDATE`, runID))
	if err != nil {
		return "", types.AgentRun{}, uuid.Nil, err
	}
	if run.RunnerID == nil || *run.RunnerID != runnerID || run.CreatedBy == "" || !runnerReportsOn(ownerKind, owner, run.CreatedBy) ||
		run.Placement != types.PlacementLocal {
		return "", types.AgentRun{}, uuid.Nil, ErrRunnerActionRefused
	}
	owner = run.CreatedBy
	current, err := readRunnerExecution(ctx, tx, runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return owner, run, uuid.Nil, nil
	}
	if err != nil {
		return "", types.AgentRun{}, uuid.Nil, err
	}
	if current.RunnerID != runnerID {
		return "", types.AgentRun{}, uuid.Nil, ErrRunnerActionRefused
	}
	return owner, run, current.Generation, nil
}

// keptIncarnation reports whether run is still the exact kept outage loss lostAt names.
func keptIncarnation(run types.AgentRun, lostAt *time.Time) bool {
	return lostAt != nil && run.State == types.RunRunning && run.LostAt != nil && run.LostReason == types.LostOutage &&
		run.LostAt.Equal(*lostAt)
}

func beginRunnerActionTx(ctx context.Context, s PG) (pgx.Tx, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("store: begin runner action: %w", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '2s'`); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// QueueRunnerAction durably queues a containment action (kill, end, stop_proxy)
// for a's claimed runner, bound to the identity the org holds at commit: the
// runner's owner, the run's exact ref and the expected execution generation
// (uuid.Nil asserts none was issued), and for stop_proxy the exact kept outage
// loss in LostAt. Any mismatch is ErrRunnerActionRefused and writes nothing.
// A replayed action ID, an open action of the same kind for the run, or a
// deferred stop_proxy for the same loss is returned with queued=false only
// when every binding matches; otherwise
// ErrConflict. A runner that is not claimed, or a run that is gone, is
// ErrNotFound: the runner row is locked FOR SHARE, so a queue meeting a revoke
// waits for it and then refuses. Recovery, quarantine and retirement need
// explicit org authority that this queue does not establish, so they refuse.
func (s PG) QueueRunnerAction(ctx context.Context, a types.RunnerPendingAction) (types.RunnerPendingAction, bool, error) {
	tx, err := beginRunnerActionTx(ctx, s)
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	out, queued, err := queueRunnerActionTx(ctx, tx, a)
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	return out, queued, tx.Commit(ctx)
}

// QueueRunnerTeardown queues the org's own offline teardown of ref (the
// remote substrate's Queue seam): it binds the run's identity as the org holds
// it under the same locks, then queues through the exact-binding path. A
// stop_proxy binds the current kept outage loss and refuses without one.
func (s PG) QueueRunnerTeardown(ctx context.Context, runnerID uuid.UUID, kind types.RunnerActionKind, ref string) (types.RunnerPendingAction, bool, error) {
	tx, err := beginRunnerActionTx(ctx, s)
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	var runID uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id FROM agent_runs WHERE runner_id = $1 AND sandbox_ref = $2 AND sandbox_ref <> ''`, runnerID, ref).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunnerPendingAction{}, false, ErrNotFound
	}
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	_, run, generation, err := lockRunnerActionRun(ctx, tx, runnerID, runID)
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	a := types.RunnerPendingAction{ID: uuid.New(), RunnerID: runnerID, RunID: runID, Kind: kind, Ref: ref, Generation: generation}
	if kind == types.RunnerActionStopProxy {
		a.LostAt = run.LostAt
	}
	out, queued, err := queueRunnerActionTx(ctx, tx, a)
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	return out, queued, tx.Commit(ctx)
}

// queueRunnerActionTx is QueueRunnerAction inside the caller's READ COMMITTED
// transaction, which must not hold locks out of the runner → run → execution order.
func queueRunnerActionTx(ctx context.Context, tx pgx.Tx, a types.RunnerPendingAction) (types.RunnerPendingAction, bool, error) {
	switch a.Kind {
	case types.RunnerActionKill, types.RunnerActionEnd, types.RunnerActionStopProxy:
	default:
		return types.RunnerPendingAction{}, false, ErrRunnerActionRefused
	}
	if a.ID == uuid.Nil || a.Recovery != nil || a.Quarantine != nil || a.Retirement != nil ||
		!strings.HasPrefix(a.Ref, "runner:"+a.RunnerID.String()+"/") || (a.Kind == types.RunnerActionStopProxy) != (a.LostAt != nil) {
		return types.RunnerPendingAction{}, false, ErrRunnerActionRefused
	}
	if a.LostAt != nil {
		lost := a.LostAt.UTC().Truncate(time.Microsecond)
		a.LostAt = &lost
	}
	owner, run, generation, err := lockRunnerActionRun(ctx, tx, a.RunnerID, a.RunID)
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	if run.SandboxRef != a.Ref || generation != a.Generation || (a.LostAt != nil && !keptIncarnation(run, a.LostAt)) {
		return types.RunnerPendingAction{}, false, ErrRunnerActionRefused
	}
	a.Owner, a.Bound = owner, true
	// A deferred stop_proxy still carries its loss's obligation: a fresh one
	// would be a second, unchained issuer for the same loss, so it is returned.
	existing, err := scanAction(tx.QueryRow(ctx, `SELECT `+actionCols+` FROM runner_pending_actions
		WHERE id = $1 OR (run_id = $2 AND kind = $3 AND settled_at IS NULL
			AND (applied_at IS NULL OR (outcome = 'deferred' AND containment_lost_at = $4)))
		ORDER BY id = $1 DESC, applied_at IS NULL DESC LIMIT 1 FOR UPDATE`, a.ID, a.RunID, string(a.Kind), a.LostAt))
	if err == nil {
		if !sameRunnerAction(existing, a) {
			return types.RunnerPendingAction{}, false, ErrConflict
		}
		return existing, false, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return types.RunnerPendingAction{}, false, err
	}
	var generationArg *uuid.UUID
	if a.Generation != uuid.Nil {
		generationArg = &a.Generation
	}
	out, err := scanAction(tx.QueryRow(ctx, `
		INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref, binding_version, owner, execution_generation, containment_lost_at)
		VALUES ($1,$2,$3,$4,$5,1,$6,$7,$8)
		RETURNING `+actionCols, a.ID, a.RunnerID, a.RunID, string(a.Kind), a.Ref, owner, generationArg, a.LostAt))
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	return out, true, nil
}

// ListPendingRunnerActions is the runner's PENDING payload: at most one page of
// its oldest open bound actions, and whether more remain. A row from before the
// binding is never delivered; it keeps fencing its run instead.
func (s PG) ListPendingRunnerActions(ctx context.Context, runnerID uuid.UUID) ([]types.RunnerPendingAction, bool, error) {
	const q = `SELECT ` + actionCols + ` FROM runner_pending_actions
		WHERE runner_id = $1 AND applied_at IS NULL AND settled_at IS NULL AND binding_version = 1 ORDER BY queue_seq LIMIT $2`
	page, err := collect(ctx, s.Pool, "list", "runner pending actions", q, []any{runnerID, runnerActionPageLimit + 1}, scanAction)
	if err != nil || len(page) <= runnerActionPageLimit {
		return page, false, err
	}
	return page[:runnerActionPageLimit], true, nil
}

// RunHasPendingRunnerAction is the fence: a run with an open action, or a
// deferred one, gets no relay, call, generation or revival.
func (s PG) RunHasPendingRunnerAction(ctx context.Context, runID uuid.UUID) (bool, error) {
	var has bool
	if err := s.Pool.QueryRow(ctx, runnerActionUnsettledSQL, runID).Scan(&has); err != nil {
		return false, fmt.Errorf("store: check pending runner actions: %w", err)
	}
	return has, nil
}

// LastRunnerTeardown reports the runner's latest answer to a queued end or kill
// of runID at ref: its kind and outcome, or answered false when it has answered none.
func (s PG) LastRunnerTeardown(ctx context.Context, runID uuid.UUID, ref string) (kind types.RunnerActionKind, outcome string, answered bool, err error) {
	err = s.Pool.QueryRow(ctx, `SELECT kind, outcome FROM runner_pending_actions
		WHERE run_id = $1 AND ref = $2 AND kind IN ('end', 'kill') AND applied_at IS NOT NULL
		ORDER BY applied_at DESC, id DESC LIMIT 1`, runID, ref).Scan(&kind, &outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("store: read last runner teardown: %w", err)
	}
	return kind, outcome, true, nil
}

// RunHeldContainedOnRunner reports whether the run's runner holds it contained: a kill, end, stop_proxy
// or quarantine was queued for it and no later recover_containment succeeded, or the runner reported it
// containment pending. Such a runner refuses any lease for the run, and a refused
// lease ends its session.
func (s PG) RunHeldContainedOnRunner(ctx context.Context, runID uuid.UUID) (bool, error) {
	var held bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runner_pending_actions c WHERE c.run_id = $1
		AND c.kind IN ('kill', 'end', 'stop_proxy', 'quarantine_run')
		AND NOT EXISTS (SELECT 1 FROM runner_pending_actions r WHERE r.run_id = c.run_id AND r.kind = 'recover_containment'
			AND r.outcome = 'success' AND r.queued_at > c.queued_at))
		OR EXISTS (SELECT 1 FROM run_containment_pending WHERE run_id = $1)`, runID).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("store: check runner containment: %w", err)
	}
	return held, nil
}

// RecordRunnerActionResult records an authenticated action_result: runnerID is
// the stream's stored identity and runID the event's run; both must be the
// action's own. The outcomes are success, deferred and refused; only refused
// carries a reason, and its first receipt closes the action and writes
// runner.action.refuse in the same transaction, and a first success writes
// runner.action.apply. observedAt is the
// runner's assertion and applied_at is the org clock. The first receipt
// rechecks the owner, ref and execution generation the action was bound to;
// a stop_proxy success on the still-kept loss records late containment in the
// same transaction, so the receipt and its audit commit or roll back together.
// An identical repeat returns first=false with no further effect; a changed
// one is ErrConflict.
func (s PG) RecordRunnerActionResult(ctx context.Context, runnerID, runID, actionID uuid.UUID, outcome, reason string, observedAt time.Time,
	prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (types.RunnerPendingAction, bool, error) {
	if !validRunnerActionResult(runnerID, runID, actionID, outcome, reason, observedAt) || prepare == nil {
		return types.RunnerPendingAction{}, false, ErrRunnerActionRefused
	}
	observedAt = observedAt.UTC().Truncate(time.Microsecond)
	tx, err := beginRunnerActionTx(ctx, s)
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	// The subject is immutable, so reading it before any lock is safe; an
	// asserted subject has no run row to lock.
	var asserted bool
	if err := tx.QueryRow(ctx, `SELECT subject = 'asserted' FROM runner_pending_actions WHERE id = $1`, actionID).Scan(&asserted); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return types.RunnerPendingAction{}, false, err
	}
	if asserted {
		return recordAssertedActionResultTx(ctx, tx, runnerID, runID, actionID, outcome, reason, observedAt, prepare)
	}
	owner, run, generation, err := lockRunnerActionRun(ctx, tx, runnerID, runID)
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	a, err := scanAction(tx.QueryRow(ctx, `SELECT `+actionCols+` FROM runner_pending_actions WHERE id = $1 FOR UPDATE`, actionID))
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	if !a.Bound || a.Asserted || a.RunnerID != runnerID || a.RunID != runID || a.Owner != owner {
		return types.RunnerPendingAction{}, false, ErrRunnerActionRefused
	}
	if a.AppliedAt != nil {
		if a.Outcome != outcome || !a.ObservedAt.Equal(observedAt) {
			return types.RunnerPendingAction{}, false, ErrConflict
		}
		return a, false, tx.Commit(ctx)
	}
	// A refusal changed nothing on the runner, so it closes the action even
	// when the run has since moved on. A retirement's kill or retire_run is
	// held instead to the binding snapshot its run's cleared ref left.
	if outcome != types.RunnerActionRefused && (run.SandboxRef != a.Ref || generation != a.Generation) {
		bound, err := retirementBindsResultTx(ctx, tx, a)
		if err != nil {
			return types.RunnerPendingAction{}, false, err
		}
		if !bound {
			return types.RunnerPendingAction{}, false, ErrRunnerActionRefused
		}
	}
	a, err = scanAction(tx.QueryRow(ctx, `UPDATE runner_pending_actions SET applied_at = clock_timestamp(), outcome = $2, observed_at = $3
		WHERE id = $1 AND applied_at IS NULL RETURNING `+actionCols, actionID, outcome, observedAt))
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	if err := s.recordRunnerActionReceiptTx(ctx, tx, a, run, reason, observedAt, prepare); err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	return a, true, tx.Commit(ctx)
}

// recordRunnerActionReceiptTx writes what a first receipt of a bound action
// records, in RecordRunnerActionResult's transaction: the refuse or apply
// audit row, the retirement it settles, and a stop_proxy's late containment.
func (s PG) recordRunnerActionReceiptTx(ctx context.Context, tx pgx.Tx, a types.RunnerPendingAction, run types.AgentRun, reason string, observedAt time.Time,
	prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) error {
	runID := a.RunID
	if a.Outcome == types.RunnerActionRefused {
		if err := recordRunnerActionRefusalTx(ctx, tx, a, &runID, reason, prepare); err != nil {
			return err
		}
	}
	if a.Outcome == types.RunnerActionSuccess {
		if err := recordRunnerActionApplyTx(ctx, tx, a, &runID, observedAt, prepare); err != nil {
			return err
		}
	}
	// A kill's success settles the deferred kills of its own obligation, the links
	// ReissueDeferredRunKills chained to it; a fresh kill has an obligation of its
	// own and settles none.
	if a.Outcome == types.RunnerActionSuccess && a.Kind == types.RunnerActionKill {
		if _, err := tx.Exec(ctx, `UPDATE runner_pending_actions SET settled_at = clock_timestamp(), settlement = 'chain', settled_by = $2
			WHERE obligation_id = $1 AND id <> $2 AND kind = 'kill' AND settled_at IS NULL AND outcome = 'deferred'`, a.ObligationID, a.ID); err != nil {
			return fmt.Errorf("store: settle deferred kills: %w", err)
		}
	}
	if err := settleRetirementResultTx(ctx, tx, a, a.Outcome); err != nil {
		return err
	}
	if err := settleRunNodeRecordTx(ctx, tx, a, a.Outcome, reason); err != nil {
		return err
	}
	// A run that has since left the kept loss (killed, ended) has its own
	// teardown record; the receipt still settles this exact action.
	if a.Outcome == types.RunnerActionSuccess && a.Kind == types.RunnerActionStopProxy && keptIncarnation(run, a.LostAt) {
		proof := RunnerContainmentProof{
			RunnerContainment: RunnerContainment{RunnerID: a.RunnerID, RunID: runID, Generation: a.Generation, Ref: a.Ref, LostAt: *a.LostAt},
			ActionID:          a.ID, ObservedAt: observedAt,
		}
		return s.RecordRunnerContainmentTx(ctx, tx, proof, prepare)
	}
	return nil
}

// ReissueDeferredRunKills is ReissueDeferredAssertedRunnerActions for a run's own
// kill: a deferred receipt is final for its action, so the kill continues in one
// successor per completed inventory of the runner's previous session, with the
// identical binding and obligation, until a success settles the chain. It returns
// how many successors it queued. Conservatively, it queues none for:
//   - a retirement's kill, whose retirement bounds its own successors;
//   - a run that no longer carries the kill's binding (its ref cleared, its
//     generation or owner moved on): the runner's receipt would be refused;
//   - a run with an open kill already, which is the one the runner will answer;
//   - a successor the runner refused: nothing in its refusal says a further kill
//     would be taken, so the deferred link stays the run's last word, as before.
func (s PG) ReissueDeferredRunKills(ctx context.Context, runnerID uuid.UUID, sessionID string) (int, error) {
	tx, err := beginRunnerActionTx(ctx, s)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	var claimed bool
	err = tx.QueryRow(ctx, `SELECT true FROM runners WHERE id = $1 AND state = 'claimed' AND revoked_at IS NULL FOR SHARE`, runnerID).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrRunnerExecutionRefused
	}
	if err != nil {
		return 0, err
	}
	var completed bool
	err = tx.QueryRow(ctx, `SELECT complete FROM runner_inventories WHERE runner_id = $1 AND session_id::text <> $2`, runnerID, sessionID).Scan(&completed)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !completed) {
		return 0, tx.Commit(ctx)
	}
	if err != nil {
		return 0, err
	}
	tips, err := collect(ctx, tx, "list", "deferred run kills", `SELECT `+actionCols+` FROM runner_pending_actions a
		WHERE runner_id = $1 AND subject = 'run' AND binding_version = 1 AND kind = 'kill' AND outcome = 'deferred'
		AND settled_at IS NULL AND NOT superseded
		AND NOT EXISTS (SELECT 1 FROM runner_retirements r WHERE r.obligation_id = a.obligation_id)
		ORDER BY queue_seq LIMIT $2`, []any{runnerID, runnerActionPageLimit}, scanAction)
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, tip := range tips {
		// The shared order: runner, run, then the action.
		owner, run, generation, err := lockRunnerActionRun(ctx, tx, runnerID, tip.RunID)
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrRunnerActionRefused) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if owner != tip.Owner || run.SandboxRef != tip.Ref || generation != tip.Generation {
			continue
		}
		var skip bool
		if err := tx.QueryRow(ctx, `SELECT a.superseded OR a.settled_at IS NOT NULL OR EXISTS (SELECT 1 FROM runner_pending_actions o
			WHERE o.run_id = a.run_id AND o.kind = 'kill' AND o.applied_at IS NULL AND o.settled_at IS NULL)
			FROM runner_pending_actions a WHERE a.id = $1 FOR UPDATE OF a`, tip.ID).Scan(&skip); err != nil {
			return 0, err
		}
		if skip {
			continue
		}
		if _, err := tx.Exec(ctx, `UPDATE runner_pending_actions SET superseded = true WHERE id = $1`, tip.ID); err != nil {
			return 0, fmt.Errorf("store: supersede deferred kill: %w", err)
		}
		var gen *uuid.UUID
		if tip.Generation != uuid.Nil {
			gen = &tip.Generation
		}
		if _, err := tx.Exec(ctx, `INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref, binding_version, owner,
			execution_generation, obligation_id, supersedes) VALUES ($1, $2, $3, 'kill', $4, 1, $5, $6, $7, $8)`,
			uuid.New(), runnerID, tip.RunID, tip.Ref, tip.Owner, gen, tip.ObligationID, tip.ID); err != nil {
			return 0, fmt.Errorf("store: queue kill successor: %w", err)
		}
		queued++
	}
	return queued, tx.Commit(ctx)
}
