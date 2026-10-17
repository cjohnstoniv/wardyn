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
		equalPtr(a.Recovery, b.Recovery) && equalPtr(a.Quarantine, b.Quarantine) && equalPtr(a.Retirement, b.Retirement)
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

// RunHeldContainedOnRunner reports whether the run's runner holds it contained: a kill, end, stop_proxy
// or quarantine was queued for it and no later recover_containment succeeded. Such a runner refuses any
// lease for the run, and a refused lease ends its session.
func (s PG) RunHeldContainedOnRunner(ctx context.Context, runID uuid.UUID) (bool, error) {
	var held bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runner_pending_actions c WHERE c.run_id = $1
		AND c.kind IN ('kill', 'end', 'stop_proxy', 'quarantine_run')
		AND NOT EXISTS (SELECT 1 FROM runner_pending_actions r WHERE r.run_id = c.run_id AND r.kind = 'recover_containment'
			AND r.outcome = 'success' AND r.queued_at > c.queued_at))`, runID).Scan(&held)
	if err != nil {
		return false, fmt.Errorf("store: check runner containment: %w", err)
	}
	return held, nil
}

// RecordRunnerActionResult records an authenticated action_result: runnerID is
// the stream's stored identity and runID the event's run; both must be the
// action's own. The outcomes are success, deferred and refused; only refused
// carries a reason, and its first receipt closes the action and writes
// runner.action.refuse in the same transaction. observedAt is the
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
	if outcome == types.RunnerActionRefused {
		if err := recordRunnerActionRefusalTx(ctx, tx, a, &runID, reason, prepare); err != nil {
			return types.RunnerPendingAction{}, false, err
		}
	}
	if err := settleRetirementResultTx(ctx, tx, a, outcome); err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	// A run that has since left the kept loss (killed, ended) has its own
	// teardown record; the receipt still settles this exact action.
	if outcome == types.RunnerActionSuccess && a.Kind == types.RunnerActionStopProxy && keptIncarnation(run, a.LostAt) {
		proof := RunnerContainmentProof{
			RunnerContainment: RunnerContainment{RunnerID: runnerID, RunID: runID, Generation: a.Generation, Ref: a.Ref, LostAt: *a.LostAt},
			ActionID:          a.ID, ObservedAt: observedAt,
		}
		if err := s.RecordRunnerContainmentTx(ctx, tx, proof, prepare); err != nil {
			return types.RunnerPendingAction{}, false, err
		}
	}
	return a, true, tx.Commit(ctx)
}
