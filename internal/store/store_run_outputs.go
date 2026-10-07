// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The persisted run output (migration 0119): one masked final row per run, the
// pending row that records a capture is owed, and the erasure tombstones that
// keep an erased run's output erased. Kept out of store.go for the same size
// reason as store_watcher.go.
package store

import (
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrRunOutputErased is every writer's and reader's answer for a run whose
// output was erased: the tombstone is checked in the same transaction as the
// write or read, under the run's lock, so no replica can recreate or serve it.
var ErrRunOutputErased = errors.New("store: the run's output was erased")

// RunOutput is one row of run_outputs. CapturedAt is nil while the row is
// pending (a capture is owed); MaskScope is "" when the row carries none.
type RunOutput struct {
	RunID      uuid.UUID
	Output     []byte
	Truncated  bool
	Incomplete bool
	CaptureGap bool
	Source     string // "stdout", "pane_snapshot" or "recording"
	MaskScope  string // "run", "globals_only" or ""
	CapturedAt *time.Time
	ClaimedAt  time.Time
	// RecordingErased is independent of found: its durable source fence remains
	// after the derived row is deleted, without hiding unrelated stdout/panes.
	RecordingErased bool
}

// RunOutputStore is the run-output record. Optional like RunWatcherLeaser: the
// ~30 test doubles that embed Store would route these to a nil interface, so
// the api layer type-asserts and keeps output in memory only when a store lacks
// it. Production is always PG. Capture and read transactions check the general
// erasure tombstone; erasure and housekeeping remain permitted afterwards.
type RunOutputStore interface {
	// InsertPendingRunOutput records that a capture is owed for runID. A row
	// that already exists is left as it is.
	InsertPendingRunOutput(ctx context.Context, runID uuid.UUID) error
	// RefreshRunOutputClaim stamps a pending row's claimed_at: the final write
	// is being retried, so the sweeper must not resolve it yet.
	RefreshRunOutputClaim(ctx context.Context, runID uuid.UUID) error
	// SaveFinalRunOutput writes the run's final row (output, flags and
	// mask_scope as given), replacing a pending or stdout capture-gap row. The
	// database stamps captured_at.
	SaveFinalRunOutput(ctx context.Context, o RunOutput) error
	// SaveGapRunOutput writes a capture-gap row with no bytes, only over a
	// pending row or none: a final row already committed is never overwritten.
	// It reports whether it wrote.
	SaveGapRunOutput(ctx context.Context, runID uuid.UUID) (bool, error)
	// MarkRunOutputIncomplete sets incomplete on runID's final row, reporting
	// whether a final row was there to mark.
	MarkRunOutputIncomplete(ctx context.Context, runID uuid.UUID) (bool, error)
	// GetRunOutput returns runID's row, pending or final. found is false when
	// there is none.
	GetRunOutput(ctx context.Context, runID uuid.UUID) (o RunOutput, found bool, err error)
	// EraseRunOutputs writes a tombstone for each run and deletes its rows, in
	// one transaction: all of it or none of it.
	EraseRunOutputs(ctx context.Context, runIDs []uuid.UUID) error
	// DeleteRunOutputsOlderThan deletes final rows captured longer ago than age
	// (the database's clock), returning how many went.
	DeleteRunOutputsOlderThan(ctx context.Context, age time.Duration) (int, error)
	// ListStalePendingRunOutputs returns the runs whose pending row was claimed
	// longer ago than age and which are terminal, oldest first, at most limit.
	ListStalePendingRunOutputs(ctx context.Context, age time.Duration, limit int) ([]uuid.UUID, error)
	// QueueRecordingRunOutput records recovery work after a committed upload.
	// newRecording also invalidates a running claim; false is bounded read repair.
	QueueRecordingRunOutput(ctx context.Context, runID uuid.UUID, retention, retryAfter time.Duration, newRecording bool) error
	// ClaimRecordingRunOutput reserves one eligible terminal run; source reads
	// happen after this transaction releases its connection.
	ClaimRecordingRunOutput(ctx context.Context, runID uuid.UUID, retention, staleAfter time.Duration) (RecordingOutputClaim, bool, error)
	// SaveRecordingRunOutput accepts only the current claim/generation and never
	// replaces final direct stdout or a pane snapshot. It consumes the claim.
	SaveRecordingRunOutput(ctx context.Context, claim RecordingOutputClaim, o RunOutput, retention time.Duration) (bool, error)
	// ListPendingRecordingRunOutputs bounds one elected recovery pass.
	ListPendingRecordingRunOutputs(ctx context.Context, staleAfter time.Duration, limit int) ([]uuid.UUID, error)
	// EraseRecordingRunOutputs fences future recording-derived rows and deletes
	// only that source, independently of the currently configured recording store.
	EraseRecordingRunOutputs(ctx context.Context, runIDs []uuid.UUID) (int, error)
}

var _ RunOutputStore = PG{}

// runOutputTx runs fn in a transaction that holds runID's lock (shared for a
// read, exclusive for a write) and has found no tombstone for it, so a write
// can never commit after an erasure and a read can never serve what one removed.
func (s PG) runOutputTx(ctx context.Context, runID uuid.UUID, shared bool, fn func(pgx.Tx) error) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("store: run output: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	lock := `SELECT pg_advisory_xact_lock($1, $2)`
	if shared {
		lock = `SELECT pg_advisory_xact_lock_shared($1, $2)`
	}
	if _, err := tx.Exec(ctx, lock, db.RunOutputLockClass, runOutputLockKey(runID)); err != nil {
		return fmt.Errorf("store: run output lock: %w", err)
	}
	var erased bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_output_erasures WHERE run_id = $1)`, runID).Scan(&erased); err != nil {
		return fmt.Errorf("store: run output tombstone: %w", err)
	}
	if erased {
		return ErrRunOutputErased
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// runOutputLockKey is the objid half of runID's lock. A crc32 collision only
// serializes two runs.
func runOutputLockKey(runID uuid.UUID) int32 { return int32(crc32.ChecksumIEEE(runID[:])) }

// InsertPendingRunOutput — see RunOutputStore.
func (s PG) InsertPendingRunOutput(ctx context.Context, runID uuid.UUID) error {
	return s.runOutputTx(ctx, runID, false, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO run_outputs (run_id, source) VALUES ($1, 'stdout') ON CONFLICT (run_id) DO NOTHING`, runID)
		return err
	})
}

