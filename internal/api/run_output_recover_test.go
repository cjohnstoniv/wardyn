// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// recoveringRunner is a substrate that can read a run's output back. What it
// gives is scripted: log is what the substrate holds when the read starts; err
// ends the read at once (ErrOutputUnrecoverable, ErrSandboxGone); follow, when
// set, keeps the read open, like a log follow, until it closes and then writes
// tail; hang streams until ctx ends (hangDeaf: until release closes, ignoring
// ctx). The agent reports running until exited closes. execCalls (the embedded
// fakeRunner's) is what proves nothing was re-run.
type recoveringRunner struct {
	*outputRunner
	log      string
	tail     string
	err      error
	follow   chan struct{}
	hang     bool
	hangDeaf bool
	release  chan struct{}
	exited   chan struct{}

	rmu      sync.Mutex
	recovers int
	stops    int
}

func newRecoveringRunner() *recoveringRunner {
	return &recoveringRunner{outputRunner: &outputRunner{fakeRunner: &fakeRunner{}}, exited: make(chan struct{}), release: make(chan struct{})}
}

func (r *recoveringRunner) RecoverOutput(ctx context.Context, _ string, w io.Writer) error {
	r.rmu.Lock()
	r.recovers++
	r.rmu.Unlock()
	if r.err != nil {
		return r.err
	}
	_, _ = io.WriteString(w, r.log)
	switch {
	case r.hangDeaf:
		<-r.release
		return nil
	case r.hang:
		<-ctx.Done()
		return ctx.Err()
	case r.follow != nil:
		select {
		case <-r.follow:
		case <-ctx.Done():
			return ctx.Err()
		}
		_, _ = io.WriteString(w, r.tail)
	}
	return nil
}

func (r *recoveringRunner) AgentStatus(context.Context, string, string) (runner.Status, error) {
	select {
	case <-r.exited:
		code := 0
		return runner.Status{State: types.RunStopped, ExitCode: &code}, nil
	default:
		return runner.Status{State: types.RunRunning}, nil
	}
}

func (r *recoveringRunner) StopSandbox(context.Context, string) error {
	r.rmu.Lock()
	r.stops++
	r.rmu.Unlock()
	return nil
}

func (r *recoveringRunner) recoverCount() int { r.rmu.Lock(); defer r.rmu.Unlock(); return r.recovers }
func (r *recoveringRunner) stopCount() int    { r.rmu.Lock(); defer r.rmu.Unlock(); return r.stops }

// recoveryFixture is a server that restarted: it holds no tail for a RUNNING
// run whose capture is owed (its pending row was written by the process that
// dispatched it), over a substrate that can give the output back.
func recoveryFixture(t *testing.T, rr *recoveringRunner) (*outputFixture, *raceBroker) {
	t.Helper()
	brk := &raceBroker{}
	f := newOutputFixture(t, func(c *Config) { c.Runner, c.Broker = rr, brk })
	f.mem.rows[f.run.ID] = store.RunOutput{RunID: f.run.ID, Source: "stdout", ClaimedAt: time.Now().Add(-time.Hour)}
	return f, brk
}

// gapReason is the audited reason of a run's capture-gap row.
func (f *outputFixture) gapReason(t *testing.T) string {
	t.Helper()
	for _, ev := range f.audit.eventsFor(f.run.ID, "run.output.finalize") {
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		if d["capture_gap"] == true {
			reason, _ := d["reason"].(string)
			return reason
		}
	}
	t.Fatal("no capture-gap audit row")
	return ""
}

