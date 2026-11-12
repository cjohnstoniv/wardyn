// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// takeEndHolds takes n end holds on s for runs of their own, released at the test's end.
func takeEndHolds(t *testing.T, s *Server, n int) {
	t.Helper()
	for range n {
		_, unlock, err := s.holdRunEnd(context.Background(), uuid.New(), time.Minute)
		if err != nil {
			t.Fatalf("end hold refused with %d of %d taken: %v", s.endHolds.n, maxEndHolds, err)
		}
		t.Cleanup(unlock)
	}
}

// A finalize whose end hold has passed makes no stop and no teardown record: the runner is not called, the run's
// ref is kept and no run.complete row is written, and the caller hears the hold ran out. The tail owner's finalize,
// which holds no end hold, then stops the sandbox and clears the ref. The holder's other calls, which run on its own
// ctx and ignore the deadline (the audit of the end, the revoke, the output's drain), are not asserted here.
// Catches a stop or a teardown record made past the hold.
func TestEndHold_PastDeadlineNoStopNoRecord(t *testing.T) {
	pool, cfg, _, run, _, probe := runnerExitAPIFixture(t)
	srv := New(cfg)
	got, err := cfg.Store.GetRun(t.Context(), run.ID)
	if err != nil || got.SandboxRef == "" {
		t.Fatalf("fixture run %+v, %v: want one with a sandbox", got, err)
	}
	ctx := withHoldDeadline(t.Context(), time.Now().Add(-time.Second))
	if err := srv.finalizeRunEffects(ctx, run.ID, got.SandboxRef, "run.complete", nil, false, false); !errors.Is(err, errHoldPassed) {
		t.Errorf("finalize past the hold = %v, want it to say the hold ran out", err)
	}
	if n := probe.calls.Load(); n != 0 {
		t.Errorf("StopSandbox reached %d times past the hold, want 0", n)
	}
	if after, _ := cfg.Store.GetRun(t.Context(), run.ID); after.SandboxRef != got.SandboxRef {
		t.Errorf("sandbox ref %q past the hold, want it kept as %q", after.SandboxRef, got.SandboxRef)
	}
	var rows int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='run.complete'`, run.ID).Scan(&rows); err != nil || rows != 0 {
		t.Errorf("%d teardown rows (%v) past the hold, want 0", rows, err)
	}
	srv.tailFinalize(t.Context(), got, tailFolder)
	if n := probe.calls.Load(); n != 1 {
		t.Errorf("StopSandbox reached %d times by the tail owner, want 1", n)
	}
	if after, _ := cfg.Store.GetRun(t.Context(), run.ID); after.SandboxRef != "" {
		t.Errorf("sandbox ref %q after the tail owner's stop, want it cleared", after.SandboxRef)
	}
	srv.WaitBackground()
}

// teardownCtxWithin and settleAfterTeardown end at the end hold's deadline when it comes before today's timeout,
// with errHoldPassed as the cause, whatever the floor; a hold that has passed gives a ctx already done; and a hold
// after today's timeout, or none, leaves today's. Catches a floor that outlives the hold.
func TestEndHold_FloorCappedAtDeadline(t *testing.T) {
	const slack = time.Second
	for _, tc := range []struct {
		name  string
		floor time.Duration
		hold  time.Duration // from now; 0 is no hold
		want  time.Duration // the ctx's deadline, from now; 0 is a ctx already done
	}{
		{"no hold", runTeardownBudget, 0, reconcileFinalizeTimeout},
		{"hold before the floor", runTeardownBudget, 5 * time.Second, 5 * time.Second},
		{"hold at the floor", runTeardownBudget, runTeardownBudget, runTeardownBudget},
		{"hold after the floor", runTeardownBudget, 30 * time.Second, 30 * time.Second},
		{"hold after today's timeout", runTeardownBudget, 2 * reconcileFinalizeTimeout, reconcileFinalizeTimeout},
		{"hold before a macOS VM's floor", macOSVMTeardownBudget, 30 * time.Second, 30 * time.Second},
		{"no hold, a macOS VM's floor", macOSVMTeardownBudget, 0, macOSVMTeardownBudget},
		{"hold passed", runTeardownBudget, -time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			if tc.hold != 0 {
				ctx = withHoldDeadline(ctx, time.Now().Add(tc.hold))
			}
			before := time.Now()
			tctx, cancel := teardownCtxWithin(ctx, tc.floor)
			defer cancel()
			after := time.Now()
			if tc.want == 0 {
				if tctx.Err() == nil || !errors.Is(context.Cause(tctx), errHoldPassed) {
					t.Fatalf("teardown ctx err %v cause %v, want done with errHoldPassed", tctx.Err(), context.Cause(tctx))
				}
			} else {
				dl, ok := tctx.Deadline()
				if !ok || dl.Before(before.Add(tc.want-slack)) || dl.After(after.Add(tc.want+slack)) {
					t.Errorf("teardown ctx deadline %v from now, want about %v", time.Until(dl), tc.want)
				}
			}
			var sdl time.Time
			var serr, scause error
			settleAfterTeardown(ctx, func(sctx context.Context) {
				sdl, _ = sctx.Deadline()
				serr, scause = sctx.Err(), context.Cause(sctx)
			})
			want := runTeardownBudget
			if tc.hold != 0 {
				want = min(want, tc.hold)
			}
			switch {
			case want <= 0:
				if serr == nil || !errors.Is(scause, errHoldPassed) {
					t.Errorf("settle ctx err %v cause %v, want done with errHoldPassed", serr, scause)
				}
			case sdl.Before(before.Add(want-slack)) || sdl.After(time.Now().Add(want+slack)):
				t.Errorf("settle ctx deadline %v from now, want about %v", time.Until(sdl), want)
			}
		})
	}
	// A cap that fires is the hold's, by its cause.
	tctx, cancel := teardownCtxWithin(withHoldDeadline(context.Background(), time.Now().Add(20*time.Millisecond)), runTeardownBudget)
	defer cancel()
	<-tctx.Done()
	if !errors.Is(context.Cause(tctx), errHoldPassed) {
		t.Errorf("the capped ctx ended with cause %v, want errHoldPassed", context.Cause(tctx))
	}
}

// With maxEndHolds end holds taken, the next is refused as busy: an error wrapping db.ErrLockNoCapacity, which
// writeLockRefused answers 503 lock_unavailable. Catches a semaphore that does not bound, or a refusal that is
// answered as a server error.
func TestEndHold_SemaphoreFullIsBusy(t *testing.T) {
	if maxEndHolds != 4 {
		t.Fatalf("maxEndHolds = %d: this pin names 4", maxEndHolds)
	}
	s := &Server{}
	takeEndHolds(t, s, maxEndHolds)
	_, unlock, err := s.holdRunEnd(context.Background(), uuid.New(), time.Minute)
	if err == nil {
		unlock()
		t.Fatal("a fifth end hold was taken")
	}
	if !errors.Is(err, db.ErrLockNoCapacity) || !db.LockRefused(err) {
		t.Errorf("fifth hold = %v, want a refusal wrapping db.ErrLockNoCapacity", err)
	}
	w := httptest.NewRecorder()
	writeLockRefused(w, httptest.NewRequest(http.MethodPost, "/", nil), err)
	if w.Code != http.StatusServiceUnavailable || reasonOf(t, w) != reasonLockUnavailable {
		t.Errorf("answer = %d %q, want 503 %s", w.Code, w.Body.String(), reasonLockUnavailable)
	}
}

// An end hold's unlock releases the run's lock and its slot; a lock that cannot be taken gives its slot back; and
// a ctx already under an end hold re-enters it with no second slot and the earlier deadline. Catches a slot that
// leaks, or a nested hold that counts twice.
func TestEndHold_SlotReleasedWithLock(t *testing.T) {
	s := &Server{}
	id := uuid.New()
	hctx, unlock, err := s.holdRunEnd(context.Background(), id, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	outer, _ := holdDeadline(hctx)
	takeEndHolds(t, s, maxEndHolds-1)
	nctx, nunlock, err := s.holdRunEnd(hctx, id, time.Hour)
	if err != nil {
		t.Fatalf("a nested hold with every slot taken = %v, want it to re-enter", err)
	}
	if dl, _ := holdDeadline(nctx); !dl.Equal(outer) {
		t.Errorf("nested hold deadline %v, want the outer %v", dl, outer)
	}
	nunlock()
	if s.endHolds.n != maxEndHolds {
		t.Errorf("%d slots taken after the nested unlock, want %d", s.endHolds.n, maxEndHolds)
	}
	unlock()
	if _, u, ok := s.tryLockRunOp(context.Background(), id); !ok {
		t.Error("the run's lock is still held after the hold's unlock")
	} else {
		u()
	}
	_, u, err := s.holdRunEnd(context.Background(), uuid.New(), time.Minute)
	if err != nil {
		t.Fatalf("no slot after an unlock: %v", err)
	}
	u()
	// A lock that cannot be taken: held elsewhere, and the hold's ctx already cancelled.
	busy := uuid.New()
	_, bunlock, err := s.lockRunOp(context.Background(), busy)
	if err != nil {
		t.Fatal(err)
	}
	defer bunlock()
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.holdRunEnd(cctx, busy, time.Minute); err == nil {
		t.Fatal("a hold on a lock held elsewhere was taken")
	}
	if s.endHolds.n != maxEndHolds-1 {
		t.Errorf("%d slots taken after a refused lock, want %d", s.endHolds.n, maxEndHolds-1)
	}
}

// holdPassingAgent is the bring-back's sandbox whose agent stop, before the pass, returns only after dl: the end
// hold passes while the bring-back runs.
type holdPassingAgent struct {
	*agentSandbox
	dl time.Time
}

func (r *holdPassingAgent) ExecStream(ctx context.Context, ref string, spec runner.ExecSpec) (*runner.ExecSession, error) {
	if len(spec.Argv) == 3 && spec.Argv[0] == "sh" && spec.Argv[2] == agentStopScript {
		time.Sleep(time.Until(r.dl) + 50*time.Millisecond)
	}
	return r.agentSandbox.ExecStream(ctx, ref, spec)
}

// lateWriteStore is the bring-back's store with a database's ctx: a folder record or ref write on a done ctx
// fails, as a Postgres write does, and one that lands at or after dl is counted.
type lateWriteStore struct {
	refStore
	dl   time.Time
	late atomic.Int32
}

func (s *lateWriteStore) lands(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !time.Now().Before(s.dl) {
		s.late.Add(1)
	}
	return nil
}

func (s *lateWriteStore) SetRunLocalFolderStatusIf(ctx context.Context, id uuid.UUID, guard store.FolderStatusGuard, st types.RunLocalFolderStatus) (bool, error) {
	if err := s.lands(ctx); err != nil {
		return false, err
	}
	return s.refStore.SetRunLocalFolderStatusIf(ctx, id, guard, st)
}

func (s *lateWriteStore) BumpRunLocalFolderGen(ctx context.Context, id uuid.UUID, guard store.FolderStatusGuard, at time.Time) (types.RunLocalFolderStatus, bool, error) {
	if err := s.lands(ctx); err != nil {
		return types.RunLocalFolderStatus{}, false, err
	}
	return s.refStore.BumpRunLocalFolderGen(ctx, id, guard, at)
}

func (s *lateWriteStore) SetSandboxRef(ctx context.Context, id uuid.UUID, ref string) error {
	if err := s.lands(ctx); err != nil {
		return err
	}
	return s.refStore.SetSandboxRef(ctx, id, ref)
}

// A person's bring-back is an end hold: with every hold taken it is refused 503 lock_unavailable and the run stays
// RUNNING. A bring-back whose hold passes while it runs (here its agent stop returns after the hold's deadline,
// which the server's base ctx carries in, so the bring-back's own hold re-enters it) stops no sandbox and writes
// no folder record or ref after the deadline. Catches the bring-back's own lockRunOp left in place. The deadline is
// injected as an outer hold on the base ctx, so the second case proves the caps downstream, not that the bring-back's
// own B supplies it; the first case covers that the bring-back takes its own hold.
func TestEndHold_BringBackIsAHold(t *testing.T) {
	t.Run("every hold taken", func(t *testing.T) {
		s, cs, audit, run, sb, _ := bringBackFixture(t)
		var stops atomic.Int32
		sb.stopped = func() { stops.Add(1) }
		takeEndHolds(t, s, maxEndHolds)
		res := bringBack(t, s, bringBackOwnerSession(t), run)
		if res.code != http.StatusServiceUnavailable || res.reason() != reasonLockUnavailable {
			t.Fatalf("bring-back with every hold taken = %d %s, want 503 %s", res.code, res.body, reasonLockUnavailable)
		}
		if cs.State() != types.RunRunning || findAudit(audit.snapshot(), run.ID, "run.folder.finalize", "success") != nil {
			t.Errorf("state %s after a refused bring-back, want RUNNING with no end written", cs.State())
		}
		if n := stops.Load(); n != 0 {
			t.Errorf("StopSandbox reached %d times by a refused bring-back, want 0", n)
		}
	})
	t.Run("the hold passes during the bring-back", func(t *testing.T) {
		s, cs, _, run, sb, _ := bringBackFixture(t)
		dl := time.Now().Add(300 * time.Millisecond)
		s.cfg.BaseCtx = withHoldDeadline(context.Background(), dl)
		s.cfg.Runner = &holdPassingAgent{agentSandbox: sb, dl: dl}
		ws := &lateWriteStore{refStore: refStore{cs}, dl: dl}
		s.cfg.Store = ws
		var stops atomic.Int32
		sb.stopped = func() { stops.Add(1) }
		_ = bringBack(t, s, bringBackOwnerSession(t), run)
		if n := stops.Load(); n != 0 {
			t.Errorf("StopSandbox reached %d times after the hold passed, want 0", n)
		}
		if n := ws.late.Load(); n != 0 {
			t.Errorf("%d folder record or ref writes landed after the hold passed, want 0", n)
		}
		if got, _ := ws.GetRun(context.Background(), run.ID); got.SandboxRef == "" {
			t.Error("the sandbox ref was cleared after the hold passed")
		}
	})
}

// A runner exit is an end hold, always: with every hold taken its receipt is not applied and stays pending, and a
// later sweep applies it. One whose hold has passed (an outer hold already past, which the exit's own re-enters)
// is accepted but stops nothing and leaves its tail incomplete for the next sweep. Catches the runner exit's own
// lockRunOp left in place. The deadline is injected as an outer hold on the ctx, so the second case proves the caps
// downstream, not that the exit's own B supplies it; the first case covers that the exit takes its own hold.
func TestEndHold_RunnerExitIsAHold(t *testing.T) {
	stage := func(t *testing.T) (*Server, Config, store.PG, types.Runner, types.AgentRun, runnerwire.AgentExit, *runnerExitTailProbe, func(uuid.UUID) (bool, bool)) {
		t.Helper()
		pool, cfg, rn, run, exit, probe := runnerExitAPIFixture(t)
		st := cfg.Store.(store.PG)
		srv := New(cfg)
		t.Cleanup(srv.WaitBackground)
		event := runnerwire.Event{Kind: runnerwire.EventAgentExit, RunID: &run.ID, Data: mustJSON(exit)}
		if err := srv.ExitFromRunner(t.Context(), rn.ID, event); err != nil {
			t.Fatal(err)
		}
		if err := st.BindRunnerExecution(t.Context(), rn.ID, run.ID, exit.Generation, exit.ExecID); err != nil {
			t.Fatal(err)
		}
		phase := func(id uuid.UUID) (accepted, done bool) {
			if err := pool.QueryRow(t.Context(), `SELECT accepted_at IS NOT NULL, tail_complete FROM runner_execution_exits WHERE run_id=$1`, id).Scan(&accepted, &done); err != nil {
				t.Fatal(err)
			}
			return accepted, done
		}
		return srv, cfg, st, rn, run, exit, probe, phase
	}
	t.Run("every hold taken", func(t *testing.T) {
		srv, cfg, _, _, run, _, probe, phase := stage(t)
		takeEndHolds(t, srv, maxEndHolds)
		if err := srv.SweepRunnerExits(t.Context()); err == nil {
			t.Fatal("a runner exit applied with every end hold taken")
		}
		if accepted, done := phase(run.ID); accepted || done || probe.calls.Load() != 0 {
			t.Fatalf("receipt accepted %v done %v, %d stops: want it pending and untouched", accepted, done, probe.calls.Load())
		}
		later := New(cfg)
		t.Cleanup(later.WaitBackground)
		if err := later.SweepRunnerExits(t.Context()); err != nil {
			t.Fatalf("the retry = %v", err)
		}
		if accepted, done := phase(run.ID); !accepted || !done || probe.calls.Load() != 1 {
			t.Errorf("after the retry: accepted %v done %v, %d stops; want it applied once", accepted, done, probe.calls.Load())
		}
	})
	t.Run("the hold passes", func(t *testing.T) {
		srv, cfg, st, rn, run, exit, probe, phase := stage(t)
		before, err := cfg.Store.GetRun(t.Context(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		ctx := withHoldDeadline(t.Context(), time.Now().Add(-time.Second))
		if err := srv.applyRunnerExit(ctx, st, store.RunnerExitWork{RunnerID: rn.ID, RunID: run.ID, Exit: exit}); err == nil {
			t.Error("a runner exit past its hold reported its tail complete")
		}
		if n := probe.calls.Load(); n != 0 {
			t.Errorf("StopSandbox reached %d times past the hold, want 0", n)
		}
		if _, done := phase(run.ID); done {
			t.Error("the tail is recorded complete past the hold, want it left for the next sweep")
		}
		if after, _ := cfg.Store.GetRun(t.Context(), run.ID); after.SandboxRef != before.SandboxRef {
			t.Errorf("sandbox ref %q past the hold, want it kept as %q", after.SandboxRef, before.SandboxRef)
		}
	})
}

// A pass back under an end hold ending 40 s out leaves its record the teardown's 15 s inside the hold: the pass is
// sent a deadline at most 25 s out, and its record is written. Catches the cap applied only inside
// computerFolderPassWithin, which would send the pass a deadline 35 s out.
func TestEndHold_PassBudgetLeavesTheRecordItsTeardown(t *testing.T) {
	if runTeardownBudget != 15*time.Second {
		t.Fatalf("runTeardownBudget = %v: this pin names 15 s", runTeardownBudget)
	}
	f := newPassFixture(t, 2, 2, nil)
	r := &scriptedFolderRunner{}
	id := f.cs.runner.ID
	f.s.runnerStreams.subsMu.Lock()
	f.s.runnerStreams.subs[id] = r.endpoint(t, id)
	f.s.runnerStreams.subsMu.Unlock()
	before := time.Now()
	ctx := withHoldDeadline(context.Background(), before.Add(40*time.Second))
	_ = f.s.bringComputerFolderBack(ctx, f.run.ID, "sbx")
	got := r.args().DeadlineUnixMs
	if got == 0 || got > time.Now().Add(25*time.Second).UnixMilli() {
		t.Errorf("the pass was sent deadline %d, %v from the start; want at most 25 s out", got, time.UnixMilli(got).Sub(before))
	}
	if st := f.cs.folderStatus(); st.Back == nil {
		t.Errorf("record %+v after the pass, want its pass back written inside the hold", st)
	}
}

// A revive of a run past its maximum age is refused with its own reason, through the revive handler: before the
// end's answer, which asks for an extension the cap refuses, and after the files-gone answer. Also when the cap
// passes while the revive pulls its proxy image, from the re-read before the claim. Catches the cap tested after
// reviveEligible's past-end arm, or after reviveExpiryDue's early return for a run still inside its keep. The
// "no end" pull case is a run with no end (keptUntil answers false): only reviveExpiryDue's own cap arm refuses it.
func TestRevive_MaxAgeHasItsOwnReason(t *testing.T) {
	for name, tc := range map[string]struct {
		set    func(f *reviveFixture, run *types.AgentRun) // the stored run, before the revive
		during func(f *reviveFixture)                      // while the proxy image is pulled; nil for none
		want   string
	}{
		"no end, past its cap": {set: func(f *reviveFixture, run *types.AgentRun) {
			lost, limit := f.now.Add(-time.Minute), f.now.Add(-time.Second)
			run.EndsAt, run.LostAt, run.LostReason, run.LifetimeCapAt = nil, &lost, types.LostOutage, &limit
		}, want: reasonReviveMaxAge},
		"end passed inside its grace, cap passed": {set: func(f *reviveFixture, run *types.AgentRun) {
			limit := f.now.Add(-time.Second)
			run.LifetimeCapAt = &limit
		}, want: reasonReviveMaxAge},
		"ended inside its files grace, end ahead, cap passed": {set: func(f *reviveFixture, run *types.AgentRun) {
			end, limit := f.now.Add(time.Hour), f.now.Add(-time.Second)
			run.EndsAt, run.LifetimeCapAt = &end, &limit
		}, want: reasonReviveMaxAge},
		"end passed, cap ahead": {set: func(f *reviveFixture, run *types.AgentRun) {
			limit := f.now.Add(time.Hour)
			run.LifetimeCapAt = &limit
		}, want: reasonRevivePastEnd},
		"ended, files gone, cap passed": {set: func(f *reviveFixture, run *types.AgentRun) {
			limit := f.now.Add(-time.Second)
			run.LifetimeCapAt = &limit
			f.now = f.now.Add(f.srv.cfg.EndedRunGrace + time.Second)
		}, want: reasonReviveEndedFilesGone},
		"no end, cap passes during the pull": {set: func(f *reviveFixture, run *types.AgentRun) {
			lost, limit := f.now.Add(-time.Minute), f.now.Add(time.Minute)
			run.EndsAt, run.LostAt, run.LostReason, run.LifetimeCapAt = nil, &lost, types.LostOutage, &limit
		}, during: func(f *reviveFixture) { f.now = f.now.Add(2 * time.Minute) }, want: reasonReviveMaxAge},
		"cap passes during the pull": {set: func(f *reviveFixture, run *types.AgentRun) {
			end, limit := f.now.Add(time.Hour), f.now.Add(time.Minute)
			run.EndsAt, run.LifetimeCapAt = &end, &limit
		}, during: func(f *reviveFixture) { f.now = f.now.Add(2 * time.Minute) }, want: reasonReviveMaxAge},
	} {
		t.Run(name, func(t *testing.T) {
			f, sr := newEndedFixture(t)
			if f.srv.cfg.EndedRunGrace < time.Hour {
				t.Fatalf("EndedRunGrace = %v: these cases need a grace of an hour or more", f.srv.cfg.EndedRunGrace)
			}
			f.st.mu.Lock()
			tc.set(f, &f.st.run)
			f.run = f.st.run
			f.st.mu.Unlock()
			if tc.during != nil {
				f.rr.onEnsure = func() {
					f.rr.onEnsure = nil
					tc.during(f)
				}
			}
			code, body := f.reviveAs(t, true)
			if code != http.StatusConflict || errorReasonOf(body) != tc.want {
				t.Fatalf("revive = %d %s, want 409 %s", code, body, tc.want)
			}
			if len(f.rr.replaced) != 0 || len(sr.starts()) != 0 {
				t.Errorf("proxies replaced %d, agent starts %v; want none", len(f.rr.replaced), sr.starts())
			}
		})
	}
}

// holdSeen records, for each StopSandbox, the end-hold deadline its ctx carries (zero for none).
type holdSeen struct {
	mu  sync.Mutex
	dls []time.Time
}

func (h *holdSeen) see(ctx context.Context) {
	dl, _ := holdDeadline(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dls = append(h.dls, dl)
}

func (h *holdSeen) calls() []time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.dls)
}

// holdSeeingLeaseRunner is the lease fixture's runner, recording each StopSandbox's hold.
type holdSeeingLeaseRunner struct {
	*leaseRunner
	seen *holdSeen
}

func (r *holdSeeingLeaseRunner) StopSandbox(ctx context.Context, ref string) error {
	r.seen.see(ctx)
	return r.leaseRunner.StopSandbox(ctx, ref)
}

// holdSeeingSandbox is the pass fixture's sandbox runner, recording each StopSandbox's hold before the call.
type holdSeeingSandbox struct {
	*folderSandboxRunner
	seen *holdSeen
}

func (r *holdSeeingSandbox) StopSandbox(ctx context.Context, ref string) error {
	r.seen.see(ctx)
	return r.folderSandboxRunner.StopSandbox(ctx, ref)
}

// A kept run's end (stopKeptRun) is an end hold for a sweep's budget: the stop's ctx carries a deadline 60 s from
// the call, and the slot is given back; with every hold taken it is a busy refusal and ends nothing; and past its
// hold (an outer hold already passed, which it re-enters) the terminal-effects guard reads the deadline and nothing
// is stopped. Catches stopKeptRun's own tryLockRunOpHeld left in place. The passed deadline is injected as an outer
// hold on the ctx, so that case proves the caps downstream, not that stopKeptRun's own B supplies it; "the hold is
// set" and "every hold taken" cover that it takes its own hold. A full semaphore answers held (a busy refusal), so
// a run lost more than keptEndHoldOffLimit past its due end is cut by forceKeptStop, as a lock held elsewhere is.
func TestEndHold_KeptRunEndIsAHold(t *testing.T) {
	setup := func(t *testing.T) (*leaseFixture, *holdSeen) {
		t.Helper()
		f := newLeaseFixture(t, -time.Minute)
		seen := &holdSeen{}
		f.srv.cfg.Runner = &holdSeeingLeaseRunner{leaseRunner: f.rn, seen: seen}
		return f, seen
	}
	t.Run("the hold is set", func(t *testing.T) {
		f, seen := setup(t)
		before := time.Now()
		f.srv.stopKeptRun(context.Background(), f.st, f.run, types.RunStopped, "run.ended", nil)
		after := time.Now()
		dls := seen.calls()
		if f.st.State() != types.RunStopped || len(dls) != 1 {
			t.Fatalf("state %s, %d stops; want STOPPED and one stop", f.st.State(), len(dls))
		}
		if dls[0].Before(before.Add(endHoldSweep)) || dls[0].After(after.Add(endHoldSweep)) {
			t.Errorf("the stop's hold deadline is %v from the call, want %v", dls[0].Sub(before), endHoldSweep)
		}
		if f.srv.endHolds.n != 0 {
			t.Errorf("%d end holds taken after the end, want 0", f.srv.endHolds.n)
		}
	})
	t.Run("every hold taken", func(t *testing.T) {
		f, seen := setup(t)
		takeEndHolds(t, f.srv, maxEndHolds)
		f.srv.stopKeptRun(context.Background(), f.st, f.run, types.RunStopped, "run.ended", nil)
		if f.st.State() != types.RunRunning || len(seen.calls()) != 0 {
			t.Errorf("state %s, %d stops with every end hold taken; want RUNNING and none", f.st.State(), len(seen.calls()))
		}
	})
	// A lost run (as heldEndFixture loses one, run_forced_end_test.go:28-31) with every hold taken: the full
	// semaphore is a busy answer, so stopKeptRun hands the run to forceKeptStop, which waits out
	// keptEndHoldOffLimit from the loss (data kept: false counts it from the loss, run_kept_stop_forced.go:55-58).
	lost := func(t *testing.T) (*leaseFixture, *holdSeen, types.AgentRun) {
		t.Helper()
		f, seen := setup(t)
		if applied, err := f.st.MarkRunEnded(context.Background(), f.run.ID, f.now); err != nil || !applied {
			t.Fatalf("MarkRunEnded = %v, %v; want the claim to land", applied, err)
		}
		run, err := f.st.GetRun(context.Background(), f.run.ID)
		if err != nil || run.LostAt == nil {
			t.Fatalf("lost run %+v, %v", run, err)
		}
		takeEndHolds(t, f.srv, maxEndHolds)
		return f, seen, run
	}
	// Control: green with or without held=true; it pins that nothing happens inside keptEndHoldOffLimit.
	t.Run("every hold taken, inside the hold-off", func(t *testing.T) {
		f, seen, run := lost(t)
		f.now = run.LostAt.Add(keptEndHoldOffLimit - time.Second)
		f.srv.stopKeptRun(context.Background(), f.st, run, types.RunStopped, "run.ended", map[string]any{"kept": false})
		if f.st.State() != types.RunRunning || len(seen.calls()) != 0 {
			t.Errorf("state %s, %d stops inside the hold-off; want RUNNING and none", f.st.State(), len(seen.calls()))
		}
		if n := len(f.audit.eventsFor(run.ID, "run.ended")); n != 0 {
			t.Errorf("%d run.ended rows inside the hold-off, want 0", n)
		}
	})
	t.Run("every hold taken, past the hold-off", func(t *testing.T) {
		f, seen, run := lost(t)
		f.now = run.LostAt.Add(keptEndHoldOffLimit + time.Minute)
		f.srv.stopKeptRun(context.Background(), f.st, run, types.RunStopped, "run.ended", map[string]any{"kept": false})
		if f.st.State() != types.RunStopped || len(seen.calls()) != 1 {
			t.Fatalf("state %s, %d stops past the hold-off; want STOPPED and one stop (the forced end)", f.st.State(), len(seen.calls()))
		}
		forced := 0
		for _, ev := range f.audit.eventsFor(run.ID, "run.ended") {
			if leaseAuditData(t, ev)["forced"] == true {
				forced++
			}
		}
		if forced != 1 {
			t.Errorf("%d forced run.ended rows, want 1", forced)
		}
	})
	t.Run("the hold passed", func(t *testing.T) {
		f, seen := setup(t)
		ctx := withHoldDeadline(context.Background(), time.Now().Add(-time.Second))
		f.srv.stopKeptRun(ctx, f.st, f.run, types.RunStopped, "run.ended", nil)
		if n := len(seen.calls()); n != 0 {
			t.Errorf("%d stops past the hold, want 0", n)
		}
	})
}

// The tail owner's run (tailRun) is an end hold for the dedicated budget: the stop after its pass back carries a
// deadline 3 minutes from the call; with every hold taken it runs nothing; and past its hold (an outer hold
// already passed, which it re-enters) no pass is sent and nothing is stopped. No pass is sent because the first
// read on a teardown ctx already done stops it (folderBackEligible, run_computer_folder.go:230-233), not the pass
// budget, which is not reached; the terminal-effects guard stops the stop. The pass budget's cap is pinned by
// TestEndHold_PassBudgetLeavesTheRecordItsTeardown. Catches tailRun's own tryLockRunOp left in place. The deadline
// is injected as an outer hold on the ctx, so the passed case proves the caps downstream, not that tailRun's own B
// supplies it; "the hold is set" and "every hold taken" cover that it takes its own hold.
func TestEndHold_TailOwnerRunIsAHold(t *testing.T) {
	setup := func(t *testing.T) (*passFixture, types.AgentRun, *holdSeen) {
		t.Helper()
		f := newPassFixture(t, 1, 0, &types.FolderResume{Unsynced: 1})
		seen := &holdSeen{}
		f.s.cfg.Runner = &holdSeeingSandbox{folderSandboxRunner: f.sb, seen: seen}
		ds := f.cs.localFolderStore.dispatchTestStore
		ds.mu.Lock()
		ds.state, ds.run.SandboxRef = types.RunCompleted, "sbx"
		ds.mu.Unlock()
		run, err := ds.GetRun(context.Background(), f.run.ID)
		if err != nil {
			t.Fatal(err)
		}
		return f, run, seen
	}
	t.Run("the hold is set", func(t *testing.T) {
		f, run, seen := setup(t)
		before := time.Now()
		f.s.tailRun(context.Background(), run.ID, tailFolder)
		after := time.Now()
		dls := seen.calls()
		if len(dls) != 1 {
			t.Fatalf("%d stops after the tail owner's pass back, want 1", len(dls))
		}
		if dls[0].Before(before.Add(endHoldDedicated)) || dls[0].After(after.Add(endHoldDedicated)) {
			t.Errorf("the stop's hold deadline is %v from the call, want %v", dls[0].Sub(before), endHoldDedicated)
		}
		if f.s.endHolds.n != 0 {
			t.Errorf("%d end holds taken after the tail, want 0", f.s.endHolds.n)
		}
	})
	t.Run("every hold taken", func(t *testing.T) {
		f, run, seen := setup(t)
		takeEndHolds(t, f.s, maxEndHolds)
		pass := passSeen(f.sb)
		if f.s.tailRun(context.Background(), run.ID, tailFolder) || pass.Load() || len(seen.calls()) != 0 {
			t.Errorf("with every end hold taken: pass %v, %d stops; want the run left for the next tick", pass.Load(), len(seen.calls()))
		}
	})
	t.Run("the hold passed", func(t *testing.T) {
		f, run, seen := setup(t)
		pass := passSeen(f.sb)
		ctx := withHoldDeadline(context.Background(), time.Now().Add(-time.Second))
		f.s.tailRun(ctx, run.ID, tailFolder)
		if pass.Load() || len(seen.calls()) != 0 {
			t.Errorf("past the hold: pass %v, %d stops; want no pass sent and nothing stopped", pass.Load(), len(seen.calls()))
		}
	})
}
