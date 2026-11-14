// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// Live output chunks and the run_outputs row they owe (migrations 0122 and 0129), against Postgres.

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// failOwedRowInsert fails, from inside its transaction, the statement that writes a run_outputs row.
type failOwedRowInsert struct{ fired atomic.Bool }

func (f *failOwedRowInsert) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !strings.Contains(data.SQL, "INSERT INTO run_outputs") {
		return ctx
	}
	f.fired.Store(true)
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	return ctx
}

func (f *failOwedRowInsert) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// A chunk written for a run with no row comes with the run's pending row, in the same transaction:
// when that insert fails the chunk is not written either. A pending row already there keeps its claim.
func TestPG_RunOutputChunkAppendWritesTheOwedRow(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	count := func(q string, id uuid.UUID) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, q, id).Scan(&n); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}

	run := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	if wrote, err := pg.AppendRunOutputChunk(ctx, run, "r", []byte("out"), 1<<20); err != nil || !wrote {
		t.Fatalf("append: wrote=%v err=%v", wrote, err)
	}
	if n := count(`SELECT count(*) FROM run_outputs WHERE run_id = $1 AND captured_at IS NULL`, run); n != 1 {
		t.Fatalf("%d pending rows after the run's first chunk, want the one it owes", n)
	}

	claimed := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	if _, err := pool.Exec(ctx, `INSERT INTO run_outputs (run_id, source, claimed_at) VALUES ($1, 'stdout', now() - interval '1 hour')`, claimed); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.AppendRunOutputChunk(ctx, claimed, "r", []byte("out"), 1<<20); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM run_outputs WHERE run_id = $1 AND claimed_at < now() - interval '30 minutes'`, claimed); n != 1 {
		t.Fatal("a chunk moved the claim of the pending row already there")
	}

	hook := &failOwedRowInsert{}
	cfg, err := pgxpool.ParseConfig(pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = hook
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(traced.Close)
	failed := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	if _, err := store.NewPG(traced).AppendRunOutputChunk(ctx, failed, "r", []byte("lost with its row"), 1<<20); err == nil || !hook.fired.Load() {
		t.Fatalf("an append whose owed-row insert failed: err=%v fired=%v, want the error", err, hook.fired.Load())
	}
	if n := count(`SELECT count(*) FROM run_output_chunks WHERE run_id = $1`, failed); n != 0 {
		t.Fatalf("%d chunks committed without the row they owe", n)
	}
	if n := count(`SELECT count(*) FROM run_outputs WHERE run_id = $1`, failed); n != 0 {
		t.Fatalf("%d rows after the failed append, want none", n)
	}
}

// Orphaned chunks (no run_outputs row) are listed only for terminal runs that did not end past the
// window, in run id order and within the limit. The chunks of a terminal run that ended past the window
// are deleted whatever its row. A live run's chunks are never listed or deleted, nor are those of a run
// with no end recorded, and an erasure tombstone stays.
func TestPG_OrphanRunOutputChunks(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	const window = 30 * 24 * time.Hour
	// chunked writes one chunk for a new run, keeps or drops the pending row the append wrote with it, and
	// sets the run's state and, when ended is not "", its end that long ago.
	chunked := func(state types.RunState, ended string, keepRow bool) uuid.UUID {
		t.Helper()
		id := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
		if _, err := pg.AppendRunOutputChunk(ctx, id, "r", []byte("chunk"), 1<<20); err != nil {
			t.Fatal(err)
		}
		if !keepRow {
			if _, err := pool.Exec(ctx, `DELETE FROM run_outputs WHERE run_id = $1`, id); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state = $2 WHERE id = $1`, id, string(state)); err != nil {
			t.Fatal(err)
		}
		if ended != "" {
			if _, err := pool.Exec(ctx, `UPDATE agent_runs SET ended_at = now() - $2::interval WHERE id = $1`, id, ended); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	young := chunked(types.RunCompleted, "1 day", false)
	old := chunked(types.RunFailed, "40 days", false)
	oldPending := chunked(types.RunCompleted, "40 days", true)
	youngPending := chunked(types.RunCompleted, "1 day", true)
	live := chunked(types.RunRunning, "", false)
	noEnd := chunked(types.RunKilled, "", false)
	erased := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	if err := pg.EraseRunOutputs(ctx, []uuid.UUID{erased}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state = 'COMPLETED', ended_at = now() - interval '40 days' WHERE id = $1`, erased); err != nil {
		t.Fatal(err)
	}
	sorted := func(ids ...uuid.UUID) []uuid.UUID {
		slices.SortFunc(ids, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
		return ids
	}
	list := func(age time.Duration, limit int) []uuid.UUID {
		t.Helper()
		ids, err := pg.ListOrphanRunOutputChunks(ctx, age, limit)
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}

	if got, want := list(window, 100), sorted(young, noEnd); !slices.Equal(got, want) {
		t.Errorf("orphans inside the window = %v, want %v", got, want)
	}
	if got, want := list(0, 100), sorted(young, old, noEnd); !slices.Equal(got, want) {
		t.Errorf("orphans with no window = %v, want %v", got, want)
	}
	if got, want := list(0, 1), sorted(young, old, noEnd)[:1]; !slices.Equal(got, want) {
		t.Errorf("orphans limited to 1 = %v, want %v", got, want)
	}

	if n, err := pg.DeleteEndedRunOutputChunks(ctx, 0); err != nil || n != 0 {
		t.Fatalf("a delete with no window = %d, %v; want 0, nil", n, err)
	}
	if n, err := pg.DeleteEndedRunOutputChunks(ctx, window); err != nil || n != 2 {
		t.Fatalf("delete past the window = %d, %v; want the 2 chunks of the runs that ended 40 days ago", n, err)
	}
	for name, c := range map[string]struct {
		id   uuid.UUID
		left int
	}{"young": {young, 1}, "old": {old, 0}, "old pending": {oldPending, 0}, "young pending": {youngPending, 1}, "live": {live, 1}, "no end": {noEnd, 1}} {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_output_chunks WHERE run_id = $1`, c.id).Scan(&n); err != nil || n != c.left {
			t.Errorf("%s: %d chunks left (err %v), want %d", name, n, err, c.left)
		}
	}
	var tombstones int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM run_output_erasures WHERE run_id = $1`, erased).Scan(&tombstones); err != nil || tombstones != 1 {
		t.Errorf("%d tombstones (err %v) after the delete, want the erased run's kept", tombstones, err)
	}
}