// A -> crash -> B: B adopts a run that is still running, with output from before
// the handoff, during it, and after it. The Kubernetes-shaped substrate gives
// the whole log back from its start and keeps following it, so B persists all
// three, once; nothing was re-run.
func TestRecoverRunOutput_LiveAdoptionResumesTheFollow(t *testing.T) {
	prev := reconcileWatchIntervalNS.Swap(int64(20 * time.Millisecond))
	t.Cleanup(func() { reconcileWatchIntervalNS.Store(prev) })
	rr := newRecoveringRunner()
	rr.log, rr.follow, rr.tail = "before the crash\nduring the handoff\n", make(chan struct{}), "after, on B\n"
	f, brk := recoveryFixture(t, rr)

	go f.srv.reconcileWatch(f.srv.cfg.BaseCtx, f.run.ID, "sbx-out", "exec-1")
	waitFor(t, "B to resume the log", func() bool {
		v, kept := f.srv.readExecOutput(f.run.ID, 1<<20, false)
		return kept && string(v.out) == rr.log
	})
	if code, got := f.get(t); code != 200 || got.Output != rr.log || got.Complete {
		t.Fatalf("live read on B = %d %+v, want the recovered bytes so far, not complete", code, got)
	}
	close(rr.follow) // the agent prints more, then exits
	close(rr.exited)

	row := f.finalRow(t)
	if want := "before the crash\nduring the handoff\nafter, on B\n"; string(row.Output) != want || row.Incomplete || row.CaptureGap || row.Truncated {
		t.Fatalf("row %+v (%q), want %q, complete", row, row.Output, want)
	}
	f.srv.FinishRunOutput(t.Context(), f.run.ID)
	f.srv.WaitBackground()
	if n, execs, saves := rr.recoverCount(), rr.execCount(), f.mem.saveCount(f.run.ID); n != 1 || execs != 0 || saves != 1 {
		t.Fatalf("%d reads, %d execs, %d writes, want 1, 0, 1: the output is read once and the agent is never re-run", n, execs, saves)
	}
	if brk.revocations(f.run.ID) != 1 || rr.stopCount() != 1 {
		t.Fatalf("revocations %d, stops %d, want the revoke cascade and the teardown once each", brk.revocations(f.run.ID), rr.stopCount())
	}
}

// A run that has already ended when a process finalises it (the reconciler found
// the agent gone) is re-read to its end, once, and its row carries all of it.
func TestRecoverRunOutput_FinalisedRunIsReadBack(t *testing.T) {
	rr := newRecoveringRunner()
	rr.log = "line one\nline two\n"
	f, _ := recoveryFixture(t, rr)
	f.srv.reconcileFinalize(t.Context(), f.run.ID, types.RunCompleted, "sbx-out", "reconciled exit")
	row := f.finalRow(t)
	if string(row.Output) != rr.log || row.Incomplete || row.CaptureGap || row.Source != "stdout" {
		t.Fatalf("row %+v, want the whole log, complete", row)
	}
	f.srv.FinishRunOutput(t.Context(), f.run.ID)
	if rr.recoverCount() != 1 || f.mem.saveCount(f.run.ID) != 1 {
		t.Fatalf("%d reads and %d writes, want one each: a second finish must not re-read or duplicate", rr.recoverCount(), f.mem.saveCount(f.run.ID))
	}
	if code, got := f.get(t); code != 200 || got.Output != rr.log || !got.Complete {
		t.Fatalf("read = %d %+v, want the recovered row", code, got)
	}
}

// A Docker exec agent keeps no log to recover: the row is a capture gap, whether
// the run is finalised after the restart or adopted live and ends later, and the
// agent is never re-run.
func TestRecoverRunOutput_UnrecoverableIsACaptureGapNeverARerun(t *testing.T) {
	for name, drive := range map[string]func(t *testing.T, f *outputFixture, rr *recoveringRunner){
		"finalised": func(t *testing.T, f *outputFixture, _ *recoveringRunner) {
			f.srv.reconcileFinalize(t.Context(), f.run.ID, types.RunCompleted, "sbx-out", "reconciled exit")
		},
		"adopted live": func(t *testing.T, f *outputFixture, rr *recoveringRunner) {
			prev := reconcileWatchIntervalNS.Swap(int64(20 * time.Millisecond))
			t.Cleanup(func() { reconcileWatchIntervalNS.Store(prev) })
			go f.srv.reconcileWatch(f.srv.cfg.BaseCtx, f.run.ID, "sbx-out", "exec-1")
			waitFor(t, "B to ask the substrate", func() bool { return rr.recoverCount() == 1 })
			close(rr.exited)
		},
	} {
		t.Run(name, func(t *testing.T) {
			rr := newRecoveringRunner()
			rr.err = runner.ErrOutputUnrecoverable
			f, _ := recoveryFixture(t, rr)
			drive(t, f, rr)
			row := f.finalRow(t)
			if !row.CaptureGap || len(row.Output) != 0 || row.Incomplete {
				t.Fatalf("row %+v, want a capture gap with no bytes", row)
			}
			f.srv.WaitBackground()
			if rr.execCount() != 0 || rr.recoverCount() != 1 || f.srv.tailFor(f.run.ID) != nil {
				t.Fatalf("%d execs, %d reads, tail held %v, want 0, 1, released", rr.execCount(), rr.recoverCount(), f.srv.tailFor(f.run.ID) != nil)
			}
			if got := f.gapReason(t); got != "unrecoverable" {
				t.Errorf("gap reason %q, want unrecoverable", got)
			}
			if code, got := f.get(t); code != 200 || !got.CaptureGap || !got.Complete || got.Output != "" {
				t.Fatalf("read = %d %+v, want a complete capture gap", code, got)
			}
		})
	}
}

