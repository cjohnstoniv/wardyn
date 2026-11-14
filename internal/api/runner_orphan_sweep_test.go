// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// passSweeper is a runner whose every sweep pass is pass(n, isOrphan), n counting from 1.
type passSweeper struct {
	calls int
	pass  func(n int, isOrphan func(uuid.UUID) bool) (int, error)
}

func (p *passSweeper) SweepOrphanedSandboxes(_ context.Context, _ time.Duration, isOrphan func(uuid.UUID) bool) (int, error) {
	p.calls++
	return p.pass(p.calls, isOrphan)
}

// readRunStore fails its first failReads run reads.
type readRunStore struct {
	sandboxOrphanStore
	failReads int
}

func (s *readRunStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if s.failReads > 0 {
		s.failReads--
		return types.AgentRun{}, errors.New("pg: connection reset")
	}
	return s.sandboxOrphanStore.GetRun(ctx, id)
}

var (
	quickWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	onSession  = func() bool { return true }
	// errHeld is how a runner reports a candidate it holds until its pod is proven stopped.
	errHeld = errors.New("runner: k8s: teardown orphaned run: held, NetworkPolicies kept")
)

// TestReadySweepRetriesAFailedPass (#3534): a READY pass whose sweep call fails, or whose run read
// fails, is tried again on the same session, and the retry sweeps the orphan.
func TestReadySweepRetriesAFailedPass(t *testing.T) {
	for _, tc := range []struct {
		name      string
		callErr   error
		failReads int
	}{
		{name: "sweep call fails", callErr: errors.New("runnerwire: internal")},
		{name: "run read fails", failReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			orphan := uuid.New()
			st := &readRunStore{sandboxOrphanStore: sandboxOrphanStore{runs: []types.AgentRun{{ID: orphan, State: types.RunFailed}}}, failReads: tc.failReads}
			srv := New(baseTestConfig(h, st))
			var torn []uuid.UUID
			sw := &passSweeper{pass: func(n int, isOrphan func(uuid.UUID) bool) (int, error) {
				if n == 1 && tc.callErr != nil {
					return 0, tc.callErr
				}
				if !isOrphan(orphan) {
					return 0, nil
				}
				torn = append(torn, orphan)
				return 1, nil
			}}
			srv.sweepRunnerOrphansOnReady(context.Background(), uuid.New(), sw, onSession, quickWaits)
			if sw.calls != 2 || len(torn) != 1 {
				t.Fatalf("calls %d torn %v: the retry did not sweep the orphan, or did not stop after", sw.calls, torn)
			}
		})
	}
}

// TestReadySweepHeldCandidateStaysHeld (#3534): a candidate the runner holds is retried on every
// wait, the runner refusing it each time; the sweep tears nothing down for it and stops at the bound.
func TestReadySweepHeldCandidateStaysHeld(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, &sandboxOrphanStore{}))
	run := uuid.New()
	var asked int
	sw := &passSweeper{pass: func(_ int, isOrphan func(uuid.UUID) bool) (int, error) {
		if isOrphan(run) {
			asked++
		}
		return 0, errHeld
	}}
	srv.sweepRunnerOrphansOnReady(context.Background(), uuid.New(), sw, onSession, quickWaits)
	if sw.calls != 1+len(quickWaits) || asked != sw.calls {
		t.Fatalf("calls %d asked %d, want %d passes over the held candidate", sw.calls, asked, 1+len(quickWaits))
	}
}

// TestReadySweepSweepsHeldCandidateOnceProven (#3534): the pass after the runner gets its proof of
// stop sweeps the held candidate, and the retries end there.
func TestReadySweepSweepsHeldCandidateOnceProven(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, &sandboxOrphanStore{}))
	run := uuid.New()
	const provenAt = 3
	var torn []uuid.UUID
	sw := &passSweeper{pass: func(n int, isOrphan func(uuid.UUID) bool) (int, error) {
		if !isOrphan(run) {
			return 0, nil
		}
		if n < provenAt {
			return 0, errHeld
		}
		torn = append(torn, run)
		return 1, nil
	}}
	srv.sweepRunnerOrphansOnReady(context.Background(), uuid.New(), sw, onSession, quickWaits)
	if sw.calls != provenAt || len(torn) != 1 {
		t.Fatalf("calls %d torn %v, want the held run swept on pass %d", sw.calls, torn, provenAt)
	}
}

