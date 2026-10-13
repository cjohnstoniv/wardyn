// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// An asserted subject is a run a claimed runner reports that the org has no
// row for. It is contained, never trusted: a kill and then a quarantine_run
// bound to the runner-asserted run and ref, with no owner and no execution
// generation. The runner-asserted generation travels only inside the
// quarantine proof, flagged asserted. Nothing is acknowledged or retired.

// assertedSubjectTx runs inside page intake after the runner row was locked
// FOR SHARE as claimed and nonrevoked. It reports whether the inventory needs
// reconciliation and whether the runner's asserted-subject cap held it.
func assertedSubjectTx(ctx context.Context, tx pgx.Tx, runnerID uuid.UUID, row runnerwire.RunState) (needs, capped bool, err error) {
	local, ok := strings.CutPrefix(row.Ref, "runner:"+runnerID.String()+"/")
	if !ok || local == "" {
		return false, false, ErrRunnerExecutionRefused
	}
	var roots int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM runner_pending_actions
		WHERE runner_id = $1 AND subject = 'asserted' AND asserted_run_id = $2 AND supersedes IS NULL`, runnerID, row.RunID).Scan(&roots); err != nil {
		return false, false, fmt.Errorf("store: read asserted subject: %w", err)
	}
	// A reported subject that already has its containment rows is still
	// awaiting them (a quarantined run leaves the runner's inventory), so the
	// rows are reused and nothing grows.
	if roots > 0 {
		return true, false, nil
	}
	var subjects int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT DISTINCT asserted_run_id, ref FROM runner_pending_actions
		WHERE runner_id = $1 AND subject = 'asserted') s`, runnerID).Scan(&subjects); err != nil {
		return false, false, fmt.Errorf("store: count asserted subjects: %w", err)
	}
	if subjects >= types.RunnerAssertedSubjectCap {
		return true, true, nil
	}
	proof, err := json.Marshal(types.RunnerQuarantine{ID: uuid.New(), Generation: row.Generation, Asserted: true})
	if err != nil {
		return false, false, err
	}
	for _, a := range []struct {
		kind  types.RunnerActionKind
		proof []byte
	}{{types.RunnerActionKill, nil}, {types.RunnerActionQuarantine, proof}} {
		if _, err := tx.Exec(ctx, `INSERT INTO runner_pending_actions (id, runner_id, run_id, subject, asserted_run_id, kind, ref, binding_version, owner, proof)
			VALUES ($1, $2, NULL, 'asserted', $3, $4, $5, 1, '', $6)`, uuid.New(), runnerID, row.RunID, string(a.kind), row.Ref, a.proof); err != nil {
			return false, false, fmt.Errorf("store: queue asserted containment: %w", err)
		}
	}
	return true, false, nil
}

// recordAssertedActionResultTx records the receipt of an asserted subject's
// action without any run lock: there is no run row. The order is runner FOR
// SHARE, then the action. A success settles the earlier deferred actions of
// its obligation ('chain'); nothing else in the org changes.
func recordAssertedActionResultTx(ctx context.Context, tx pgx.Tx, runnerID, runID, actionID uuid.UUID, outcome string, observedAt time.Time) (types.RunnerPendingAction, bool, error) {
	var claimed bool
	err := tx.QueryRow(ctx, `SELECT true FROM runners WHERE id = $1 AND state = 'claimed' AND revoked_at IS NULL FOR SHARE`, runnerID).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunnerPendingAction{}, false, ErrNotFound
	}
	if err != nil {
		return types.RunnerPendingAction{}, false, fmt.Errorf("store: lock runner: %w", err)
	}
	a, err := scanAction(tx.QueryRow(ctx, `SELECT `+actionCols+` FROM runner_pending_actions WHERE id = $1 FOR UPDATE`, actionID))
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	if !a.Bound || !a.Asserted || a.RunnerID != runnerID || a.RunID != runID || a.Owner != "" {
		return types.RunnerPendingAction{}, false, ErrRunnerActionRefused
	}
	if a.AppliedAt != nil {
		if a.Outcome != outcome || !a.ObservedAt.Equal(observedAt) {
			return types.RunnerPendingAction{}, false, ErrConflict
		}
		return a, false, tx.Commit(ctx)
	}
	a, err = scanAction(tx.QueryRow(ctx, `UPDATE runner_pending_actions SET applied_at = clock_timestamp(), outcome = $2, observed_at = $3
		WHERE id = $1 AND applied_at IS NULL RETURNING `+actionCols, actionID, outcome, observedAt))
	if err != nil {
		return types.RunnerPendingAction{}, false, err
	}
	if outcome == types.RunnerActionSuccess {
		if _, err := tx.Exec(ctx, `UPDATE runner_pending_actions SET settled_at = clock_timestamp(), settlement = 'chain', settled_by = $2
			WHERE obligation_id = $1 AND id <> $2 AND settled_at IS NULL`, a.ObligationID, a.ID); err != nil {
			return types.RunnerPendingAction{}, false, fmt.Errorf("store: settle asserted obligation: %w", err)
		}
	}
	return a, true, tx.Commit(ctx)
}

