// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The run_outputs rules the finalisation contract leans on, against Postgres:
// pending then final, a gap that never replaces a final row, the incomplete mark
// that only a final row takes, tombstones that stop every writer and reader, an
// erasure that is all or nothing, and the sweeper's two queries.
func TestPG_RunOutputs(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID

	if err := pg.InsertPendingRunOutput(ctx, run); err != nil {
		t.Fatal(err)
	}
	got, found, err := pg.GetRunOutput(ctx, run)
	if err != nil || !found || got.CapturedAt != nil || got.Source != "stdout" {
		t.Fatalf("pending row = %+v found=%v err=%v, want a pending stdout row", got, found, err)
	}
	if marked, _ := pg.MarkRunOutputIncomplete(ctx, run); marked {
		t.Error("a pending row took the incomplete mark")
	}

	final := store.RunOutput{RunID: run, Output: []byte("a\x00b \xff"), Truncated: true, Source: "stdout", MaskScope: "run"}
	if err := pg.SaveFinalRunOutput(ctx, final); err != nil {
		t.Fatal(err)
	}
	got, _, _ = pg.GetRunOutput(ctx, run)
	if got.CapturedAt == nil || string(got.Output) != "a\x00b \xff" || !got.Truncated || got.MaskScope != "run" || got.CaptureGap || got.Incomplete {
		t.Fatalf("final row = %+v, want the bytes (NUL and all) and the flags back", got)
	}
	if wrote, err := pg.SaveGapRunOutput(ctx, run); err != nil || wrote {
		t.Fatalf("a gap over a final row: wrote=%v err=%v, want it refused", wrote, err)
	}
	if marked, err := pg.MarkRunOutputIncomplete(ctx, run); err != nil || !marked {
		t.Fatalf("incomplete mark on the final row: %v %v", marked, err)
	}
	if got, _, _ = pg.GetRunOutput(ctx, run); !got.Incomplete || string(got.Output) != "a\x00b \xff" {
		t.Fatalf("after the mark: %+v", got)
	}

	// A gap replaces a pending row, and a final write replaces a gap.
	run2 := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	_ = pg.InsertPendingRunOutput(ctx, run2)
	if wrote, err := pg.SaveGapRunOutput(ctx, run2); err != nil || !wrote {
		t.Fatalf("gap over a pending row: %v %v", wrote, err)
	}
	if err := pg.SaveFinalRunOutput(ctx, store.RunOutput{RunID: run2, Output: []byte("late arrival"), Source: "stdout"}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ = pg.GetRunOutput(ctx, run2); got.CaptureGap || string(got.Output) != "late arrival" {
		t.Fatalf("a final write after a gap = %+v, want the real bytes and no gap", got)
	}

	// An erasure is all or nothing: an unknown run id fails the tombstone insert
	// (the foreign key) after the first tombstone was written, and nothing stays.
	if err := pg.EraseRunOutputs(ctx, []uuid.UUID{run, uuid.New()}); err == nil {
		t.Fatal("an erasure naming a run that does not exist succeeded")
	}
	if _, found, err := pg.GetRunOutput(ctx, run); err != nil || !found {
		t.Fatalf("the failed erasure left the row %v / %v, want it intact and readable", found, err)
	}
	if err := pg.EraseRunOutputs(ctx, []uuid.UUID{run, run2}); err != nil {
		t.Fatal(err)
	}
	for name, call := range map[string]func() error{
		"pending":  func() error { return pg.InsertPendingRunOutput(ctx, run) },
		"refresh":  func() error { return pg.RefreshRunOutputClaim(ctx, run) },
		"final":    func() error { return pg.SaveFinalRunOutput(ctx, final) },
		"gap":      func() error { _, err := pg.SaveGapRunOutput(ctx, run); return err },
		"mark":     func() error { _, err := pg.MarkRunOutputIncomplete(ctx, run); return err },
		"read":     func() error { _, _, err := pg.GetRunOutput(ctx, run); return err },
		"re-erase": func() error { return pg.EraseRunOutputs(ctx, []uuid.UUID{run}) },
	} {
		err := call()
		if name == "re-erase" {
			if err != nil {
				t.Errorf("erasing twice: %v, want it idempotent", err)
			}
			continue
		}
		if !errors.Is(err, store.ErrRunOutputErased) {
			t.Errorf("%s after the erasure: %v, want ErrRunOutputErased", name, err)
		}
	}
	var rows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM run_outputs WHERE run_id = ANY($1)`, []uuid.UUID{run, run2}).Scan(&rows)
	if rows != 0 {
		t.Fatalf("%d rows left after the erasure", rows)
	}
}

// The sweeper's queries: only final rows past the window go, and only a terminal
// run's pending row with a stale claim is listed.
func TestPG_RunOutputs_RetentionAndStalePending(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	mk := func(state types.RunState) uuid.UUID { return persistRun(t, ctx, pool, newRun(state)).ID }
	old, recent, stale, fresh, live := mk(types.RunCompleted), mk(types.RunCompleted), mk(types.RunFailed), mk(types.RunFailed), mk(types.RunRunning)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO run_outputs (run_id, source, captured_at) VALUES ($1, 'stdout', now() - interval '31 days'), ($2, 'stdout', now() - interval '1 day')`, old, recent)
	exec(`INSERT INTO run_outputs (run_id, source, claimed_at) VALUES ($1, 'stdout', now() - interval '6 minutes'), ($2, 'stdout', now() - interval '1 minute'), ($3, 'stdout', now() - interval '6 minutes')`, stale, fresh, live)

	ids, err := pg.ListStalePendingRunOutputs(ctx, 5*time.Minute, 10)
	if err != nil || len(ids) != 1 || ids[0] != stale {
		t.Fatalf("stale pending = %v, %v; want only the terminal run's stale row", ids, err)
	}
	n, err := pg.DeleteRunOutputsOlderThan(ctx, 30*24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("deleted %d, %v; want exactly the 31-day-old final row", n, err)
	}
	var left int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM run_outputs`).Scan(&left)
	if left != 4 {
		t.Fatalf("%d rows left, want 4 (the recent final row and the three pending ones)", left)
	}
}