// TestReadySweepRetryEndsWithTheSession (#3534): no retry runs once the session that went READY is
// replaced, or once ctx ends during a wait.
func TestReadySweepRetryEndsWithTheSession(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, &sandboxOrphanStore{}))
	held := func(int, func(uuid.UUID) bool) (int, error) { return 0, errHeld }

	sw := &passSweeper{pass: held}
	srv.sweepRunnerOrphansOnReady(context.Background(), uuid.New(), sw, func() bool { return false }, quickWaits)
	if sw.calls != 1 {
		t.Fatalf("session replaced: %d passes, want 1", sw.calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sw = &passSweeper{pass: func(n int, isOrphan func(uuid.UUID) bool) (int, error) {
		cancel()
		return held(n, isOrphan)
	}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic: %v\n%s", r, debug.Stack())
			}
		}()
		srv.sweepRunnerOrphansOnReady(ctx, uuid.New(), sw, onSession, []time.Duration{time.Hour})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ctx ended: the sweep still waits for its retry")
	}
	if sw.calls != 1 {
		t.Fatalf("ctx ended: %d passes, want 1", sw.calls)
	}
}

// verdictSweeper is a runner that lists one run's sandbox to the READY sweep and records each verdict it is given.
type verdictSweeper struct {
	run      uuid.UUID
	verdicts []bool
}

func (v *verdictSweeper) SweepOrphanedSandboxes(_ context.Context, _ time.Duration, isOrphan func(uuid.UUID) bool) (int, error) {
	o := isOrphan(v.run)
	v.verdicts = append(v.verdicts, o)
	if o {
		return 1, nil
	}
	return 0, nil
}

// readySweep is one READY pass over the forced fixture's run, listed by the runner that reconnected; the cut, if
// one is made, goes through the fixture's own runner, as the remote substrate's does. It answers the pass's verdict.
func (f *forced) readySweep(t *testing.T) bool {
	t.Helper()
	sw := &verdictSweeper{run: f.run.ID}
	f.s.sweepRunnerOrphansOnReady(context.Background(), f.cs.runner.ID, sw, onSession, nil)
	if len(sw.verdicts) != 1 {
		t.Fatalf("the READY pass gave %d verdicts on the run; want 1", len(sw.verdicts))
	}
	return sw.verdicts[0]
}

// cadenceSweep is one cadence label sweep over the forced fixture's run, answering its verdict.
func (f *forced) cadenceSweep(t *testing.T) bool {
	t.Helper()
	if err := f.s.sweepOrphanedSandboxes(context.Background()); err != nil {
		t.Fatal(err)
	}
	return f.orphanSeen()
}

// unreadable makes the forced fixture's computer folder record unreadable.
func (f *forced) unreadable() {
	f.s.cfg.Store = unreadableRecordStore{f.s.cfg.Store.(markedStore)}
}

// orphanCutRows counts the sandbox.orphan.sweep failure rows a failed cut wrote (teardown_error).
func orphanCutRows(audit *recRecorder) int {
	n := 0
	for _, ev := range audit.snapshot() {
		if ev.Action == "sandbox.orphan.sweep" && ev.Outcome == "failure" && strings.Contains(string(ev.Data), "teardown_error") {
			n++
		}
	}
	return n
}

// A runner that reconnects does not remove an ended run's sandbox kept for its copy back on a connected runner
// that fences: the READY pass fences the agent once and keeps it, as the cadence sweep does. Catches: the READY
// sweep calling every ended run an orphan, and a READY sweep that takes the run's lock but skips the keep verdict.
func TestReadySweep_KeepsAnOwedSandboxOnAFencedRunner(t *testing.T) {
	f := newForced(t)
	fr, _ := f.onFencedRunner(t)
	f.claimOnly(t)
	f.setRecord(owedRecord)
	f.release()
	if f.readySweep(t) {
		t.Fatal("the READY sweep called a sandbox kept for its copy back an orphan")
	}
	if fr.fences.Load() != 1 || f.sb.stopCount() != 0 {
		t.Errorf("%d fences, %d sandbox stops; want the agent fenced once and the sandbox kept", fr.fences.Load(), f.sb.stopCount())
	}
	if run := f.terminalRun(t); run.SandboxRef == "" {
		t.Error("the kept sandbox's ref was cleared")
	}
}