// ReissueDeferredAssertedRunnerActions is the reconciler for an asserted
// subject's deferred kill or quarantine: a deferred receipt is final for its
// action, so the obligation continues in a fresh successor with the identical
// binding and proof. It runs off the read loop as a fresh session is admitted
// (before its inventory begins), and only when the runner's previous session
// completed its inventory, so each obligation gets at most one successor per
// completed inventory. It returns how many successors it queued.
func (s PG) ReissueDeferredAssertedRunnerActions(ctx context.Context, runnerID uuid.UUID, sessionID string) (int, error) {
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
	tips, err := collect(ctx, tx, "list", "deferred asserted actions", `SELECT `+actionCols+` FROM runner_pending_actions
		WHERE runner_id = $1 AND subject = 'asserted' AND outcome = 'deferred' AND settled_at IS NULL AND NOT superseded
		ORDER BY queue_seq LIMIT $2 FOR UPDATE`, []any{runnerID, runnerActionPageLimit}, scanAction)
	if err != nil {
		return 0, err
	}
	for _, tip := range tips {
		var proof []byte
		if tip.Quarantine != nil {
			if proof, err = json.Marshal(tip.Quarantine); err != nil {
				return 0, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE runner_pending_actions SET superseded = true WHERE id = $1`, tip.ID); err != nil {
			return 0, fmt.Errorf("store: supersede deferred asserted action: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO runner_pending_actions (id, runner_id, run_id, subject, asserted_run_id, kind, ref, binding_version, owner, proof, obligation_id, supersedes)
			VALUES ($1, $2, NULL, 'asserted', $3, $4, $5, 1, '', $6, $7, $8)`, uuid.New(), runnerID, tip.RunID, string(tip.Kind), tip.Ref, proof, tip.ObligationID, tip.ID); err != nil {
			return 0, fmt.Errorf("store: queue asserted successor: %w", err)
		}
	}
	return len(tips), tx.Commit(ctx)
}

// PurgeAssertedRunnerSubjects deletes every action row of an asserted subject
// whose quarantine succeeded more than RunnerAssertedEvidenceTTL ago. The
// runner deletes its own evidence for that run on the same clock.
func (s PG) PurgeAssertedRunnerSubjects(ctx context.Context) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM runner_pending_actions d USING (
			SELECT DISTINCT runner_id, asserted_run_id FROM runner_pending_actions
			WHERE subject = 'asserted' AND kind = 'quarantine_run' AND outcome = 'success'
			AND applied_at < clock_timestamp() - $1::interval) q
		WHERE d.subject = 'asserted' AND d.runner_id = q.runner_id AND d.asserted_run_id = q.asserted_run_id`,
		types.RunnerAssertedEvidenceTTL.String())
	if err != nil {
		return 0, fmt.Errorf("store: purge asserted runner subjects: %w", err)
	}
	return tag.RowsAffected(), nil
}