// RefreshRunOutputClaim — see RunOutputStore.
func (s PG) RefreshRunOutputClaim(ctx context.Context, runID uuid.UUID) error {
	return s.runOutputTx(ctx, runID, false, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE run_outputs SET claimed_at = now() WHERE run_id = $1 AND captured_at IS NULL`, runID)
		return err
	})
}

// SaveFinalRunOutput — see RunOutputStore.
func (s PG) SaveFinalRunOutput(ctx context.Context, o RunOutput) error {
	if o.Source == "recording" {
		return errors.New("store: recording output requires a recovery claim")
	}
	return s.runOutputTx(ctx, o.RunID, false, func(tx pgx.Tx) error {
		body := o.Output
		if body == nil {
			body = []byte{}
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO run_outputs (run_id, output, truncated, incomplete, capture_gap, source, mask_scope, captured_at, claimed_at)
			VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), now(), now())
			ON CONFLICT (run_id) DO UPDATE SET
				output = EXCLUDED.output, truncated = EXCLUDED.truncated, incomplete = EXCLUDED.incomplete,
				capture_gap = EXCLUDED.capture_gap, source = EXCLUDED.source, mask_scope = EXCLUDED.mask_scope,
				captured_at = EXCLUDED.captured_at, claimed_at = EXCLUDED.claimed_at
			WHERE run_outputs.captured_at IS NULL
			   OR (run_outputs.capture_gap AND run_outputs.source = 'stdout')`,
			o.RunID, body, o.Truncated, o.Incomplete, o.CaptureGap, o.Source, o.MaskScope)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		// The live chunks (0129) were the tail's stand-in until this row: they go in the transaction that writes it.
		_, err = tx.Exec(ctx, `DELETE FROM run_output_chunks WHERE run_id = $1`, o.RunID)
		return err
	})
}

// SaveGapRunOutput — see RunOutputStore.
func (s PG) SaveGapRunOutput(ctx context.Context, runID uuid.UUID) (bool, error) {
	wrote := false
	err := s.runOutputTx(ctx, runID, false, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO run_outputs (run_id, source, capture_gap, captured_at)
			VALUES ($1, 'stdout', true, now())
			ON CONFLICT (run_id) DO UPDATE SET capture_gap = true, captured_at = now(), claimed_at = now()
			WHERE run_outputs.captured_at IS NULL`, runID)
		if err != nil {
			return err
		}
		wrote = tag.RowsAffected() == 1
		if !wrote {
			return nil
		}
		_, err = tx.Exec(ctx, `DELETE FROM run_output_chunks WHERE run_id = $1`, runID)
		return err
	})
	return wrote, err
}

// MarkRunOutputIncomplete — see RunOutputStore.
func (s PG) MarkRunOutputIncomplete(ctx context.Context, runID uuid.UUID) (bool, error) {
	marked := false
	err := s.runOutputTx(ctx, runID, false, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE run_outputs SET incomplete = true WHERE run_id = $1 AND captured_at IS NOT NULL`, runID)
		marked = err == nil && tag.RowsAffected() == 1
		return err
	})
	return marked, err
}

