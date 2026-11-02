// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newRunnerLostFixture is newLostFixture's run placed on a person's own
// runner, as create binds one (local, with its runner's id), whose agent
// exited 0 with its sandbox still there: what the runner answers the probe for
// a task that finished normally (runner/docker/driver_exec.go AgentStatus).
func newRunnerLostFixture(t *testing.T, interactive bool) *lostFixture {
	t.Helper()
	f := newLostFixture(t)
	id := uuid.New()
	f.st.run.Placement, f.st.run.RunnerID = types.PlacementLocal, &id
	f.st.run.Interactive = interactive
	if !interactive {
		f.st.run.AgentExecID = "exec-1"
	}
	f.run = f.st.run
	code := 0
	f.lr.exitCode = &code
	return f
}

// TestLostRun_AHeadlessRunnerRunIsLeftForItsExitReceipt is #3665. A headless
// run on a runner has no watcher heartbeat after dispatch, so the watcher
// sweep claims it and probes it. When the probe lands between the agent's exit
// and the runner's exit receipt, it must not mark the run lost (reboot): that
// mark would get the receipt refused, and the run would never complete.
func TestLostRun_AHeadlessRunnerRunIsLeftForItsExitReceipt(t *testing.T) {
	left := func(t *testing.T, f *lostFixture) {
		t.Helper()
		if lostAt, reason := f.st.lost(); lostAt != nil {
			t.Fatalf("lost = %v %q, want no mark: the runner's exit receipt ends this run", lostAt, reason)
		}
		if lost := f.audit.eventsFor(f.run.ID, "run.lost"); len(lost) != 0 {
			t.Errorf("run.lost events = %+v, want none", lost)
		}
		if f.st.State() != types.RunRunning || f.rn.endCount() != 0 || f.rn.stopCount() != 0 || f.lr.proxyStopCount() != 0 {
			t.Errorf("state %s, EndSandbox %d, StopSandbox %d, StopProxy %d; want RUNNING, 0, 0, 0",
				f.st.State(), f.rn.endCount(), f.rn.stopCount(), f.lr.proxyStopCount())
		}
	}
	t.Run("the sweep", func(t *testing.T) {
		f := newRunnerLostFixture(t, false)
		f.sweepWatchers(t)
		left(t, f)
	})
	t.Run("the watch", func(t *testing.T) {
		f := newRunnerLostFixture(t, false)
		f.srv.cfg.ExecOutputTailOff = true
		prev := reconcileWatchIntervalNS.Swap(int64(time.Millisecond))
		t.Cleanup(func() { reconcileWatchIntervalNS.Store(prev) })
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if f.srv.reconcileWatch(ctx, f.run.ID, f.run.SandboxRef, f.run.AgentExecID); ctx.Err() != nil {
			t.Fatal("the watcher never acted on the stopped agent")
		}
		left(t, f)
	})
}

// TestLostRun_OnlyAHeadlessRunnerRunSkipsTheRebootMark: an interactive run on
// a runner keeps its reboot detection, and a headless run on the
// organisation's own substrate (placement "" or remote, no runner) is
// finalized by the watcher as before.
func TestLostRun_OnlyAHeadlessRunnerRunSkipsTheRebootMark(t *testing.T) {
	t.Run("an interactive run on a runner", func(t *testing.T) {
		f := newRunnerLostFixture(t, true)
		f.sweepWatchers(t)
		if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostReboot {
			t.Fatalf("lost = %v %q, want the run marked lost (reboot)", lostAt, reason)
		}
		if lost := f.audit.eventsFor(f.run.ID, "run.lost"); len(lost) != 1 || leaseAuditData(t, lost[0])["reason"] != "reboot" {
			t.Errorf("run.lost events = %+v, want one with reason reboot", lost)
		}
		if f.st.State() != types.RunRunning {
			t.Errorf("state %s, want RUNNING (kept)", f.st.State())
		}
	})
	for _, p := range []types.Placement{"", types.PlacementRemote} {
		t.Run("a headless run placed "+string(p)+" with no runner", func(t *testing.T) {
			f := newLostFixture(t)
			f.st.run.Interactive, f.st.run.AgentExecID, f.st.run.Placement = false, "exec-1", p
			f.run = f.st.run
			code := 0
			f.lr.exitCode = &code
			f.sweepWatchers(t)
			if f.st.State() != types.RunCompleted || f.rn.stopCount() != 1 || f.rn.endCount() != 0 {
				t.Errorf("state %s, StopSandbox %d, EndSandbox %d; want COMPLETED, 1, 0", f.st.State(), f.rn.stopCount(), f.rn.endCount())
			}
			if lostAt, _ := f.st.lost(); lostAt != nil {
				t.Errorf("lost at %v, want no mark", lostAt)
			}
		})
	}
}
