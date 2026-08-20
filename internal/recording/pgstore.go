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

// Compile-time assertion: PGStore implements Store.
var _ Store = (*PGStore)(nil)

// maxCastBytes bounds a single stored cast (migration 0028). It matches
// internal/api/recording.go's maxRecordingUploadBytes (64 MiB), but this cap
// is NOT redundant with that one: it fronts only ONE of SaveCast's two
// callers (the HTTP upload handler, via http.MaxBytesReader). The other
// caller — the interactive-attach session recorder
// (internal/api/attach.go newSessionRecorder's finish closure) — calls
// SaveCastNamed directly, in-process, with no HTTP body to bound; it now
// truncates at its own, much lower maxSessionCastBytes, so in practice this cap
// only ever bites the upload path. Unlike a
// local fs write, an oversized BYTEA row bloats the shared Postgres
// table/WAL/backups for every replica, not just one pod's disk, so the store
// enforces its own cap rather than trusting every caller to have one
// upstream. Kept equal to the upload cap so behavior does not depend on which
// path recorded the session.
const maxCastBytes = 64 << 20 // 64 MiB

// readCapped reads everything r yields, stopping at limit+1 bytes so the caller
// can tell "exactly at the cap" from "over it".
//
// It exists because both obvious spellings blow through the control plane's
// memory ceiling on a max-size cast — the deployed limit is 512Mi
// (deploy/helm/wardyn/values.yaml resources.limits.memory) and an in-sandbox
// agent streaming an unbounded cast is EXPLICITLY modelled hostile behavior
// (internal/api/recording.go). Both grow by REALLOCATING, so the old and new
// backing arrays are live at once and the earlier ones are still uncollected
// garbage. Measured peak runtime.MemStats.HeapAlloc for one 64 MiB cast, go1.26:
//
//	io.ReadAll(io.LimitReader(r, cap+1))        158.3 MiB   (chunk list + final copy)
//	bytes.Buffer(1 MiB) + io.Copy               224.6 MiB   (2x regrow — WORSE)
//	64 KiB start, ONE regrow straight to cap+1   64.7 MiB   (this)
//
// Two concurrent max-size uploads is the OOMKill case, so the fix is to make
// the buffer reach its final size in one step: a typical few-KiB cast never
// leaves the 64 KiB start (0.1 MiB allocated), and the worst case peaks at the
// payload itself instead of 2.5x it.
//
// Do not "simplify" this back to one of the others after a Go upgrade. The two
// spellings above depend on runtime growth heuristics that change between
// releases (io.ReadAll grew by reallocation before go1.21 and by a chunk list
// after); this reaches its final size in exactly two allocations by
// construction, so its ceiling holds whatever those heuristics do next.
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

// PGStore is a Postgres-backed Store (migration 0028). Unlike FSStore, a cast
// saved through one replica's handle is immediately visible to OpenCast
// through any OTHER replica's handle — FSStore's directory is per-pod, so a
// replay request that lands on a different pod than the one that recorded the
// session 404s. The zero value is unusable; use NewPGStore.
type PGStore struct {
	pool *pgxpool.Pool
}

// NewPGStore returns a Store backed by pool — the SAME pgxpool the rest of the
// control plane uses, so there is no separate connection or credential to
// manage.
func NewPGStore(pool *pgxpool.Pool) *PGStore {
	return &PGStore{pool: pool}
}

// SaveCastNamed persists r under the composite "<runID>~<suffix>" key (see
// Store's doc for the addressing contract). validSuffix (store.go) is shared
// verbatim with FSStore so a suffix is accepted or rejected identically
// regardless of which store an operator has selected.
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
	// Read fully (bounded) rather than streaming: pgx sends a bytea parameter as
	// a single []byte, so there is nothing to stream to — readCapped stops at
	// cap+1 so a too-large cast is detected and rejected before any INSERT is
	// even attempted.
	data, err := readCapped(r, maxCastBytes)
	if err != nil {
		return fmt.Errorf("recording: read cast %q: %w", runID, err)
	}
	if len(data) > maxCastBytes {
		return fmt.Errorf("recording: cast %q exceeds %d byte limit", runID, maxCastBytes)
	}
	_, err = s.pool.Exec(ctx, `
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
}

// OpenCast returns a ReadCloser over the stored bytes for key (either a bare
// runID or a "<runID>~<suffix>" composite). Returns ErrNotFound when absent.
func (s *PGStore) OpenCast(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := validKey(key); err != nil {
		return nil, err
	}
	var payload []byte
	err := s.pool.QueryRow(ctx,
		`SELECT payload FROM recordings WHERE cast_key = $1`, key,
	).Scan(&payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("recording: open cast %q: %w", key, err)
	}
	return io.NopCloser(bytes.NewReader(payload)), nil
}

// Sweep deletes every cast row last written more than olderThan ago,
// returning how many rows it removed. It mirrors FSStore.Sweep's age
// semantics — measured on last write via updated_at, not created_at, so a
// cast that was re-saved is never swept out from under an in-progress session
// — but, like FSStore.Sweep, is deliberately NOT part of the Store interface
// (see store.go's package doc: retention is a storage-backend concern, and a
// future object-storage backend would use its bucket's own lifecycle rules
// instead of an app-level sweep). cmd/wardynd's startBackgroundWorkers
// reaches this through the unexported recordingSweepable interface
// (adapters.go), which both FSStore and PGStore satisfy structurally.
func (s *PGStore) Sweep(olderThan time.Duration) (int, error) {
	// Bounded, not context.Background(): the DELETE walks a TOASTed table that
	// grows without bound under the keep-forever default, and its caller
	// (cmd/wardynd/adapters.go runRecordingSweeper) runs it SYNCHRONOUSLY on the
	// sweep loop — an unbounded statement would pin the sweeper past rootCtx
	// cancellation and stall shutdown. Timing out just skips a sweep; the next
	// tick retries, and the retention window is a day-scale knob.
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

func init() {
	Register("pg", func(d Deps) (Store, error) {
		if d.Pool == nil {
			// Unlike fs's empty-Dir => disabled convention (an operator-facing
			// knob, WARDYN_RECORDING_DIR), Deps.Pool has no operator-facing
			// equivalent — wardynd always has a pool by the time it selects a
			// recording store (Postgres is the one required dependency), so a
			// nil pool here can only mean a wiring bug. Fail loud rather than
			// silently going dark on the governance-evidence path.
			return nil, errors.New("recording: pg store requires a pool (Deps.Pool is nil)")
		}
		return NewPGStore(d.Pool), nil
	})
}