// An owed sandbox whose runner is not connected here is left by the READY pass: nothing is called on the runner
// and no row is written. Catches: the READY sweep calling every ended run an orphan, and a keep read as "the cut
// is confirmed" alone, which removes a sandbox left for a runner that is away.
func TestReadySweep_LeavesAnOwedSandboxWhoseRunnerIsAway(t *testing.T) {
	f := newForced(t)
	fr, online := f.onFencedRunner(t)
	f.claimOnly(t)
	f.setRecord(owedRecord)
	online.Store(false)
	f.release()
	before := len(f.audit.snapshot())
	if f.readySweep(t) {
		t.Fatal("the READY sweep called a sandbox left for its away runner an orphan")
	}
	if rows := len(f.audit.snapshot()) - before; fr.calls() != 0 || rows != 0 {
		t.Errorf("%d runner calls, %d new rows; want none", fr.calls(), rows)
	}
}

// The READY pass skips an ended run whose lock is held, as an end still copying its folder back holds it; once
// the lock is free the same run, with nothing to copy back, is an orphan, and every pass releases the lock it
// took. Catches: the READY sweep calling every ended run an orphan, a READY sweep that does not try the run's
// lock, and one that leaves the lock held.
func TestReadySweep_SkipsARunWhoseLockIsHeld(t *testing.T) {
	f := newForced(t)
	f.withoutOwedPass()
	f.claimOnly(t)
	if f.readySweep(t) {
		t.Fatal("the READY sweep called a run whose lock is held an orphan")
	}
	f.release()
	if !f.readySweep(t) {
		t.Fatal("with the lock free and nothing to copy back, the READY sweep left the run")
	}
	if _, unlock, free := f.s.tryLockRunOp(context.Background(), f.run.ID); !free {
		t.Error("the READY pass left the run's lock held")
	} else {
		unlock()
	}
}

// GetRun reads one run, as the READY sweep does.
func (s *orphanSweepStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		if r.ID == id {
			return r, nil
		}
	}
	return types.AgentRun{}, store.ErrNotFound
}

// Cuts that never answer share the READY pass's one bound, orphanSweepBound: of three kept runs, one cut is sent
// and ends at that bound, the others are not cut, none is an orphan and every ref stays. The next pass, whose cuts
// answer, decides each. Catches: a READY pass with no bound, one that reads a timed-out cut as an orphan, and a
// fresh bound per run.
func TestReadySweep_AHangingCutEndsAtTheBound(t *testing.T) {
	const bound, slack = 100 * time.Millisecond, time.Second
	srv, st, rn, audit := keptSweepRig(t, 3, bound)
	rn.hang.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second) // only a cut with no bound waits this out
	defer cancel()
	start := time.Now()
	srv.sweepRunnerOrphansOnReady(ctx, uuid.New(), rn, onSession, nil)
	if took := time.Since(start); took > bound+runTeardownBudget+slack {
		t.Errorf("the READY pass took %v; want it back within its bound %v and one failure row", took, bound)
	}
	if rn.cuts() != 1 {
		t.Errorf("%d cuts sent in one READY pass; want 1, the rest left once its bound was spent", rn.cuts())
	}
	if dl := rn.deadline(0); dl.IsZero() || dl.After(start.Add(bound+slack)) {
		t.Errorf("the cut ran to %v after the pass began; want the pass's bound of %v", dl.Sub(start), bound)
	}
	rows := 0
	for _, r := range st.runs {
		if rn.verdicts[r.ID] || r.SandboxRef == "" {
			t.Errorf("run %s: verdict %v, ref %q; want it not an orphan and its ref kept", r.ID, rn.verdicts[r.ID], r.SandboxRef)
		}
		rows += cutFailureRows(audit, r.ID)
	}
	if len(st.clearedRefs) != 0 || rows != 1 {
		t.Errorf("refs cleared %v, %d cut failure rows; want none cleared and the one timed-out cut's row", st.clearedRefs, rows)
	}
	rn.hang.Store(false)
	orphanSweepBound = time.Minute // the rig restores it
	srv.sweepRunnerOrphansOnReady(context.Background(), uuid.New(), rn, onSession, nil)
	for _, r := range st.runs {
		if !rn.verdicts[r.ID] {
			t.Errorf("run %s: the next READY pass left it; want its failed cut decided as an orphan", r.ID)
		}
	}
}