// A sandbox that is already gone, and a runner that cannot read output back at
// all, are capture gaps too.
func TestRecoverRunOutput_GoneOrUnsupportedIsACaptureGap(t *testing.T) {
	t.Run("sandbox gone", func(t *testing.T) {
		rr := newRecoveringRunner()
		rr.err = runner.ErrSandboxGone
		f, _ := recoveryFixture(t, rr)
		f.srv.reconcileFinalize(t.Context(), f.run.ID, types.RunFailed, "sbx-out", "reconciled exit")
		if row := f.finalRow(t); !row.CaptureGap || len(row.Output) != 0 {
			t.Fatalf("row %+v, want a capture gap", row)
		}
		if got := f.gapReason(t); got != "sandbox_gone" {
			t.Errorf("gap reason %q, want sandbox_gone", got)
		}
	})
	t.Run("the run has no sandbox", func(t *testing.T) {
		rr := newRecoveringRunner()
		f, _ := recoveryFixture(t, rr)
		f.st.mu.Lock()
		f.st.run.SandboxRef = ""
		f.st.mu.Unlock()
		f.srv.reconcileFinalize(t.Context(), f.run.ID, types.RunFailed, "", "reconciled exit")
		if row := f.finalRow(t); !row.CaptureGap {
			t.Fatalf("row %+v, want a capture gap", row)
		}
		if rr.recoverCount() != 0 {
			t.Errorf("%d reads of a run with no sandbox, want none", rr.recoverCount())
		}
	})
}

// A log that streams without end is cut at the recovery bound, and the row is
// incomplete with what was read; the revoke cascade and the teardown still ran
// inside reconcileFinalizeTimeout. A runner that ignores its context cannot hold
// the finaliser past the bound either.
func TestRecoverRunOutput_EndlessLogIsCutAtTheBound(t *testing.T) {
	for name, shape := range map[string]func(*recoveringRunner){
		"honours ctx": func(r *recoveringRunner) { r.hang = true },
		"ignores ctx": func(r *recoveringRunner) { r.hangDeaf = true },
	} {
		t.Run(name, func(t *testing.T) {
			rr := newRecoveringRunner()
			rr.log = "partial\n"
			shape(rr)
			t.Cleanup(func() { close(rr.release) })
			f, brk := recoveryFixture(t, rr)
			f.srv.runOutputRecoverWaitOverride = 60 * time.Millisecond
			start := time.Now()
			f.srv.reconcileFinalize(t.Context(), f.run.ID, types.RunFailed, "sbx-out", "reconciled exit")
			if took := time.Since(start); took > 10*time.Second {
				t.Fatalf("the finalize took %s", took)
			}
			row := f.finalRow(t)
			if !row.Incomplete || row.CaptureGap || string(row.Output) != "partial\n" {
				t.Fatalf("row %+v (%q), want incomplete with the bytes read", row, row.Output)
			}
			if brk.revocations(f.run.ID) != 1 || rr.stopCount() != 1 {
				t.Fatalf("revocations %d, stops %d, want the cascade and the teardown to have run", brk.revocations(f.run.ID), rr.stopCount())
			}
			if !f.audit.has(f.run.ID, "run.output.finalize", "failure") {
				t.Error("an incomplete capture is not audited")
			}
		})
	}
}

