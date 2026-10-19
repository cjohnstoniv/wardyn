// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// QueueRunnerContainmentRecovery queues recover_containment for a run its runner holds contained by a
// settled end or stop_proxy, for a revive that has already re-checked the run's owner and policy. Its proof
// is the marker the runner reported for the run on its current session, taken only when the organisation's
// own records carry every field of it: the marker is a bound end or stop_proxy of this run, applied after
// the last successful recovery, a success of that kind (completion evidence) answers it, it is contained,
// and its generation is the run's current one. A kill or quarantine ever queued for the run, a containment
// pending hold, any unsettled action other than a deferred recovery, or a report that is containment only, carries a mutation failure, a
// retirement or no sandbox, or a recovery the runner already refused for the same marker, is
// ErrRunnerActionRefused and writes nothing. An open recovery for the run is
// returned with queued=false; a deferred one is continued by the new one. The recovery grants nothing by itself: the run is leased again only by a
// later revive, above the runner's replay floor.
func (s PG) QueueRunnerContainmentRecovery(ctx context.Context, runnerID, runID uuid.UUID) (types.RunnerPendingAction, bool, error) {
	tx, err := beginRunnerActionTx(ctx, s)
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	owner, run, generation, err := lockRunnerActionRun(ctx, tx, runnerID, runID)
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	if run.State != types.RunRunning || run.SandboxRef == "" {
		return types.RunnerPendingAction{}, false, ErrRunnerActionRefused
	}
	existing, err := scanAction(tx.QueryRow(ctx, `SELECT `+actionCols+` FROM runner_pending_actions
		WHERE run_id = $1 AND kind = 'recover_containment' AND applied_at IS NULL AND settled_at IS NULL FOR UPDATE`, runID))
	if err == nil {
		return existing, false, tx.Commit(ctx)
	}
	if !errors.Is(err, ErrNotFound) {
		return types.RunnerPendingAction{}, false, err
	}
	var blocked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runner_pending_actions WHERE run_id = $1
			AND (kind IN ('kill', 'quarantine_run') OR ((applied_at IS NULL OR outcome = 'deferred') AND settled_at IS NULL
				AND NOT (kind = 'recover_containment' AND outcome = 'deferred'))))
		OR EXISTS (SELECT 1 FROM run_containment_pending WHERE run_id = $1)`, runID).Scan(&blocked); err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	if blocked {
		return types.RunnerPendingAction{}, false, ErrRunnerActionRefused
	}
	marker, err := reportedContainmentMarker(ctx, tx, runnerID, runID, generation)
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	var generationArg *uuid.UUID
	if generation != uuid.Nil {
		generationArg = &generation
	}
	proof, err := json.Marshal(marker)
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	// A runner that refused this exact proof once answers it the same way again. Order is queue_seq, the
	// delivery order: queued_at is each queuing transaction's start, which ties and can invert.
	var evidenced bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM runner_pending_actions c
		WHERE c.id = $1 AND c.run_id = $2 AND c.runner_id = $3 AND c.kind = $4 AND c.binding_version = 1 AND c.subject = 'run'
		AND c.applied_at IS NOT NULL AND c.execution_generation IS NOT DISTINCT FROM $5
		AND NOT EXISTS (SELECT 1 FROM runner_pending_actions r WHERE r.run_id = c.run_id AND r.kind = 'recover_containment'
			AND r.outcome = 'success' AND r.queue_seq > c.queue_seq)
		AND EXISTS (SELECT 1 FROM runner_pending_actions w WHERE w.run_id = c.run_id AND w.kind = c.kind
			AND w.outcome = 'success' AND w.queue_seq >= c.queue_seq)
		AND NOT EXISTS (SELECT 1 FROM runner_pending_actions f WHERE f.run_id = c.run_id AND f.kind = 'recover_containment'
			AND f.outcome = 'refused' AND f.proof = $6::jsonb))`,
		marker.ID, runID, runnerID, marker.Operation, generationArg, proof).Scan(&evidenced); err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	if !evidenced {
		return types.RunnerPendingAction{}, false, ErrRunnerActionRefused
	}
	// A deferred recovery (a mutation of the run was still in flight on the runner) is final for its own
	// action, so this one continues its obligation as its successor; the chain settles when one is answered.
	var tipID, obligation *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id, obligation_id FROM runner_pending_actions WHERE run_id = $1 AND kind = 'recover_containment'
		AND outcome = 'deferred' AND settled_at IS NULL AND NOT superseded ORDER BY queue_seq DESC LIMIT 1 FOR UPDATE`, runID).Scan(&tipID, &obligation)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return types.RunnerPendingAction{}, false, err
	}
	if tipID != nil {
		if _, err := tx.Exec(ctx, `UPDATE runner_pending_actions SET superseded = true WHERE id = $1`, *tipID); err != nil {
			return types.RunnerPendingAction{}, false, err
		}
	}
	a, err := scanAction(tx.QueryRow(ctx, `
		INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref, binding_version, owner, execution_generation, proof,
			obligation_id, supersedes)
		VALUES ($1,$2,$3,'recover_containment',$4,1,$5,$6,$7,$8,$9)
		RETURNING `+actionCols, uuid.New(), runnerID, runID, run.SandboxRef, owner, generationArg, proof, obligation, tipID))
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	return a, true, tx.Commit(ctx)
}

// reportedContainmentMarker is the containment marker the runner reported for the run on its current
// session, or ErrRunnerActionRefused when there is none or the report rules a recovery out: not contained,
// containment only, a mutation failure, a retirement, a containment pending hold, no sandbox, another
// generation, or an operation other than end or stop_proxy.
func reportedContainmentMarker(ctx context.Context, tx pgx.Tx, runnerID, runID, generation uuid.UUID) (*types.RunnerMutationFailure, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT assertion FROM runner_inventory_observations WHERE runner_id = $1 AND run_id = $2`, runnerID, runID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRunnerActionRefused
	}
	if err != nil {
		return nil, err
	}
	var row runnerwire.RunState
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, ErrRunnerActionRefused
	}
	marker := row.Containment
	if marker == nil || !marker.Contained || row.ContainmentOnly || row.MutationFailure != nil || row.Retirement != nil ||
		row.ContainmentPending != nil || !row.SandboxPresent || marker.Generation != generation || row.Generation != generation ||
		(marker.Operation != string(types.RunnerActionEnd) && marker.Operation != string(types.RunnerActionStopProxy)) {
		return nil, ErrRunnerActionRefused
	}
	return marker, nil
}