// GetRunOutput — see RunOutputStore.
func (s PG) GetRunOutput(ctx context.Context, runID uuid.UUID) (RunOutput, bool, error) {
	var o RunOutput
	found := false
	err := s.runOutputTx(ctx, runID, true, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM run_output_recording_recovery WHERE run_id=$1 AND erased_at IS NOT NULL
		)`, runID).Scan(&o.RecordingErased); err != nil {
			return err
		}
		var scope *string
		err := tx.QueryRow(ctx, `
			SELECT run_id, output, truncated, incomplete, capture_gap, source, mask_scope, captured_at, claimed_at
			FROM run_outputs WHERE run_id = $1`, runID).
			Scan(&o.RunID, &o.Output, &o.Truncated, &o.Incomplete, &o.CaptureGap, &o.Source, &scope, &o.CapturedAt, &o.ClaimedAt)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		if scope != nil {
			o.MaskScope = *scope
		}
		if o.RecordingErased && o.Source == "recording" {
			o = RunOutput{RunID: runID, RecordingErased: true}
			return nil
		}
		found = true
		return nil
	})
	return o, found, err
}

// EraseRunOutputs — see RunOutputStore. The locks are taken in key order, so
// two overlapping erasures cannot each hold what the other waits for.
func (s PG) EraseRunOutputs(ctx context.Context, runIDs []uuid.UUID) error {
	if len(runIDs) == 0 {
		return nil
	}
	ids := slices.Clone(runIDs)
	slices.SortFunc(ids, func(a, b uuid.UUID) int {
		ka, kb := runOutputLockKey(a), runOutputLockKey(b)
		switch {
		case ka < kb:
			return -1
		case ka > kb:
			return 1
		}
		return 0
	})
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("store: erase run outputs: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a no-op after Commit
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`, db.RunOutputLockClass, runOutputLockKey(id)); err != nil {
			return fmt.Errorf("store: erase run outputs: lock: %w", err)
		}
	}
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `INSERT INTO run_output_erasures (run_id) VALUES ($1) ON CONFLICT (run_id) DO NOTHING`, id); err != nil {
			return fmt.Errorf("store: erase run outputs: tombstone: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM run_outputs WHERE run_id = ANY($1)`, ids); err != nil {
		return fmt.Errorf("store: erase run outputs: delete: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM run_output_chunks WHERE run_id = ANY($1)`, ids); err != nil {
		return fmt.Errorf("store: erase run outputs: delete chunks: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE run_output_recording_recovery
		SET completed_generation=requested_generation, claim_token=NULL WHERE run_id=ANY($1)`, ids); err != nil {
		return fmt.Errorf("store: erase run outputs: finish recording work: %w", err)
	}
	return tx.Commit(ctx)
}

// DeleteRunOutputsOlderThan — see RunOutputStore.
func (s PG) DeleteRunOutputsOlderThan(ctx context.Context, age time.Duration) (int, error) {
	tag, err := s.Pool.Exec(ctx,
		`DELETE FROM run_outputs WHERE captured_at IS NOT NULL AND captured_at < now() - $1::interval`, age.String())
	if err != nil {
		return 0, fmt.Errorf("store: delete old run outputs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ListStalePendingRunOutputs — see RunOutputStore.
func (s PG) ListStalePendingRunOutputs(ctx context.Context, age time.Duration, limit int) ([]uuid.UUID, error) {
	nonTerminal := make([]string, len(types.NonTerminalRunStates))
	for i, st := range types.NonTerminalRunStates {
		nonTerminal[i] = string(st)
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT o.run_id FROM run_outputs o JOIN agent_runs r ON r.id = o.run_id
		WHERE o.captured_at IS NULL AND o.claimed_at < now() - $1::interval AND r.state <> ALL($2)
		ORDER BY o.claimed_at LIMIT $3`, age.String(), nonTerminal, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list stale pending run outputs: %w", err)
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan stale pending run output: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
