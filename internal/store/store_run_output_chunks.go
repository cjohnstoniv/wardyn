// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The live exec tail (migration 0129): the masked bytes a run's dispatching replica has
// printed so far, readable from any replica. One final row per run is run_outputs' (0122);
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
	Recovered bool // a recovery's chunk is among them (RunOutputChunkReplacer)
}

// RunOutputChunkStore is the live-chunk record. Optional like RunOutputStore: the api layer
// type-asserts, and a store without it keeps the tail in the writing process only. Every method
// checks the run's erasure tombstone in its own transaction and answers ErrRunOutputErased.
type RunOutputChunkStore interface {
	// AppendRunOutputChunk adds b, which the caller's masker has already passed, as runID's next
	// chunk, then deletes the oldest chunks until what is left is the fewest that still hold
	// keep bytes. It reports false, and writes nothing, once the run's final row exists: a
	// writer that was slower than the finisher never recreates what the final row replaced.
	// A chunk it writes comes with the run's pending row, in the same transaction, when the run
	// has no row yet: every stored chunk has the row that recovery and retention go by. It also
	// reports false, writing and trimming nothing, once a recovery's chunk is stored for the run
	// (RunOutputChunkReplacer): the replica that took the run over owns its chunks from then on.
	AppendRunOutputChunk(ctx context.Context, runID uuid.UUID, replica string, b []byte, keep int) (bool, error)
	// ReadRunOutputChunks returns the last limit bytes of runID's chunks.
	ReadRunOutputChunks(ctx context.Context, runID uuid.UUID, limit int) (RunOutputChunks, error)
	// SaveChunkedRunOutput writes runID's row from its chunks, as an incomplete capture gap (what
	// the replica that wrote them had not flushed is lost), and deletes them, in one transaction.
	// It reports false, writing nothing, when the run has no chunks or already has a final row.
	SaveChunkedRunOutput(ctx context.Context, runID uuid.UUID, maskScope string, limit int) (bool, error)
}

// RunOutputChunkReplacer is a chunk store a live adoption's recovery tail writes into, replacing the chunks the
// run's launcher wrote. Optional: the api layer type-asserts, and a store without it keeps v0.10.2's recovery tail,
// which writes no chunks. A recovery's chunk is stored under recoveredChunkMark plus the replica name it is given,
// a mark no real replica name (a uuid string) can hold.
type RunOutputChunkReplacer interface {
	// AppendRecoveredRunOutputChunk is AppendRunOutputChunk for a recovery tail. With first it deletes every chunk
	// of the run before it writes, since the re-read starts at the runner's first byte; otherwise it deletes the
	// chunks another replica appended meanwhile. Without first it writes nothing, and reports false, while another
	// recovery's chunk is present, so a later adoption supersedes an earlier one.
	AppendRecoveredRunOutputChunk(ctx context.Context, runID uuid.UUID, replica string, b []byte, keep int, first bool) (bool, error)
	// RecoveredRunOutputChunks reports whether runID holds a recovery's chunk from a replica other than mine;
	// with mine "" every recovery's chunk counts.
	RecoveredRunOutputChunks(ctx context.Context, runID uuid.UUID, mine string) (bool, error)
}

var (
	_ RunOutputChunkStore    = PG{}
	_ RunOutputChunkReplacer = PG{}
)

// recoveredChunkMark prefixes the replica of a recovery's chunk.
const recoveredChunkMark = "recovered:"