// The bound is the spec's, and it fits inside the finalize's own.
func TestRunOutputRecoverWait_ProductionValue(t *testing.T) {
	if runOutputRecoverWait != 10*time.Second {
		t.Fatalf("runOutputRecoverWait = %s, want 10s", runOutputRecoverWait)
	}
	if runOutputRecoverWait+runOutputDrainWait >= reconcileFinalizeTimeout {
		t.Fatalf("recovery %s + drain %s does not leave room in reconcileFinalizeTimeout %s", runOutputRecoverWait, runOutputDrainWait, reconcileFinalizeTimeout)
	}
}

// The sweeper resolves a terminal run's abandoned pending row by recovery when
// the sandbox still answers, and to a capture gap when it does not; a run with no
// sandbox is never read.
func TestSweepRunOutputs_RecoversStalePendingRows(t *testing.T) {
	t.Run("recoverable", func(t *testing.T) {
		rr := newRecoveringRunner()
		rr.log = "what the substrate kept\n"
		f, _ := recoveryFixture(t, rr)
		f.st.mu.Lock()
		f.st.state = types.RunFailed
		f.st.mu.Unlock()
		if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
			t.Fatal(err)
		}
		if row, _ := f.mem.row(f.run.ID); string(row.Output) != rr.log || row.CaptureGap || row.CapturedAt == nil || row.Incomplete {
			t.Fatalf("row %+v, want the stale pending row resolved by recovery", row)
		}
		if err := f.srv.SweepRunOutputs(t.Context()); err != nil || rr.recoverCount() != 1 {
			t.Fatalf("second sweep: err %v, %d reads, want a resolved row left alone", err, rr.recoverCount())
		}
	})
	t.Run("a live run is left alone", func(t *testing.T) {
		rr := newRecoveringRunner()
		f, _ := recoveryFixture(t, rr)
		if err := f.srv.SweepRunOutputs(t.Context()); err != nil || rr.recoverCount() != 0 {
			t.Fatalf("err %v, %d reads, want none for a run that is still live", err, rr.recoverCount())
		}
	})
	t.Run("unrecoverable and sandbox-less", func(t *testing.T) {
		for name, mutate := range map[string]func(*recoveringRunner, *outputFixture){
			"unrecoverable": func(rr *recoveringRunner, _ *outputFixture) { rr.err = runner.ErrOutputUnrecoverable },
			"no sandbox": func(_ *recoveringRunner, f *outputFixture) {
				f.st.mu.Lock()
				f.st.run.SandboxRef = ""
				f.st.mu.Unlock()
			},
		} {
			rr := newRecoveringRunner()
			f, _ := recoveryFixture(t, rr)
			mutate(rr, f)
			f.st.mu.Lock()
			f.st.state = types.RunFailed
			f.st.mu.Unlock()
			if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
				t.Fatal(err)
			}
			if row, _ := f.mem.row(f.run.ID); !row.CaptureGap || row.CapturedAt == nil {
				t.Errorf("%s: row %+v, want a capture gap", name, row)
			}
		}
	})
}

// Recovery never replaces a final row and never reads for an erased run.
func TestRecoverRunOutput_NeverReplacesAFinalRowOrReadsForAnErasedRun(t *testing.T) {
	rr := newRecoveringRunner()
	rr.log = "must not be read\n"
	f, _ := recoveryFixture(t, rr)
	if err := f.mem.SaveFinalRunOutput(t.Context(), store.RunOutput{RunID: f.run.ID, Output: []byte("kept\n"), Source: "stdout"}); err != nil {
		t.Fatal(err)
	}
	f.srv.FinishRunOutput(t.Context(), f.run.ID)
	if row, _ := f.mem.row(f.run.ID); string(row.Output) != "kept\n" || rr.recoverCount() != 0 {
		t.Fatalf("row %q after %d reads, want the final row untouched and no read", row.Output, rr.recoverCount())
	}

	g, _ := recoveryFixture(t, newRecoveringRunner())
	if err := g.mem.EraseRunOutputs(t.Context(), []uuid.UUID{g.run.ID}); err != nil {
		t.Fatal(err)
	}
	g.srv.FinishRunOutput(t.Context(), g.run.ID)
	if _, ok := g.mem.row(g.run.ID); ok || g.srv.cfg.Runner.(*recoveringRunner).recoverCount() != 0 {
		t.Fatal("an erased run got a row or a substrate read")
	}
}
