// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
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
//
// start is the first capacity (see castBufferStart): a reader that reports its
// length starts at that length plus one and so never regrows to the cap.
func readCapped(r io.Reader, limit, start int) ([]byte, error) {
	buf := make([]byte, 0, min(start, limit+1)) // min: the start must never exceed the cap

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

// castBufferStart is readCapped's first capacity for r: 64 KiB for a stream,
// and Len+1 for a reader that reports Len, whose bytes are already in memory
// (an 8 MiB session snapshot then buffers 8 MiB, not the 64 MiB cap).
func castBufferStart(r io.Reader) int {
	if l, ok := r.(interface{ Len() int }); ok {
		return max(l.Len()+1, 1)
	}
	return 64 << 10
}

// PGStore saves are admitted against a process-wide payload-buffer budget,
// shared by every PGStore. A save holds one slot from before it buffers a byte
// until its row is sent.
//
//   - streamSaveSlots: saves read from a source that sets its own pace, such
//     as an upload body. There is one, so uploads are written one at a time
//     per process; an upload may regrow its buffer once to the 64 MiB cap.
//     Before it queues for the slot a save queues in its admission group
//     (groupSaves): the run's owner on the upload route, else the run. A
//     group so holds one place, in the slot or waiting for it, and a waiter
//     from another group is behind one holder at most. The slot goes to its
//     waiters in arrival order, so a save arriving as it is released never
//     passes one already waiting.
//   - inMemorySaveSlot: saves whose reader reports Len, so its bytes are
//     already in memory and its read cannot stall: an attach or SSH session
//     snapshot, buffered at its own length. A stream save never takes it, so
//     these never wait behind an upload.
//
// The count follows a measurement, not an estimate: pgx copies the bytea twice
// more while sending it, and the collector lets freed buffers stay resident
// between saves. Repeating one 64 MiB upload together with one 8 MiB session
// save peaked at about 370 MiB resident (VmHWM of a test process, 10 rounds,
// with GOMEMLIMIT=400MiB and without it). Two upload slots peaked between 439
// and 503 MiB under that limit and about 630 MiB without. The chart sets
// GOMEMLIMIT=400MiB under its 512 MiB memory limit.
//
// A save waits at most castSaveAdmitWait to be admitted, and the upload route
// bounds how long an admitted upload's body may take to arrive.
//
// The payload is read whole (bounded) but written in castWriteChunk pieces, so the peak above is one
// payload and a chunk's copies, not three payloads. The slot count was measured before that and is
// still one.
//
// ponytail: one upload at a time; re-measure the resident peak, then allow concurrent uploads again.
const castStreamSlots = 1

// castWriteChunk is how much of a payload one statement sends. A larger one holds more of pgx's copies at
// once; a smaller one makes the database rewrite the growing value more often (each append copies it).
var castWriteChunk = 16 << 20

var (
	streamSaveSlots  = &slotQueue{size: castStreamSlots}
	inMemorySaveSlot = &slotQueue{size: 1}
	groupSavesMu     sync.Mutex
	groupSaves       = map[string]chan struct{}{} // closed when the group's save lets go
)

type admissionGroupKey struct{}

// WithAdmissionGroup names the group ctx's stream saves queue in before they
// queue for the stream slot. The upload route passes the run's owner, so one
// person's uploads wait behind each other, not ahead of everyone else's.
func WithAdmissionGroup(ctx context.Context, group string) context.Context {
	return context.WithValue(ctx, admissionGroupKey{}, group)
}

// AdmissionGroup is the group a stream save of key queues in: the one ctx
// names, else the key's run.
func AdmissionGroup(ctx context.Context, key string) string {
	if g, _ := ctx.Value(admissionGroupKey{}).(string); g != "" {
		return g
	}
	return castRun(key)
}

// castSaveAdmitWait bounds a save's wait for admission. It is longer than one
// full hold (the upload route's 60 s read window plus the write), so a waiter
// behind one holder is admitted; and 65 + 60 = 125 s stays under the proxy's
// 130 s upload timeout.
var castSaveAdmitWait = 65 * time.Second

// castSaveWait runs each time a save finds what it needs held and is about to
// wait. A save waiting for a slot is already queued when it runs.
var castSaveWait = func() {}

// admitCastSave admits a save of key read from r, waiting until it may run or
// ctx ends; a waiter that leaves holds nothing.
func admitCastSave(ctx context.Context, key string, r io.Reader) (release func(), err error) {
	ctx, cancel := context.WithTimeout(ctx, castSaveAdmitWait)
	defer cancel()
	if _, ok := r.(interface{ Len() int }); ok {
		return inMemorySaveSlot.take(ctx)
	}
	releaseGroup, err := holdGroup(ctx, AdmissionGroup(ctx, key))
	if err != nil {
		return nil, err
	}
	releaseSlot, err := streamSaveSlots.take(ctx)
	if err != nil {
		releaseGroup()
		return nil, err
	}
	return func() { releaseSlot(); releaseGroup() }, nil
}

// slotQueue admits up to size holders at once and hands each released slot
// to its oldest waiter. A slot is free only while nobody waits.
type slotQueue struct {
	mu      sync.Mutex
	size    int
	held    int
	waiters []chan struct{} // arrival order; closed when handed a slot
}

// take waits until q hands the caller a slot or ctx ends; a waiter that
// leaves holds nothing.
func (q *slotQueue) take(ctx context.Context) (func(), error) {
	q.mu.Lock()
	if q.held < q.size {
		q.held++
		q.mu.Unlock()
		return q.release, nil
	}
	ready := make(chan struct{})
	q.waiters = append(q.waiters, ready)
	q.mu.Unlock()
	castSaveWait()
	select {
	case <-ready:
		return q.release, nil
	case <-ctx.Done():
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	select {
	case <-ready: // handed a slot as ctx ended: pass it on
		q.handOff()
	default:
		q.waiters = slices.DeleteFunc(q.waiters, func(w chan struct{}) bool { return w == ready })
	}
	return nil, ctx.Err()
}

func (q *slotQueue) release() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.handOff()
}

// handOff passes a held slot to the oldest waiter, or frees it. q.mu is held.
func (q *slotQueue) handOff() {
	if len(q.waiters) == 0 {
		q.held--
		return
	}
	close(q.waiters[0])
	q.waiters = q.waiters[1:]
}

// holdGroup waits until no other stream save of group is admitted or waiting for a slot.
func holdGroup(ctx context.Context, group string) (func(), error) {
	for {
		groupSavesMu.Lock()
		held, busy := groupSaves[group]
		if !busy {
			mine := make(chan struct{})
			groupSaves[group] = mine
			groupSavesMu.Unlock()
			return func() {
				groupSavesMu.Lock()
				delete(groupSaves, group)
				groupSavesMu.Unlock()
				close(mine)
			}, nil
		}
		groupSavesMu.Unlock()
		castSaveWait()
		select {
		case <-held:
		case <-ctx.Done():
			return nil, ctx.Err()
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
	// Admitted before the first byte is buffered, held until the row is sent.
	release, err := admitCastSave(ctx, runID, r)
	if err != nil {
		return err
	}
	defer release()
	// Read fully (bounded), not streamed: pgx sends a bytea parameter as a
	// single []byte, so a too-large cast is rejected before any INSERT.
	data, err := readCapped(contextReader{ctx, r}, maxCastBytes, castBufferStart(r))
	if err != nil {
		return fmt.Errorf("recording: read cast %q: %w", runID, err)
	}
	if len(data) > maxCastBytes {
		return fmt.Errorf("recording: cast %q exceeds %d byte limit", runID, maxCastBytes)
	}
	// Written in castWriteChunk pieces in one transaction: pgx copies a bytea parameter twice more
	// while sending it, so a whole 64 MiB payload in one statement peaked at three copies of it,
	// and a chunk's copies are a fraction. A reader still sees the old row or the whole new one.
	return s.runTx(ctx, runID, false, func(tx pgx.Tx) error {
		first := data[:min(len(data), castWriteChunk)]
		_, err := tx.Exec(ctx, `
		INSERT INTO recordings (cast_key, payload)
		VALUES ($1, $2)
		ON CONFLICT (cast_key) DO UPDATE
			SET payload = EXCLUDED.payload, updated_at = now()`,
			runID, first,
		)
		if err != nil {
			return fmt.Errorf("recording: save cast %q: %w", runID, err)
		}
		for rest := data[len(first):]; len(rest) > 0; rest = rest[min(len(rest), castWriteChunk):] {
			if _, err := tx.Exec(ctx, `UPDATE recordings SET payload = payload || $2 WHERE cast_key = $1`,
				runID, rest[:min(len(rest), castWriteChunk)]); err != nil {
				return fmt.Errorf("recording: save cast %q: %w", runID, err)
			}
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
