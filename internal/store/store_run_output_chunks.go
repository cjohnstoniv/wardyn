// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The live exec tail (migration 0121): the masked bytes a run's dispatching replica has
// printed so far, readable from any replica. One final row per run is run_outputs' (0119);
// these chunks are what stands in for it until that row is written, and what a recovery falls
// back to when the substrate cannot be re-read.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RunOutputChunks is a run's live tail as stored: the chunks' bytes in order, ending at the
// newest. Found is false when no chunk is held. Truncated says bytes older than Bytes were
// printed: chunks were trimmed, or the read cut the front.
type RunOutputChunks struct {
	Bytes     []byte
	Truncated bool
	Found     bool
}

// RunOutputChunkStore is the live-chunk record. Optional like RunOutputStore: the api layer
// type-asserts, and a store without it keeps the tail in the writing process only. Every method
// checks the run's erasure tombstone in its own transaction and answers ErrRunOutputErased.
type RunOutputChunkStore interface {
	// AppendRunOutputChunk adds b, which the caller's masker has already passed, as runID's next
	// chunk, then deletes the oldest chunks until what is left is the fewest that still hold
	// keep bytes. It reports false, and writes nothing, once the run's final row exists: a
	// writer that was slower than the finisher never recreates what the final row replaced.
	AppendRunOutputChunk(ctx context.Context, runID uuid.UUID, replica string, b []byte, keep int) (bool, error)
	// ReadRunOutputChunks returns the last limit bytes of runID's chunks.
	ReadRunOutputChunks(ctx context.Context, runID uuid.UUID, limit int) (RunOutputChunks, error)
	// SaveChunkedRunOutput writes runID's row from its chunks, as an incomplete capture gap (what
	// the replica that wrote them had not flushed is lost), and deletes them, in one transaction.
	// It reports false, writing nothing, when the run has no chunks or already has a final row.
	SaveChunkedRunOutput(ctx context.Context, runID uuid.UUID, maskScope string, limit int) (bool, error)
}

var _ RunOutputChunkStore = PG{}

// AppendRunOutputChunk — see RunOutputChunkStore.
func (s PG) AppendRunOutputChunk(ctx context.Context, runID uuid.UUID, replica string, b []byte, keep int) (bool, error) {
	if len(b) == 0 {
		return false, nil
	}
	wrote := false
	err := s.runOutputTx(ctx, runID, false, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO run_output_chunks (run_id, seq, replica, bytes_masked)
			SELECT $1, COALESCE((SELECT max(seq) FROM run_output_chunks WHERE run_id = $1), 0) + 1, $2, $3
			WHERE NOT EXISTS (SELECT 1 FROM run_outputs WHERE run_id = $1 AND captured_at IS NOT NULL)`,
			runID, replica, b)
		if err != nil {
			return err
		}
		if wrote = tag.RowsAffected() == 1; !wrote {
			return nil
		}
		_, err = tx.Exec(ctx, `
			DELETE FROM run_output_chunks WHERE run_id = $1 AND seq < COALESCE((
				SELECT seq FROM (
					SELECT seq, sum(octet_length(bytes_masked)) OVER (ORDER BY seq DESC) AS cum
					FROM run_output_chunks WHERE run_id = $1) t
				WHERE cum >= $2 ORDER BY seq DESC LIMIT 1), 0)`, runID, keep)
		return err
	})
	if err != nil && !errors.Is(err, ErrRunOutputErased) {
		err = fmt.Errorf("store: append a run output chunk: %w", err)
	}
	return wrote, err
}

// ReadRunOutputChunks — see RunOutputChunkStore.
func (s PG) ReadRunOutputChunks(ctx context.Context, runID uuid.UUID, limit int) (RunOutputChunks, error) {
	var out RunOutputChunks
	err := s.runOutputTx(ctx, runID, true, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT seq, bytes_masked FROM run_output_chunks WHERE run_id = $1 ORDER BY seq`, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		first := int64(0)
		for rows.Next() {
			var seq int64
			var b []byte
			if err := rows.Scan(&seq, &b); err != nil {
				return err
			}
			if !out.Found {
				first = seq
			}
			out.Found = true
			out.Bytes = append(out.Bytes, b...)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out.Truncated = first > 1
		if len(out.Bytes) > limit {
			out.Bytes, out.Truncated = out.Bytes[len(out.Bytes)-limit:], true
		}
		return nil
	})
	if err != nil && !errors.Is(err, ErrRunOutputErased) {
		err = fmt.Errorf("store: read run output chunks: %w", err)
	}
	return out, err
}

// SaveChunkedRunOutput — see RunOutputChunkStore.
func (s PG) SaveChunkedRunOutput(ctx context.Context, runID uuid.UUID, maskScope string, limit int) (bool, error) {
	wrote := false
	err := s.runOutputTx(ctx, runID, false, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT seq, bytes_masked FROM run_output_chunks WHERE run_id = $1 ORDER BY seq`, runID)
		if err != nil {
			return err
		}
		var body []byte
		first, found := int64(0), false
		for rows.Next() {
			var seq int64
			var b []byte
			if err := rows.Scan(&seq, &b); err != nil {
				rows.Close()
				return err
			}
			if !found {
				first, found = seq, true
			}
			body = append(body, b...)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if !found {
			return nil
		}
		truncated := first > 1
		if len(body) > limit {
			body, truncated = body[len(body)-limit:], true
		}
		tag, err := tx.Exec(ctx, `
			INSERT INTO run_outputs (run_id, output, truncated, incomplete, capture_gap, source, mask_scope, captured_at, claimed_at)
			VALUES ($1, $2, $3, true, true, 'stdout', NULLIF($4, ''), now(), now())
			ON CONFLICT (run_id) DO UPDATE SET output = EXCLUDED.output, truncated = EXCLUDED.truncated,
				incomplete = true, capture_gap = true, source = 'stdout', mask_scope = EXCLUDED.mask_scope,
				captured_at = now(), claimed_at = now()
			WHERE run_outputs.captured_at IS NULL`, runID, body, truncated, maskScope)
		if err != nil {
			return err
		}
		if wrote = tag.RowsAffected() == 1; !wrote {
			return nil
		}
		_, err = tx.Exec(ctx, `DELETE FROM run_output_chunks WHERE run_id = $1`, runID)
		return err
	})
	if err != nil && !errors.Is(err, ErrRunOutputErased) {
		err = fmt.Errorf("store: save a run's output from its chunks: %w", err)
	}
	return wrote, err
}
