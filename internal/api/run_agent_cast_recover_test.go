// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// agentCastRecoveryFixture is agentCastFixture over a substrate that can read the run's output back, with
// the run's capture owed (its pending row written by the process that dispatched it) and its agent cast
// owed (the obligation that dispatch stored), under a requirement.
func agentCastRecoveryFixture(t *testing.T, kind types.MachineKind, rr *recoveringRunner, shape ...func(*Config)) (*outputFixture, *castStore, recording.Store) {
	t.Helper()
	f, cs, casts := agentCastFixture(t, kind, append([]func(*Config){func(c *Config) { c.Runner, c.Broker = rr, &raceBroker{} }}, shape...)...)
	f.mem.rows[f.run.ID] = store.RunOutput{RunID: f.run.ID, Source: "stdout", ClaimedAt: time.Now().Add(-time.Hour)}
	cs.required, cs.owed = true, true
	return f, cs, casts
}

// dropTail is this process forgetting the run's tail, as a restart does: the process that then
// finalises the run holds none.
func dropTail(f *outputFixture) {
	f.srv.execOutputs.mu.Lock()
	delete(f.srv.execOutputs.m, f.run.ID)
	f.srv.execOutputs.mu.Unlock()
}

// notKilled asserts the recording decision never failed or stopped the run.
func notKilled(t *testing.T, f *outputFixture) {
	t.Helper()
	if f.st.FailureHint() != "" || f.st.State() == types.RunFailed {
		t.Fatalf("state %s hint %q: the run was failed for its recording", f.st.State(), f.st.FailureHint())
	}
}

// A restart mid-run: server A dispatched a macOS VM task run at Terminal and held its cast in memory when
// it went; the process that finalises the run with no tail re-reads the runner's copy and stores it as
// <run>~agent-recovered, masked, with recovered: true. The run stays marked recorded and, under a
// requirement, is not failed.
func TestAgentCastRecover_RestartMidRunRecordsTheReRead(t *testing.T) {
	rr := newRecoveringRunner()
	rr.log = "before the restart " + agentCastSecret + "\nafter the restart\n"
	f, cs, casts := agentCastRecoveryFixture(t, types.MachineMacOSVM, rr)
	run, spec, ok := startDispatch(f, types.SiteConfig{}, false)
	tw, _ := spec.ExecOutput.(*tailWriter)
	if !ok || tw == nil || tw.cast == nil {
		t.Fatalf("A did not start the run with its cast (started=%v)", ok)
	}
	tw.BeginDrain()
	_, _ = tw.Write([]byte("before the restart\n"))
	dropTail(f) // A goes before the agent's copy ends: its cast is never saved
	f.st.mu.Lock()
	f.st.state = types.RunRunning
	f.st.mu.Unlock()

	f.srv.reconcileFinalize(t.Context(), run.ID, types.RunCompleted, "sbx-out", "reconciled exit")
	row := waitCastRow(t, f, agentCastRecoveredSession)
	if row["outcome"] != "success" || row["recovered"] != true || row["target"] != recording.CastKey(run.ID.String(), agentCastRecoveredSession) {
		t.Fatalf("row %v, want a recovered success row", row)
	}
	if out := readCast(t, casts, run.ID, agentCastRecoveredSession); !strings.Contains(out, "after the restart") || !strings.Contains(out, "<secret-hidden>") {
		t.Fatalf("the recovered cast %q, want the whole re-read, masked", out)
	}
	waitFor(t, "the obligation to clear", func() bool { return !cs.owing() })
	if rows := castRows(t, f, agentCastSession); len(rows) != 0 || cs.marked() != "" {
		t.Fatalf("agent rows %v, mark %q: want no dispatch cast and the run recorded", rows, cs.marked())
	}
	notKilled(t, f)
}