// What no keep holds is still removed on reconnect: an unreadable record whose runner cut failed is an orphan,
// with the cut's one failure row (an ended run with no record and a run with no row are the
// TestRunnerOrphanSweepOnReady pins). Catches: a READY sweep that never removes an ended run's sandbox.
func TestReadySweep_RemovesWhatNoKeepHolds(t *testing.T) {
	f := newForced(t)
	fr, _ := f.onFencedRunner(t)
	fr.fenceErr = errors.New("the runner did not answer the fence")
	f.claimOnly(t)
	f.unreadable()
	f.release()
	if !f.readySweep(t) {
		t.Fatal("the READY sweep left an unreadable record whose cut failed")
	}
	if fr.fences.Load() != 1 || orphanCutRows(f.audit) != 1 {
		t.Errorf("%d fences, %d cut failure rows; want the one failed cut and its row", fr.fences.Load(), orphanCutRows(f.audit))
	}
}

// The READY sweep gives an ended run's sandbox the cadence sweep's own verdict, case by case, each case on a fresh
// fixture per sweep. Catches: the two sweeps deciding an ended run by different rules.
func TestReadySweep_DecidesAnEndedRunAsTheCadenceSweepDoes(t *testing.T) {
	away := func(t *testing.T, f *forced) {
		_, online := f.onFencedRunner(t)
		online.Store(false)
	}
	failedCut := func(t *testing.T, f *forced) {
		fr, _ := f.onFencedRunner(t)
		fr.fenceErr = errors.New("the runner did not answer the fence")
	}
	vm := func(f *forced) {
		f.cs.dispatchTestStore.mu.Lock()
		f.cs.run.MachineKind = types.MachineLinuxVM
		f.cs.dispatchTestStore.mu.Unlock()
	}
	for _, tc := range []struct {
		name   string
		setup  func(*testing.T, *forced)
		held   bool // the run's lock stays held through the sweep
		orphan bool
	}{
		{"nothing to copy back", func(_ *testing.T, f *forced) { f.withoutOwedPass() }, false, true},
		{"owed, the server's Docker", func(_ *testing.T, f *forced) { f.setRecord(owedRecord) }, false, false},
		{"owed, a fenced runner", func(t *testing.T, f *forced) { f.onFencedRunner(t); f.setRecord(owedRecord) }, false, false},
		{"owed, a runner that is away", func(t *testing.T, f *forced) { away(t, f); f.setRecord(owedRecord) }, false, false},
		{"the run's lock held", func(_ *testing.T, f *forced) { f.withoutOwedPass() }, true, false},
		{"unreadable, the cut confirmed", func(t *testing.T, f *forced) { f.onFencedRunner(t); f.unreadable() }, false, false},
		{"unreadable, the cut failed", func(t *testing.T, f *forced) { failedCut(t, f); f.unreadable() }, false, true},
		{"owed, a virtual machine", func(_ *testing.T, f *forced) { vm(f); f.setRecord(owedRecord) }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got [2]bool
			for i, sweep := range []func(*forced, *testing.T) bool{(*forced).cadenceSweep, (*forced).readySweep} {
				f := newForced(t)
				tc.setup(t, f)
				f.claimOnly(t)
				if !tc.held {
					f.release()
				}
				got[i] = sweep(f, t)
			}
			if got[0] != tc.orphan || got[1] != tc.orphan {
				t.Errorf("orphan: cadence %v, READY %v; want both %v", got[0], got[1], tc.orphan)
			}
		})
	}
}
