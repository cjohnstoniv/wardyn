// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRunLease_WarningThresholdBoundaries pins warnRunEnding's edges exactly:
// `left > threshold` is a skip, so a run whose time-to-end lands EXACTLY on a
// threshold fires it, and one second past does not. It also pins
// dayWarningMinLease's own boundary, which is `<=`: a lease of exactly 48h
// still suppresses the 24-hour warning, and 48h plus one second allows it.
// TestRunLease_EndingSoonWarnings already covers the dedup/re-arm behavior at
// coarse offsets; this covers the comparisons themselves at their edges.
func TestRunLease_WarningThresholdBoundaries(t *testing.T) {
	warned := func(f *leaseFixture) []float64 {
		var out []float64
		for _, ev := range f.audit.eventsFor(f.run.ID, "run.ending_soon") {
			out = append(out, leaseAuditData(t, ev)["threshold_sec"].(float64))
		}
		return out
	}

	// next is the threshold one second past each fires INTO — `left > threshold`
	// only skips THAT threshold, so a run 1s past 10m still lands inside the
	// wider 1h bucket, and 1s past 1h still lands inside 24h. Only 1s past the
	// widest (24h) threshold fires nothing.
	for _, tc := range []struct {
		name      string
		threshold time.Duration
		next      time.Duration // 0 means "nothing fires"
	}{
		{"10 minutes", 10 * time.Minute, time.Hour},
		{"1 hour", time.Hour, 24 * time.Hour},
		{"24 hours", 24 * time.Hour, 0},
	} {
		t.Run(tc.name+" exactly at the threshold fires it", func(t *testing.T) {
			f := newLeaseFixture(t, tc.threshold)
			f.sweep(t)
			got := warned(f)
			if len(got) != 1 || got[0] != tc.threshold.Seconds() {
				t.Fatalf("left == threshold: warnings %v, want exactly one at %v", got, tc.threshold.Seconds())
			}
		})
		t.Run(tc.name+" one second past it stops firing THAT threshold", func(t *testing.T) {
			f := newLeaseFixture(t, tc.threshold+time.Second)
			f.sweep(t)
			got := warned(f)
			switch {
			case tc.next == 0:
				if len(got) != 0 {
					t.Fatalf("left == threshold+1s, past the widest threshold: warnings %v, want none", got)
				}
			case len(got) != 1 || got[0] != tc.next.Seconds():
				t.Fatalf("left == threshold+1s: warnings %v, want exactly the next wider threshold at %v (not %v itself)",
					got, tc.next.Seconds(), tc.threshold.Seconds())
			}
		})
	}

	t.Run("a 48h lease still suppresses the 24h warning", func(t *testing.T) {
		f := newLeaseFixture(t, 24*time.Hour)
		f.st.mu.Lock()
		f.st.run.CreatedAt = f.st.run.EndsAt.Add(-48 * time.Hour)
		f.st.mu.Unlock()
		f.sweep(t)
		if got := warned(f); len(got) != 0 {
			t.Fatalf("lease == 48h exactly: warnings %v, want none (dayWarningMinLease is <=)", got)
		}
	})
	t.Run("a 48h-plus-one-second lease allows the 24h warning", func(t *testing.T) {
		f := newLeaseFixture(t, 24*time.Hour)
		f.st.mu.Lock()
		f.st.run.CreatedAt = f.st.run.EndsAt.Add(-48*time.Hour - time.Second)
		f.st.mu.Unlock()
		f.sweep(t)
		if got := warned(f); len(got) != 1 || got[0] != (24*time.Hour).Seconds() {
			t.Fatalf("lease == 48h+1s: warnings %v, want one at 86400", got)
		}
	})
}

// TestPatchRunLimitsUsesCapturedBounds pins captureRunLimits's own claim (see
// its doc comment): the run's limits, end and wait are a SNAPSHOT taken once
// at create, not a live reference to the ceiling's. captureRunLimits itself
// has no direct test on main today — only exercised indirectly through
// create-path integration tests — so this is the one place proving the copy
// itself: mutating the ceiling AFTER the call must never move the run that
// already captured it, which is what lets PATCH /runs/{id} (run_end_wait.go,
// already covered by TestSetRunEnd_ExtendingIsTheLease/TestSetRunEnd_TheGate)
// clamp against bounds a later profile edit cannot reach.
func TestPatchRunLimitsUsesCapturedBounds(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	ceiling := governanceCeiling{Limits: types.GovernanceLimits{
		RunLimits: types.RunLimits{MaxEndAheadSec: 2 * 86400, DefaultEndSec: 86400, MaxWaitSec: 3600, DefaultWaitSec: 1800},
	}}
	run := &types.AgentRun{CreatedAt: now}
	s := &Server{}
	s.captureRunLimits(run, ceiling)

	wantEnd := now.Add(86400 * time.Second)
	if run.RunLimits != ceiling.Limits.RunLimits {
		t.Fatalf("captured RunLimits = %+v, want a copy of %+v", run.RunLimits, ceiling.Limits.RunLimits)
	}
	if run.EndsAt == nil || !run.EndsAt.Equal(wantEnd) || run.WaitBudgetSec != 1800 {
		t.Fatalf("captured end/wait = %v %d; want %v 1800", run.EndsAt, run.WaitBudgetSec, wantEnd)
	}

	// Mutate the ceiling AFTER capture, the way an admin editing the profile
	// would (tighter here, but the direction does not matter — nothing should
	// reach back into an already-captured run either way).
	ceiling.Limits.RunLimits.MaxEndAheadSec = 3600
	ceiling.Limits.RunLimits.MaxWaitSec = 60
	ceiling.Limits.RunLimits.DefaultWaitSec = 30

	if run.RunLimits.MaxEndAheadSec != 2*86400 || run.RunLimits.MaxWaitSec != 3600 {
		t.Fatalf("run.RunLimits after the ceiling was edited = %+v; want it unchanged — captured once, not a live reference",
			run.RunLimits)
	}
	if run.WaitBudgetSec != 1800 {
		t.Fatalf("run.WaitBudgetSec after the ceiling was edited = %d, want the still-captured 1800", run.WaitBudgetSec)
	}

	// End to end: a run whose CAPTURED limits are looser than a fixture's
	// fresh RunLimits{} must still get the wider room on PATCH — proving the
	// handler reads run.RunLimits, never any live profile.
	f := newEndWaitFixture(t, run.RunLimits)
	code, out := f.patch(t, ownerSession(t), endsAtBody(f.now.Add(36*time.Hour)))
	if code != 200 || out.EndsAt == nil || !out.EndsAt.Equal(f.now.Add(36*time.Hour)) {
		t.Fatalf("extend within the captured 2-day max = %d %+v; want 200 at 36h", code, out)
	}
}