// Another replica adopts the RUNNING run while the agent still writes: its cast holds the bytes from
// before the adoption and after it, and is saved when the agent exits.
func TestAgentCastRecover_AdoptionRecordsBeforeAndAfter(t *testing.T) {
	prev := reconcileWatchIntervalNS.Swap(int64(20 * time.Millisecond))
	t.Cleanup(func() { reconcileWatchIntervalNS.Store(prev) })
	rr := newRecoveringRunner()
	rr.log, rr.follow, rr.tail = "before the adoption\n", make(chan struct{}), "after, on B\n"
	f, cs, casts := agentCastRecoveryFixture(t, types.MachineMacOSVM, rr)

	go f.srv.reconcileWatch(f.srv.cfg.BaseCtx, f.run.ID, "sbx-out", "exec-1")
	waitFor(t, "B to resume the log", func() bool {
		v, kept := f.srv.readExecOutput(f.run.ID, 1<<20, false)
		return kept && string(v.out) == rr.log
	})
	if rows := castRows(t, f, agentCastRecoveredSession); len(rows) != 0 {
		t.Fatalf("saved before the agent exited: %v", rows)
	}
	close(rr.follow)
	close(rr.exited)
	if row := waitCastRow(t, f, agentCastRecoveredSession); row["outcome"] != "success" || row["recovered"] != true {
		t.Fatalf("row %v, want a recovered success row", row)
	}
	out := readCast(t, casts, f.run.ID, agentCastRecoveredSession)
	if !strings.Contains(out, "before the adoption") || !strings.Contains(out, "after, on B") || cs.marked() != "" {
		t.Fatalf("cast %q mark %q, want both halves and the run recorded", out, cs.marked())
	}
}

// A gapped re-read (the runner had dropped the head): the cast carries the gap marker and its row
// gap_bytes.
func TestAgentCastRecover_GappedReReadSaysSo(t *testing.T) {
	rr := newRecoveringRunner()
	rr.log, rr.gap = "the rest of the output\n", 4096
	f, _, casts := agentCastRecoveryFixture(t, types.MachineMacOSVM, rr)
	f.srv.recoverRunOutput(t.Context(), f.srv.runOutputStore(), f.run, "no_tail")
	row := waitCastRow(t, f, agentCastRecoveredSession)
	if row["outcome"] != "success" || row["gap_bytes"] != float64(4096) {
		t.Fatalf("row %v, want success with gap_bytes 4096", row)
	}
	out := readCast(t, casts, f.run.ID, agentCastRecoveredSession)
	if !strings.Contains(out, outputGapMarker(4096)) || !strings.Contains(out, "the rest of the output") {
		t.Fatalf("cast %q, want the gap marker then the rest", out)
	}
}

// A recovery that gives nothing, under a requirement: the run is not killed, is marked runner_declined,
// and has a session.recording.write failure row for the agent session saying why. No cast is stored.
func TestAgentCastRecover_NothingToReadMarksTheRunAndKeepsItRunning(t *testing.T) {
	for _, tc := range []struct {
		reason string
		err    error
		shape  func(*Config)
		edit   func(*outputFixture)
	}{
		{reason: "unrecoverable", err: runner.ErrOutputUnrecoverable},
		{reason: "sandbox_gone", err: runner.ErrSandboxGone},
		{reason: "runner_offline", err: runner.ErrRunnerOffline},
		{reason: "mask_uncovered", shape: func(c *Config) {
			pool, _ := miscCovClosedPool(t)
			c.MaskManifests = maskmanifest.New(pool, nil, c.MaskRegistry)
		}},
		{reason: "erased", edit: func(f *outputFixture) { f.mem.erased[f.run.ID] = true }},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			rr := newRecoveringRunner()
			rr.log, rr.err = "never read\n", tc.err
			var shape []func(*Config)
			if tc.shape != nil {
				shape = append(shape, tc.shape)
			}
			f, cs, casts := agentCastRecoveryFixture(t, types.MachineMacOSVM, rr, shape...)
			if tc.edit != nil {
				tc.edit(f)
			}
			f.srv.recoverRunOutput(t.Context(), f.srv.runOutputStore(), f.run, "no_tail")
			row := waitCastRow(t, f, agentCastSession)
			if row["outcome"] != "failure" || row["reason"] != tc.reason {
				t.Fatalf("row %v, want a failure row with reason %q", row, tc.reason)
			}
			if cs.marked() != types.NotRecordedRunnerDeclined || cs.owing() {
				t.Fatalf("not_recorded = %q, owed %v, want runner_declined and the obligation settled", cs.marked(), cs.owing())
			}
			if _, err := casts.OpenCast(context.Background(), recording.CastKey(f.run.ID.String(), agentCastRecoveredSession)); err == nil {
				t.Fatal("a cast was stored for a re-read that gave nothing")
			}
			notKilled(t, f)
		})
	}
}

