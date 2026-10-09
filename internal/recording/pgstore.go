// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var _ Store = (*PGStore)(nil)

// maxCastBytes bounds a single stored cast, matching maxRecordingUploadBytes so behavior
// doesn't depend on which path recorded the session; also fronts the in-process
// attach-session recorder, which has no HTTP body to bound another way. An oversized BYTEA
// row bloats the shared Postgres table/WAL/backups for every replica, so the store enforces
// its own cap rather than trusting callers upstream.
const maxCastBytes = 64 << 20 // 64 MiB

// readCapped reads everything r yields, stopping at limit+1 bytes so the
// caller can tell "exactly at the cap" from "over it".
//
// A plain io.ReadAll or bytes.Buffer+io.Copy grows by reallocating, so old and new backing
// arrays are live at once — measured peak HeapAlloc for one 64 MiB cast (go1.26):
//
//	io.ReadAll(io.LimitReader(r, cap+1))        158.3 MiB
//	bytes.Buffer(1 MiB) + io.Copy                224.6 MiB   (worse)
//	64 KiB start, ONE regrow straight to cap+1    64.7 MiB   (this)
//
// Two concurrent max-size uploads is the OOMKill case this avoids by
// reaching final size in one regrow. Do not simplify this back to one of
// the others after a Go upgrade — those depend on runtime growth heuristics
// that change between releases; this doesn't.
func readCapped(r io.Reader, limit int) ([]byte, error) {
	buf := make([]byte, 0, min(64<<10, limit+1)) // min: the start must never exceed the cap

	for {
		if len(buf) == cap(buf) {
			if cap(buf) > limit {
				return buf, nil // limit+1 bytes: over the cap, caller rejects
			}
			buf = append(make([]byte, 0, limit+1), buf...)
		}
		n, err := r.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf, nil
			}
			return nil, err
		}
	}
}

// PGStore is a Postgres-backed Store. Unlike FSStore (per-pod directory), a cast saved via
// one replica is immediately visible through any other. The zero value is unusable; use
// NewPGStore.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore returns a Store backed by pool, the same pgxpool the rest of the control
// plane uses.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

// SaveCastNamed persists r under the composite "<runID>~<suffix>" key (see Store's doc for
// the addressing contract). validSuffix is shared verbatim with FSStore so a suffix is
// accepted or rejected identically regardless of the selected store.
func (s *PGStore) SaveCastNamed(ctx context.Context, runID, suffix string, r io.Reader) error {
	if err := validSuffix(suffix); err != nil {
		return err
	}
	return s.SaveCast(ctx, CastKey(runID, suffix), r)
}

// SaveCast persists the asciicast bytestream from r under cast_key = runID,
// replacing any prior cast stored under that same key (upsert), bounded by
// maxCastBytes.
func (s *PGStore) SaveCast(ctx context.Context, runID string, r io.Reader) error {
	if err := validKey(runID); err != nil {
		return err
	}
	// Read fully (bounded), not streamed: pgx sends a bytea parameter as a
	// single []byte, so a too-large cast is rejected before any INSERT.
	data, err := readCapped(contextReader{ctx, r}, maxCastBytes)
	if err != nil {
		return fmt.Errorf("recording: read cast %q: %w", runID, err)
	}
	if len(data) > maxCastBytes {
		return fmt.Errorf("recording: cast %q exceeds %d byte limit", runID, maxCastBytes)
	}
	return s.runTx(ctx, runID, false, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
		INSERT INTO recordings (cast_key, payload)
		VALUES ($1, $2)
		ON CONFLICT (cast_key) DO UPDATE
			SET payload = EXCLUDED.payload, updated_at = now()`,
			runID, data,
		)
		if err != nil {
			return fmt.Errorf("recording: save cast %q: %w", runID, err)
		}
		return nil
	})
}

// OpenCast returns a ReadCloser over the stored bytes for key (either a bare
// runID or a "<runID>~<suffix>" composite). Returns ErrNotFound when absent.
func (s *PGStore) OpenCast(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	var payload []byte
	err := s.runTx(ctx, key, false, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT payload FROM recordings WHERE cast_key = $1`, key,
		).Scan(&payload)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("recording: open cast %q: %w", key, err)
	}
	return io.NopCloser(bytes.NewReader(payload)), nil
}

// StatAndTail reports the cast's byte size and its last tailBytes, computed and sliced
// server-side so a caller wanting only a size and duration never pulls the whole payload
// across the wire. tailBytes is clamped to size when the cast is smaller.
func (s *PGStore) StatAndTail(ctx context.Context, key string, tailBytes int64) (int64, []byte, error) {
	if err := validKey(key); err != nil {
		return 0, nil, err
	}
	var size int64
	var tail []byte
	err := s.runTx(ctx, key, false, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
		SELECT octet_length(payload),
		       substring(payload from greatest(octet_length(payload) - $2::int + 1, 1)::int for $2::int)
		FROM recordings WHERE cast_key = $1`,
			key, tailBytes,
		).Scan(&size, &tail)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, ErrNotFound
	}
	if err != nil {
		return 0, nil, fmt.Errorf("recording: stat cast %q: %w", key, err)
	}
	return size, tail, nil
}

// Sweep deletes every cast row last written more than olderThan ago, returning how many
// rows it removed. Measured on updated_at, not created_at, so a re-saved cast is never
// swept from under an in-progress session. Deliberately not part of the Store interface —
// retention is a storage-backend concern, reached via the unexported recordingSweepable
// interface instead.
func (s *PGStore) Sweep(olderThan time.Duration) (int, error) {
	// Bounded: the caller runs this synchronously on the sweep loop, so an unbounded DELETE
	// would stall shutdown. Timing out just skips a sweep; the next tick retries.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM recordings WHERE updated_at < now() - $1::interval`,
		olderThan.String(),
	)
	if err != nil {
		return 0, fmt.Errorf("recording: sweep: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

var _ RunDeleter = (*PGStore)(nil)

// DeleteRun durably fences the run and removes its bare and suffixed casts in
// one transaction. Absent casts are not an error.
func (s *PGStore) DeleteRun(ctx context.Context, key string) (int, error) {
	if err := validKey(key); err != nil {
		return 0, err
	}
	var removed int
	err := s.runTx(ctx, key, true, func(tx pgx.Tx) error {
		run := key
		if _, err := tx.Exec(ctx, `INSERT INTO recording_erasures (run_id) VALUES ($1) ON CONFLICT DO NOTHING`, run); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`DELETE FROM recordings WHERE cast_key = $1 OR starts_with(cast_key, $1 || $2)`, run, castSep)
		removed = int(tag.RowsAffected())
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("recording: delete run %q: %w", key, err)
	}
	return removed, nil
}

func init() {
	Register("pg", func(d Deps) (Store, error) {
		if d.Pool == nil {
			// wardynd always has a pool by store-selection time, so nil here is a wiring bug — fail loud.
			return nil, errors.New("recording: pg store requires a pool (Deps.Pool is nil)")
		}
		return NewPGStore(d.Pool), nil
	})
}
