// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// heldEndFixture is a run ended one minute ago on a target whose executors answer keeps, whose run lock the
// test holds (released at cleanup), and the time it was lost.
func heldEndFixture(t *testing.T, keeps ...bool) (*leaseFixture, time.Time) {
	t.Helper()
	f := newLeaseFixture(t, -time.Minute)
	f.srv.cfg.Runner = leaseRouter{leaseRunner: f.rn, keeps: keeps}
	if applied, err := f.st.MarkRunEnded(context.Background(), f.run.ID, f.now); err != nil || !applied {
		t.Fatalf("MarkRunEnded = %v, %v; want the claim to land", applied, err)
	}
	_, hold, ok := f.srv.tryLockRunOp(context.Background(), f.run.ID)
	if !ok {
		t.Fatal("could not take the run's lock")
	}
	t.Cleanup(hold)
	return f, f.now
}

// sweepAt is a lease sweep at lostAt plus after.
func (f *leaseFixture) sweepAt(t *testing.T, lostAt time.Time, after time.Duration) {
	t.Helper()
	f.now = lostAt.Add(after)
	f.sweep(t)
}

// A run whose target cannot keep its sandbox, ended while another step holds its lock, is forced to end
// keptEndHoldOffLimit after it was lost, not after its keep grace (7 d here) plus the limit.
func TestLease_UnkeptEndUnderHeldLockIsForcedAfterHoldOff(t *testing.T) {
	f, lostAt := heldEndFixture(t, false)
	f.sweepAt(t, lostAt, time.Minute)
	f.sweepAt(t, lostAt, keptEndHoldOffLimit-time.Second)
	if f.st.State() != types.RunRunning || f.rn.stopCount() != 0 {
		t.Fatalf("state %s, StopSandbox %d inside the hold-off; want RUNNING, 0", f.st.State(), f.rn.stopCount())
	}
	f.sweepAt(t, lostAt, keptEndHoldOffLimit)
	if f.st.State() != types.RunStopped || f.rn.stopCount() != 1 {
		t.Fatalf("state %s, StopSandbox %d at the hold-off; want STOPPED, 1", f.st.State(), f.rn.stopCount())
	}
	forced := 0
	for _, ev := range f.audit.eventsFor(f.run.ID, "run.ended") {
		if leaseAuditData(t, ev)["forced"] == true {
			forced++
		}
	}
	if forced != 1 {
		t.Errorf("%d forced run.ended rows, want 1", forced)
	}
}

// A target that keeps its sandbox still waits for its keep grace, then the limit.
func TestLease_KeptEndUnderHeldLockStillWaits(t *testing.T) {
	f, lostAt := heldEndFixture(t, true)
	grace := f.srv.cfg.EndedRunGrace
	f.sweepAt(t, lostAt, keptEndHoldOffLimit+time.Minute)
	f.sweepAt(t, lostAt, grace+keptEndHoldOffLimit-time.Second)
	if f.st.State() != types.RunRunning || f.rn.stopCount() != 0 {
		t.Fatalf("state %s, StopSandbox %d before keptUntil + the limit; want RUNNING, 0", f.st.State(), f.rn.stopCount())
	}
	f.sweepAt(t, lostAt, grace+keptEndHoldOffLimit)
	if f.st.State() != types.RunStopped || f.rn.stopCount() != 1 {
		t.Fatalf("state %s, StopSandbox %d at keptUntil + the limit; want STOPPED, 1", f.st.State(), f.rn.stopCount())
	}
}

// A target whose answer is not known (executors that disagree) is not forced early.
func TestLease_UnknownTargetUnderHeldLockWaits(t *testing.T) {
	f, lostAt := heldEndFixture(t, false, true)
	f.sweepAt(t, lostAt, keptEndHoldOffLimit+time.Minute)
	f.sweepAt(t, lostAt, f.srv.cfg.EndedRunGrace-time.Minute)
	if f.st.State() != types.RunRunning || f.rn.stopCount() != 0 {
		t.Fatalf("state %s, StopSandbox %d for an unknown target; want RUNNING, 0", f.st.State(), f.rn.stopCount())
	}
}

// resumedRows counts the rows under action that a forced stop's retry wrote for a run it picked up.
func resumedRows(audit *recRecorder, action string) int {
	n := 0
	for _, ev := range audit.snapshot() {
		var d map[string]any
		if ev.Action == action && json.Unmarshal(ev.Data, &d) == nil && d["resumed"] == true {
			n++
		}
	}
	return n
}

// A process that died after the forced claim left the run terminal with its approval pending and no end row.
// The first retry, on a new server, cancels the approval and writes the row; the second writes neither again.
func TestForcedStop_CrashAfterClaimGetsApprovalsAndRow(t *testing.T) {
	f := newForced(t)
	fa := newFakeApprovals()
	seedPendingApproval(t, fa, f.run.ID)
	f.s.cfg.Approvals = fa
	f.claimOnly(t)
	f.sb.stopErr = errors.New("the runner did not answer the stop") // the lock is held, so the mark stays
	restarted := New(f.s.cfg)
	f.tick(t, restarted)
	if calls := fa.cancelledCalls(); len(calls) != 1 || calls[0].Count != 1 || calls[0].Reason != "run_ended" {
		t.Fatalf("approval cancels after the first retry = %+v; want one, of one row, run_ended", calls)
	}
	if n := resumedRows(f.audit, "run.ended"); n != 1 {
		t.Fatalf("%d resumed rows after the first retry, want 1", n)
	}
	if f.leaser.markCount() != 1 {
		t.Fatalf("%d marks after a failed stop, want 1", f.leaser.markCount())
	}
	f.tick(t, restarted)
	if calls := fa.cancelledCalls(); len(calls) != 1 {
		t.Errorf("approval cancels after the second retry = %+v; want still one", calls)
	}
	if n := resumedRows(f.audit, "run.ended"); n != 1 {
		t.Errorf("%d resumed rows after the second retry, want 1", n)
	}
}

// The resumed row is written once per process per run, and a restart writes its own.
func TestForcedStop_ResumedRowOncePerProcess(t *testing.T) {
	f := newForced(t)
	f.claimOnly(t)
	f.sb.stopErr = errors.New("the runner did not answer the stop")
	f.tick(t, f.s)
	f.tick(t, f.s)
	f.tick(t, f.s)
	if n := resumedRows(f.audit, "run.ended"); n != 1 {
		t.Fatalf("%d resumed rows after three ticks, want 1", n)
	}
	f.tick(t, New(f.s.cfg))
	if n := resumedRows(f.audit, "run.ended"); n != 2 {
		t.Errorf("%d resumed rows after a restart's first tick, want 2", n)
	}
}
