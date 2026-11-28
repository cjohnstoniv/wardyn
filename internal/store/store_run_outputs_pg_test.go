// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
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

// A complete stdout capture replaces an incomplete one of the same mask scope, and only that write reports it;
// nothing else that v0.10.2 refused is let through.
func TestPeerCompleteFinalReplacesIncomplete_PG(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	save := func(o store.RunOutput) bool {
		t.Helper()
		wctx, rep := store.WithReplacedIncomplete(ctx)
		if err := pg.SaveFinalRunOutput(wctx, o); err != nil {
			t.Fatal(err)
		}
		return rep.Replaced
	}
	read := func(run uuid.UUID) store.RunOutput {
		t.Helper()
		got, found, err := pg.GetRunOutput(ctx, run)
		if err != nil || !found {
			t.Fatalf("row of %s: found=%v err=%v", run, found, err)
		}
		return got
	}
	newPending := func() uuid.UUID {
		t.Helper()
		run := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
		if err := pg.InsertPendingRunOutput(ctx, run); err != nil {
			t.Fatal(err)
		}
		return run
	}

	run := newPending()
	if save(store.RunOutput{RunID: run, Output: []byte("part"), Incomplete: true, Source: "stdout", MaskScope: "run"}) {
		t.Error("an incomplete final over a pending row reported a replaced incomplete row")
	}
	if !save(store.RunOutput{RunID: run, Output: []byte("part and rest"), Source: "stdout", MaskScope: "run"}) {
		t.Error("the complete final over the incomplete row did not report the replacement")
	}
	if got := read(run); string(got.Output) != "part and rest" || got.Incomplete {
		t.Fatalf("after the complete write: %q incomplete=%v, want the complete bytes", got.Output, got.Incomplete)
	}
	if save(store.RunOutput{RunID: run, Output: []byte("part"), Incomplete: true, Source: "stdout", MaskScope: "run"}) {
		t.Error("an incomplete final over the complete row reported a replacement")
	}
	if got := read(run); string(got.Output) != "part and rest" || got.Incomplete {
		t.Fatalf("an incomplete final over the complete row changed it: %q incomplete=%v", got.Output, got.Incomplete)
	}

	run2 := newPending()
	save(store.RunOutput{RunID: run2, Output: []byte("part"), Incomplete: true, Source: "stdout", MaskScope: "run"})
	for name, o := range map[string]store.RunOutput{
		"empty scope":   {RunID: run2, Output: []byte("other scope"), Source: "stdout"},
		"pane snapshot": {RunID: run2, Output: []byte("pane"), Source: "pane_snapshot", MaskScope: "run"},
	} {
		if save(o) {
			t.Errorf("%s: reported a replacement", name)
		}
		if got := read(run2); string(got.Output) != "part" || !got.Incomplete {
			t.Errorf("%s: replaced the incomplete row: %q incomplete=%v", name, got.Output, got.Incomplete)
		}
	}

	run3 := newPending()
	save(store.RunOutput{RunID: run3, Output: []byte("part"), Incomplete: true, Source: "stdout"})
	if !save(store.RunOutput{RunID: run3, Output: []byte("all of it"), Source: "stdout"}) {
		t.Error("a complete final with no scope over an incomplete one with no scope did not report the replacement")
	}
	if got := read(run3); string(got.Output) != "all of it" || got.Incomplete {
		t.Fatalf("no-scope replacement: %q incomplete=%v", got.Output, got.Incomplete)
	}

	run4 := newPending()
	save(store.RunOutput{RunID: run4, Output: []byte("part"), Incomplete: true, Source: "stdout", MaskScope: "globals_only"})
	if save(store.RunOutput{RunID: run4, Output: []byte("all of it"), Source: "stdout", MaskScope: "run"}) {
		t.Error("a run-scope write over a globals_only row reported a replacement")
	}
	if got := read(run4); string(got.Output) != "part" || got.MaskScope != "globals_only" {
		t.Fatalf("a run-scope write replaced a globals_only row: %q scope=%q", got.Output, got.MaskScope)
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

	ids, _, err := pg.ListStalePendingRunOutputs(ctx, 5*time.Minute, nil, 10)
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

// More than a page of terminal runs' stale pending rows that stay pending, as rows whose resolve fails on
// every pass do: a cursor walk reaches every one, oldest claim first with the run id breaking a tie, leaves
// out a fresh claim, and ends on a short page with a nil cursor, so the next walk starts again from the oldest.
func TestPG_ListStalePendingRunOutputsCursor(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	pending := func(id uuid.UUID, minutes int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO run_outputs (run_id, source, claimed_at)
			VALUES ($1, 'stdout', now() - make_interval(mins => $2))`, id, minutes); err != nil {
			t.Fatal(err)
		}
	}
	var want []uuid.UUID
	for _, minutes := range []int{60, 50, 50, 40, 30} {
		id := persistRun(t, ctx, pool, newRun(types.RunFailed)).ID
		pending(id, minutes)
		want = append(want, id)
	}
	// One statement sets the tie: now() is per statement, so two pending() calls never give an equal claim.
	if _, err := pool.Exec(ctx, `UPDATE run_outputs SET claimed_at = now() - interval '50 minutes' WHERE run_id IN ($1, $2)`, want[1], want[2]); err != nil {
		t.Fatal(err)
	}
	if bytes.Compare(want[2][:], want[1][:]) < 0 { // the two 50-minute claims: the run id orders them
		want[1], want[2] = want[2], want[1]
	}
	pending(persistRun(t, ctx, pool, newRun(types.RunFailed)).ID, 1) // claimed a minute ago: not stale

	var walked []uuid.UUID
	var after *store.SweepCursor
	for i := 0; i < 5; i++ {
		page, next, err := pg.ListStalePendingRunOutputs(ctx, 5*time.Minute, after, 2)
		if err != nil {
			t.Fatal(err)
		}
		walked = append(walked, page...)
		if next == nil {
			break
		}
		after = next
	}
	if !slices.Equal(walked, want) {
		t.Fatalf("a cursor walk of pages of 2 = %v, want %v", walked, want)
	}
	if first, _, err := pg.ListStalePendingRunOutputs(ctx, 5*time.Minute, nil, 2); err != nil || len(first) != 2 || first[0] != want[0] {
		t.Fatalf("a walk from no cursor = %v, %v; want it to start again at the oldest", first, err)
	}
}
