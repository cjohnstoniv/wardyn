// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"
)

// TestPauseSettle_ALateFreezeIsLookedAtAgainAfterARestart: a freeze cut at its bound may land after the pause sweep
// has settled its record, and the server that made it may stop before then. The record stays in the database until
// pauseLateSettleAfter past its due_at, and every pass until then settles the run again, on whichever server sweeps:
// the late freeze is thawed back to the row, and the record goes on the first pass after that.
func TestPauseSettle_ALateFreezeIsLookedAtAgainAfterARestart(t *testing.T) {
	old := pauseRuntimeCallTimeout
	pauseRuntimeCallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { pauseRuntimeCallTimeout = old })
	f := newPauseFixture(t, pauseWaitingAfter+time.Minute)
	f.st.open, f.st.waiting = true, true
	f.srv.cfg.Runner = &hungRuntime{pauseRunner: f.rn}
	f.sweep(t)
	if at, _ := f.st.paused(); at != nil || f.st.owing() != 1 {
		t.Fatalf("paused %v, settle records %d; want the cut freeze unmarked and recorded", at != nil, f.st.owing())
	}
	// The server stops. The one that comes back has nothing of it in memory, and its runtime answers.
	f.st.mu.Lock()
	f.st.open, f.st.waiting = false, false
	f.st.mu.Unlock()
	rn := &frozenRunner{pauseRunner: f.rn}
	cfg := f.srv.cfg
	cfg.Runner = rn
	srv := New(cfg)
	pass := func(what string, thaws, owing int) {
		t.Helper()
		if err := srv.sweepRunPauses(context.Background()); err != nil {
			t.Fatalf("sweepRunPauses: %v", err)
		}
		if _, got := rn.counts(); got != thaws || rn.frozen.Load() || f.st.owing() != owing {
			t.Fatalf("%s: thaws %d, frozen %v, settle records %d; want %d thaws, the sandbox running and %d records",
				what, got, rn.frozen.Load(), f.st.owing(), thaws, owing)
		}
	}
	f.st.dueSince(time.Second)
	pass("the first settle", 1, 1)
	if err := rn.FreezeSandbox(context.Background(), f.run.SandboxRef); err != nil { // the cut freeze lands only now
		t.Fatal(err)
	}
	pass("the next pass", 2, 1)
	f.st.settleDue()
	pass("the first pass past the late look", 3, 0)
}

// TestPauseSettle_ACompensationLeavesARecordItDidNotWrite: a record held when a pause begins may be an earlier cut
// call's, whose late look runs until pauseLateSettleAfter past its due_at. A compensation that undoes its pause's
// freeze deletes only a record its pause wrote, and leaves any other to the sweep.
func TestPauseSettle_ACompensationLeavesARecordItDidNotWrite(t *testing.T) {
	f := newPauseFixture(t, time.Hour)
	f.st.run.RunLimits.PauseIdleAfterSec = 60
	f.rn.cpu = pct(0)
	// The freeze opens a request, so the mark fails and the compensation runs.
	f.rn.onFreeze = func() { f.st.open = true }
	if _, err := f.st.NotePauseSettle(context.Background(), f.run.ID, time.Hour); err != nil {
		t.Fatal(err)
	}
	f.sweep(t)
	if freezes, thaws := f.rn.counts(); freezes != 1 || thaws != 1 || f.st.owing() != 1 {
		t.Fatalf("freezes %d thaws %d, settle records %d; want the freeze undone and the record held before the pause kept",
			freezes, thaws, f.st.owing())
	}
	f.st.settleDue()
	f.sweep(t)
	if f.st.owing() != 0 {
		t.Fatalf("settle records %d; want the sweep to settle the record and delete it past its late look", f.st.owing())
	}
}