// AppendRunOutputChunk — see RunOutputChunkStore.
func (s PG) AppendRunOutputChunk(ctx context.Context, runID uuid.UUID, replica string, b []byte, keep int) (bool, error) {
	if len(b) == 0 {
		return false, nil
	}
	wrote := false
	err := s.runOutputTx(ctx, runID, false, func(tx pgx.Tx) error {
		var recovered bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_output_chunks WHERE run_id = $1 AND replica LIKE 'recovered:%')`,
			runID).Scan(&recovered); err != nil || recovered {
			return err // a recovery owns the chunks: this append's trim would evict its bytes
		}
		var err error
		wrote, err = appendRunOutputChunk(ctx, tx, runID, replica, b, keep)
		return err
	})
	if err != nil && !errors.Is(err, ErrRunOutputErased) {
		err = fmt.Errorf("store: append a run output chunk: %w", err)
	}
	return wrote, err
}

// AppendRecoveredRunOutputChunk — see RunOutputChunkReplacer.
func (s PG) AppendRecoveredRunOutputChunk(ctx context.Context, runID uuid.UUID, replica string, b []byte, keep int, first bool) (bool, error) {
	if len(b) == 0 {
		return false, nil
	}
	mark := recoveredChunkMark + replica
	wrote := false
	err := s.runOutputTx(ctx, runID, false, func(tx pgx.Tx) error {
		if !first {
			var other bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_output_chunks WHERE run_id = $1 AND replica LIKE 'recovered:%' AND replica <> $2)`,
				runID, mark).Scan(&other); err != nil || other {
				return err // another recovery's chunk: a later adoption's queue supersedes this one
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM run_output_chunks WHERE run_id = $1 AND (replica <> $2 OR $3)`, runID, mark, first); err != nil {
			return err
		}
		var err error
		wrote, err = appendRunOutputChunk(ctx, tx, runID, mark, b, keep)
		return err
	})
	if err != nil && !errors.Is(err, ErrRunOutputErased) {
		err = fmt.Errorf("store: append a recovered run output chunk: %w", err)
	}
	return wrote, err
}

// RecoveredRunOutputChunks — see RunOutputChunkReplacer.
func (s PG) RecoveredRunOutputChunks(ctx context.Context, runID uuid.UUID, mine string) (bool, error) {
	mark := ""
	if mine != "" {
		mark = recoveredChunkMark + mine
	}
	var found bool
	err := s.runOutputTx(ctx, runID, true, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_output_chunks WHERE run_id = $1 AND replica LIKE 'recovered:%' AND replica <> $2)`,
			runID, mark).Scan(&found)
	})
	if err != nil && !errors.Is(err, ErrRunOutputErased) {
		err = fmt.Errorf("store: read whether a run's chunks are a recovery's: %w", err)
	}
	return found, err
}

// appendRunOutputChunk writes b as runID's next chunk under replica, inside the caller's transaction, and trims the
// oldest chunks to keep bytes. It reports false, writing nothing, once the run's final row exists.
func appendRunOutputChunk(ctx context.Context, tx pgx.Tx, runID uuid.UUID, replica string, b []byte, keep int) (bool, error) {
	tag, err := tx.Exec(ctx, `
		INSERT INTO run_output_chunks (run_id, seq, replica, bytes_masked)
		SELECT $1, COALESCE((SELECT max(seq) FROM run_output_chunks WHERE run_id = $1), 0) + 1, $2, $3
		WHERE NOT EXISTS (SELECT 1 FROM run_outputs WHERE run_id = $1 AND captured_at IS NOT NULL)`,
		runID, replica, b)
	if err != nil || tag.RowsAffected() != 1 {
		return false, err
	}
	// The owed row InsertPendingRunOutput writes, for a run whose dispatch-time insert failed.
	if _, err = tx.Exec(ctx, `INSERT INTO run_outputs (run_id, source) VALUES ($1, 'stdout') ON CONFLICT (run_id) DO NOTHING`, runID); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `
		DELETE FROM run_output_chunks WHERE run_id = $1 AND seq < COALESCE((
			SELECT seq FROM (
				SELECT seq, sum(octet_length(bytes_masked)) OVER (ORDER BY seq DESC) AS cum
				FROM run_output_chunks WHERE run_id = $1) t
			WHERE cum >= $2 ORDER BY seq DESC LIMIT 1), 0)`, runID, keep)
	return err == nil, err
}

// ReadRunOutputChunks — see RunOutputChunkStore.
func (s PG) ReadRunOutputChunks(ctx context.Context, runID uuid.UUID, limit int) (RunOutputChunks, error) {
	var out RunOutputChunks
	err := s.runOutputTx(ctx, runID, true, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT seq, bytes_masked, replica LIKE 'recovered:%' FROM run_output_chunks WHERE run_id = $1 ORDER BY seq`, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		first := int64(0)
		for rows.Next() {
			var seq int64
			var b []byte
			var recovered bool
			if err := rows.Scan(&seq, &b, &recovered); err != nil {
				return err
			}
			out.Recovered = out.Recovered || recovered
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
