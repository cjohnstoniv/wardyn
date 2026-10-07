// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// ErrRecordingOutputErased refuses a new copy of an erased recording while
// leaving independently captured stdout and pane snapshots alone.
var ErrRecordingOutputErased = errors.New("store: recording-derived output was erased")

// ErrRecordingOutputUncovered refuses a historical read whose masking manifest
// was fenced or removed before the recovered row could commit.
var ErrRecordingOutputUncovered = errors.New("store: recording output lost masking coverage")

// RecordingOutputClaim identifies one read of a committed recording. Generation
// invalidates reads predating a new upload; Token also fences stale-lease owners.
type RecordingOutputClaim struct {
	RunID      uuid.UUID
	Generation int64
	Token      uuid.UUID
}

func recordingOutputErased(ctx context.Context, tx pgx.Tx, runID uuid.UUID) error {
	var erased bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM run_output_recording_recovery WHERE run_id=$1 AND erased_at IS NOT NULL
	)`, runID).Scan(&erased); err != nil {
		return err
	}
	if erased {
		return ErrRecordingOutputErased
	}
	return nil
}

// recordingOutputEligible is rechecked at commit: neither an old read nor a
// surviving cast can recreate expired output or replace a direct final capture.
func recordingOutputEligible(ctx context.Context, tx pgx.Tx, runID uuid.UUID, retention time.Duration) (bool, error) {
	var eligible bool
	err := tx.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM agent_runs WHERE id=$1 AND NOT interactive
			AND ($2::double precision <= 0 OR ended_at IS NULL OR ended_at >= now() - make_interval(secs => $2)))
		AND NOT EXISTS (SELECT 1 FROM run_outputs WHERE run_id=$1 AND captured_at IS NOT NULL
			AND (source='pane_snapshot' OR (source='stdout' AND NOT capture_gap)))`,
		runID, retention.Seconds()).Scan(&eligible)
	return eligible, err
}

// QueueRecordingRunOutput — see RunOutputStore.
func (s PG) QueueRecordingRunOutput(ctx context.Context, runID uuid.UUID, retention, retryAfter time.Duration, newRecording bool) error {
	return s.runOutputTx(ctx, runID, false, func(tx pgx.Tx) error {
		if err := recordingOutputErased(ctx, tx, runID); err != nil {
			return err
		}
		eligible, err := recordingOutputEligible(ctx, tx, runID, retention)
		if err != nil || !eligible {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO run_output_recording_recovery (run_id, requested_generation)
			SELECT $1, 1 WHERE $2 OR NOT EXISTS (SELECT 1 FROM run_outputs
				WHERE run_id=$1 AND source='recording' AND captured_at IS NOT NULL AND NOT capture_gap)
			ON CONFLICT (run_id) DO UPDATE SET requested_generation = run_output_recording_recovery.requested_generation + 1
			WHERE $2 OR (run_output_recording_recovery.requested_generation = run_output_recording_recovery.completed_generation
				AND (run_output_recording_recovery.claimed_at IS NULL
					OR run_output_recording_recovery.claimed_at < now() - make_interval(secs => $3)))`,
			runID, newRecording, retryAfter.Seconds())
		return err
	})
}

// ClaimRecordingRunOutput — see RunOutputStore.
func (s PG) ClaimRecordingRunOutput(ctx context.Context, runID uuid.UUID, retention, staleAfter time.Duration) (RecordingOutputClaim, bool, error) {
	claim := RecordingOutputClaim{RunID: runID, Token: uuid.New()}
	claimed := false
	err := s.runOutputTx(ctx, runID, false, func(tx pgx.Tx) error {
		if err := recordingOutputErased(ctx, tx, runID); err != nil {
			return err
		}
		eligible, err := recordingOutputEligible(ctx, tx, runID, retention)
		if err != nil {
			return err
		}
		if !eligible {
			_, err := tx.Exec(ctx, `UPDATE run_output_recording_recovery
				SET completed_generation=requested_generation, claim_token=NULL WHERE run_id=$1`, runID)
			return err
		}
		err = tx.QueryRow(ctx, `UPDATE run_output_recording_recovery SET claim_token=$2, claimed_at=now()
			WHERE run_id=$1 AND requested_generation > completed_generation AND erased_at IS NULL
				AND (claim_token IS NULL OR claimed_at < now() - make_interval(secs => $3))
				AND EXISTS (SELECT 1 FROM agent_runs WHERE id=$1 AND state NOT IN (`+nonTerminalRunStates+`))
			RETURNING requested_generation`, runID, claim.Token, staleAfter.Seconds()).Scan(&claim.Generation)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		claimed = err == nil
		return err
	})
	return claim, claimed, err
}

// SaveRecordingRunOutput — see RunOutputStore.
func (s PG) SaveRecordingRunOutput(ctx context.Context, claim RecordingOutputClaim, o RunOutput, retention time.Duration) (bool, error) {
	if claim.RunID != o.RunID || o.Source != "recording" || o.CaptureGap && len(o.Output) != 0 {
		return false, errors.New("store: invalid recording output result")
	}
	wrote := false
	err := s.runOutputTx(ctx, claim.RunID, false, func(tx pgx.Tx) error {
		if err := recordingOutputErased(ctx, tx, claim.RunID); err != nil {
			return err
		}
		current, err := currentRecordingOutputClaim(ctx, tx, claim, retention)
		if err != nil || !current {
			return err
		}
		if !o.CaptureGap {
			if err := recordingOutputCovered(ctx, tx, o); err != nil {
				return err
			}
		}
		wrote, err = saveRecordingOutputRow(ctx, tx, o)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE run_output_recording_recovery
			SET completed_generation=requested_generation, claim_token=NULL WHERE run_id=$1`, claim.RunID)
		return err
	})
	return wrote, err
}

