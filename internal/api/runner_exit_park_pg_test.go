// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/retrylog"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/google/uuid"
)

// A receipt the organisation refuses on every try (its runner was revoked) is not tried again before its
// doubling delay, on the sweep's fake clock, and is parked on the runnerExitParkAfter-th refusal: the row
// is kept with parked_at set, one runner.exit.park row is audited, and no later sweep lists or retries it.
func TestRunnerExitPG_RefusedReceiptBacksOffThenParks(t *testing.T) {
	pool, cfg, rn, run, exit, probe := runnerExitAPIFixture(t)
	ctx := t.Context()
	st := cfg.Store.(store.PG)
	now := time.Now()
	cfg.Now = func() time.Time { return now }
	srv := New(cfg)
	event := runnerwire.Event{Kind: runnerwire.EventAgentExit, RunID: &run.ID, Data: mustJSON(exit)}
	if err := srv.ExitFromRunner(ctx, rn.ID, event); err != nil {
		t.Fatal(err)
	}
	if err := st.BindRunnerExecution(ctx, rn.ID, run.ID, exit.Generation, exit.ExecID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runners SET state='revoked',revoked_at=clock_timestamp() WHERE id=$1`, rn.ID); err != nil {
		t.Fatal(err)
	}
	for try := 1; try < runnerExitParkAfter; try++ {
		if err := srv.SweepRunnerExits(ctx); err == nil {
			t.Fatalf("try %d: the refused receipt was not tried", try)
		}
		now = now.Add(retrylog.Delay(try) - time.Second)
		if err := srv.SweepRunnerExits(ctx); err != nil {
			t.Fatalf("try %d: retried a second before its %s delay: %v", try, retrylog.Delay(try), err)
		}
		now = now.Add(time.Second)
	}
	if err := srv.SweepRunnerExits(ctx); err != nil {
		t.Fatalf("the parking try failed the sweep: %v", err)
	}
	var parked bool
	var audits int
	if err := pool.QueryRow(ctx, `SELECT parked_at IS NOT NULL FROM runner_execution_exits WHERE run_id=$1`, run.ID).Scan(&parked); err != nil || !parked {
		t.Fatalf("receipt parked=%v (%v), want kept and parked", parked, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='runner.exit.park'`, run.ID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("%d runner.exit.park rows (%v), want 1", audits, err)
	}
	if rows, err := st.ListRunnerExitWork(ctx, uuid.Nil); err != nil || len(rows) != 0 {
		t.Fatalf("a parked receipt is still exit work: %d rows, %v", len(rows), err)
	}
	now = now.Add(time.Hour)
	if err := srv.SweepRunnerExits(ctx); err != nil {
		t.Fatalf("a sweep after parking: %v", err)
	}
	if n, err := st.ParkedRunnerExits(ctx, rn.ID); err != nil || n != 1 {
		t.Fatalf("the runner counts %d parked receipts (%v), want 1", n, err)
	}
	if probe.calls.Load() != 0 {
		t.Fatal("a refused receipt reached teardown")
	}
	assertRunnerExitPhase(t, pool, run.ID, false, false, 0)
	srv.WaitBackground()
}
