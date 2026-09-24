// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// TestPG_TheIdleReaperSkipsAKeptRun: a run its lease ended is RUNNING with its
// agent stopped and its files kept for the ended-run grace. It is not idle, and
// the idle reaper's stop would tear down the files the grace keeps, so the scan
// must not list it. Guarded by WARDYN_TEST_PG.
func TestPG_TheIdleReaperSkipsAKeptRun(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	run := skewedRun(t, store.PG{Pool: pool}, 60)
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET updated_at = now() - interval '2 hours',
		lost_at = now(), lost_reason = 'ended' WHERE id=$1`, run.ID); err != nil {
		t.Fatalf("mark the run kept: %v", err)
	}
	stopper := &recordingStopper{}
	reaper := lifecycle.New(lifecycleStore{pool: pool}, stopper, &fakeAuditRecorder{}, lifecycle.Config{})
	reaper.Tick(ctx)
	if slices.Contains(stopper.stopped, run.ID) {
		t.Fatal("the idle reaper stopped a kept run; the ended-run grace decides when its files go")
	}

	// Negative control: the same run, no longer kept, is reaped.
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = NULL, lost_reason = '' WHERE id=$1`, run.ID); err != nil {
		t.Fatalf("unmark the run: %v", err)
	}
	reaper.Tick(ctx)
	if !slices.Contains(stopper.stopped, run.ID) {
		t.Error("an idle run past its auto-stop was not reaped once it was no longer kept")
	}
}