func currentRecordingOutputClaim(ctx context.Context, tx pgx.Tx, claim RecordingOutputClaim, retention time.Duration) (bool, error) {
	var own, current bool
	err := tx.QueryRow(ctx, `SELECT COALESCE(claim_token=$2, false), requested_generation=$3
		AND EXISTS (SELECT 1 FROM agent_runs WHERE id=$1 AND state NOT IN (`+nonTerminalRunStates+`))
		FROM run_output_recording_recovery WHERE run_id=$1`, claim.RunID, claim.Token, claim.Generation).Scan(&own, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil || !own {
		return false, err
	}
	eligible, err := recordingOutputEligible(ctx, tx, claim.RunID, retention)
	if err != nil {
		return false, err
	}
	if !current || !eligible {
		_, err := tx.Exec(ctx, `UPDATE run_output_recording_recovery SET claim_token=NULL,
			completed_generation=CASE WHEN $2 THEN completed_generation ELSE requested_generation END
			WHERE run_id=$1`, claim.RunID, eligible)
		return false, err
	}
	return true, nil
}

func recordingOutputCovered(ctx context.Context, tx pgx.Tx, o RunOutput) error {
	if o.MaskScope == "" {
		return nil
	}
	if o.MaskScope != "run" {
		return ErrRecordingOutputUncovered
	}
	var covered bool
	// The row lock orders the final commit against FenceSubject and mask retention;
	// loading keys or refreshing the registry here would nest pool acquisitions.
	err := tx.QueryRow(ctx, `SELECT complete AND fenced_at IS NULL FROM run_mask_manifest WHERE run_id=$1 FOR SHARE`, o.RunID).Scan(&covered)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !covered {
		return ErrRecordingOutputUncovered
	}
	return err
}

func saveRecordingOutputRow(ctx context.Context, tx pgx.Tx, o RunOutput) (bool, error) {
	if o.Output == nil {
		o.Output = []byte{}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO run_outputs
		(run_id, output, truncated, incomplete, capture_gap, source, mask_scope, captured_at, claimed_at)
		VALUES ($1, $2, $3, true, $4, 'recording', NULLIF($5, ''), now(), now())
		ON CONFLICT (run_id) DO UPDATE SET output=EXCLUDED.output, truncated=EXCLUDED.truncated,
			incomplete=true, capture_gap=EXCLUDED.capture_gap, source='recording', mask_scope=EXCLUDED.mask_scope,
			captured_at=EXCLUDED.captured_at, claimed_at=EXCLUDED.claimed_at
		WHERE run_outputs.captured_at IS NULL OR (NOT EXCLUDED.capture_gap
			AND (run_outputs.source='recording' OR (run_outputs.source='stdout' AND run_outputs.capture_gap)))`,
		o.RunID, o.Output, o.Truncated, o.CaptureGap, o.MaskScope)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	_, err = tx.Exec(ctx, `DELETE FROM run_output_chunks WHERE run_id=$1`, o.RunID)
	return true, err
}

// ListPendingRecordingRunOutputs — see RunOutputStore.
func (s PG) ListPendingRecordingRunOutputs(ctx context.Context, staleAfter time.Duration, limit int) ([]uuid.UUID, error) {
	return collect(ctx, s.Pool, "list", "pending recording output", `
		SELECT p.run_id FROM run_output_recording_recovery p JOIN agent_runs r ON r.id=p.run_id
		WHERE p.erased_at IS NULL AND p.requested_generation > p.completed_generation
			AND (p.claim_token IS NULL OR p.claimed_at < now() - make_interval(secs => $1))
			AND r.state NOT IN (`+nonTerminalRunStates+`)
		ORDER BY p.claimed_at NULLS FIRST, p.run_id LIMIT $2`, []any{staleAfter.Seconds(), max(1, limit)},
		func(row pgx.Row) (uuid.UUID, error) {
			var id uuid.UUID
			err := row.Scan(&id)
			return id, err
		})
}

// EraseRecordingRunOutputs — see RunOutputStore.
func (s PG) EraseRecordingRunOutputs(ctx context.Context, runIDs []uuid.UUID) (int, error) {
	if len(runIDs) == 0 {
		return 0, nil
	}
	ids := slices.Clone(runIDs)
	slices.SortFunc(ids, func(a, b uuid.UUID) int {
		return cmp.Compare(runOutputLockKey(a), runOutputLockKey(b))
	})
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // no-op after commit
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, db.RunOutputLockClass, runOutputLockKey(id)); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_output_recording_recovery (run_id, erased_at)
		SELECT DISTINCT unnest($1::uuid[]), now() ON CONFLICT (run_id) DO UPDATE
		SET erased_at=COALESCE(run_output_recording_recovery.erased_at, EXCLUDED.erased_at), claim_token=NULL,
			completed_generation=run_output_recording_recovery.requested_generation`, ids); err != nil {
		return 0, fmt.Errorf("store: fence recording output: %w", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM run_outputs WHERE run_id=ANY($1) AND source='recording'`, ids)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), tx.Commit(ctx)
}