// Replica A's recovery marks the run runner_declined (its runner was offline); replica B's re-read then
// stores agent-recovered, and that save clears the mark: the run is recorded again.
func TestAgentCastRecover_ALaterSaveClearsTheMark(t *testing.T) {
	rr := newRecoveringRunner()
	rr.err = runner.ErrRunnerOffline
	f, cs, casts := agentCastRecoveryFixture(t, types.MachineMacOSVM, rr)
	f.srv.resumeRunOutput(t.Context(), f.run.ID, "sbx-out") // A adopts the run and cannot read it
	waitFor(t, "A to mark the run", func() bool { return cs.marked() == types.NotRecordedRunnerDeclined })
	f.srv.WaitBackground()

	dropTail(f) // B holds no tail for the run
	rr.err, rr.log = nil, "read on B\n"
	run, err := f.srv.cfg.Store.GetRun(t.Context(), f.run.ID)
	if err != nil || run.NotRecorded != types.NotRecordedRunnerDeclined {
		t.Fatalf("the run as B reads it: %q, %v", run.NotRecorded, err)
	}
	f.srv.recoverRunOutput(t.Context(), f.srv.runOutputStore(), run, "no_tail")
	if row := waitCastRow(t, f, agentCastRecoveredSession); row["outcome"] != "success" {
		t.Fatalf("row %v, want B's save", row)
	}
	waitFor(t, "B's save to clear the mark", func() bool { return cs.marked() == "" && !cs.owing() })
	if out := readCast(t, casts, f.run.ID, agentCastRecoveredSession); !strings.Contains(out, "read on B") {
		t.Fatalf("cast %q, want B's re-read", out)
	}
}

// Both casts: A saved agent when the agent's copy ended and went before the output row; B's recovery
// (stale_pending) saves agent-recovered. Two keys, two rows, neither replaced.
func TestAgentCastRecover_BothCastsAreKept(t *testing.T) {
	rr := newRecoveringRunner()
	rr.log = "the whole log, re-read\n"
	f, _, casts := agentCastRecoveryFixture(t, types.MachineMacOSVM, rr)
	_, spec, _ := startDispatch(f, types.SiteConfig{}, false)
	tw := spec.ExecOutput.(*tailWriter)
	tw.BeginDrain()
	_, _ = tw.Write([]byte("the dispatch copy\n"))
	tw.EndDrain(nil)
	if row := waitCastRow(t, f, agentCastSession); row["outcome"] != "success" {
		t.Fatalf("A's row %v, want success", row)
	}
	dropTail(f)
	f.srv.recoverRunOutput(t.Context(), f.srv.runOutputStore(), f.run, "stale_pending")
	if row := waitCastRow(t, f, agentCastRecoveredSession); row["outcome"] != "success" {
		t.Fatalf("B's row %v, want success", row)
	}
	if a, b := readCast(t, casts, f.run.ID, agentCastSession), readCast(t, casts, f.run.ID, agentCastRecoveredSession); !strings.Contains(a, "the dispatch copy") || !strings.Contains(b, "re-read") {
		t.Fatalf("agent %q, agent-recovered %q: want each its own", a, b)
	}
}

// A Linux VM run and a container run: a recovery attaches no cast; a macOS VM task run's does.
func TestAgentCastRecover_OnlyAMacOSVMRunGetsACast(t *testing.T) {
	for kind, want := range map[types.MachineKind]bool{types.MachineMacOSVM: true, types.MachineLinuxVM: false, types.MachineContainer: false} {
		f, _, _ := agentCastRecoveryFixture(t, kind, newRecoveringRunner())
		tw := f.srv.openRecoveryTail(f.run)
		if tw == nil {
			t.Fatalf("%s: no recovery tail", kind)
		}
		if got := tw.cast != nil; got != want || (got && tw.cast.session != agentCastRecoveredSession) {
			t.Errorf("%s: cast attached = %v, want %v", kind, got, want)
		}
		tw.EndDrain(nil)
	}
}
