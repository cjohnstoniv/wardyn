// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// goneProbeRunner is the gone rig's runner with a settable sandbox Status: every agent probe reads
// stopped with no exit code, and Status answers status and statusErr, counting the calls.
type goneProbeRunner struct {
	*offlineContainmentProbe
	mu        sync.Mutex
	status    runner.Status
	statusErr error
	statuses  int
}

func (r *goneProbeRunner) AgentStatus(context.Context, string, string) (runner.Status, error) {
	return runner.Status{State: types.RunStopped}, nil
}

func (r *goneProbeRunner) Status(context.Context, string) (runner.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statuses++
	return r.status, r.statusErr
}

func (r *goneProbeRunner) set(status runner.Status, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status, r.statusErr = status, err
}

func (r *goneProbeRunner) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.statuses
}

// goneProbe is the gone rig with a live session: a present STATE page opened it, and the server's
// status checks of the run's sandbox go to gp.
type goneProbe struct {
	*goneAPIRig
	gp  *goneProbeRunner
	srv *Server
	ref string
}

func goneProbeFixture(t *testing.T, base context.Context) *goneProbe {
	t.Helper()
	g := newGoneAPIRig(t)
	gp := &goneProbeRunner{offlineContainmentProbe: g.probe, status: runner.Status{State: types.RunStopped}}
	g.cfg.Runner = gp
	g.cfg.BaseCtx = base
	srv := New(g.cfg)
	if err := finalStatePage(t, srv, g.rn.ID, g.present); err != nil {
		t.Fatalf("present page refused: %v", err)
	}
	return &goneProbe{goneAPIRig: g, gp: gp, srv: srv, ref: g.gone.Ref}
}

func (g *goneProbe) getRun() types.AgentRun {
	g.t.Helper()
	run, err := g.st.GetRun(g.t.Context(), g.run.ID)
	if err != nil {
		g.t.Fatal(err)
	}
	return run
}

func (g *goneProbe) check() runnerProbe {
	g.t.Helper()
	return g.srv.probeRunnerSandbox(g.t.Context(), g.getRun(), g.ref, runner.Status{State: types.RunStopped})
}

func (g *goneProbe) stampAt() *time.Time {
	g.t.Helper()
	var at *time.Time
	if err := g.pool.QueryRow(g.t.Context(), `SELECT sandbox_absent_at FROM agent_runs WHERE id=$1`, g.run.ID).Scan(&at); err != nil {
		g.t.Fatal(err)
	}
	return at
}

func (g *goneProbe) ageStamp() {
	g.t.Helper()
	g.exec(`UPDATE agent_runs SET sandbox_absent_at=sandbox_absent_at-interval '31 seconds' WHERE id=$1`)
}

// checkStillRunning fails unless the run is RUNNING with no run.lost row.
func (g *goneProbe) checkStillRunning() {
	g.t.Helper()
	if run := g.getRun(); run.State != types.RunRunning || run.LostReason != "" {
		g.t.Fatalf("run: state=%s lost_reason=%q; want RUNNING, not lost", run.State, run.LostReason)
	}
	if n := len(g.lostEvents()); n != 0 {
		g.t.Fatalf("run.lost rows = %d, want 0", n)
	}
}

// sameAt reports whether a and b are the same set time.
func sameAt(a, b *time.Time) bool { return a != nil && b != nil && a.Equal(*b) }

func cancelledContext(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

func TestRunnerProbePG_TwoAbsentChecksThirtySecondsApartEndTheRun(t *testing.T) {
	g := goneProbeFixture(t, t.Context())
	if got := g.check(); got != runnerProbeAbsent || g.stampAt() == nil || g.gp.calls() != 1 {
		t.Fatalf("first absent check: %v stamp=%v statuses=%d", got, g.stampAt(), g.gp.calls())
	}
	v := g.stampAt()
	if got := g.check(); got != runnerProbeAbsent || !sameAt(g.stampAt(), v) {
		t.Fatalf("second absent check at once: %v stamp=%v want %v", got, g.stampAt(), v)
	}
	g.ageStamp()
	if got := g.check(); got != runnerProbeEnded {
		t.Fatalf("checks 31 s apart: %v; want ended", got)
	}
	g.checkGoneEnd(g.getRun())
	if got := g.check(); got != runnerProbeNone {
		t.Fatalf("a check of an ended run: %v; want none", got)
	}
	if n := len(g.lostEvents()); n != 1 {
		t.Fatalf("run.lost rows = %d, want 1", n)
	}
}

func TestRunnerProbePG_PresentCheckClearsTheStamp(t *testing.T) {
	g := goneProbeFixture(t, t.Context())
	if got := g.check(); got != runnerProbeAbsent {
		t.Fatalf("absent check: %v", got)
	}
	g.gp.set(runner.Status{State: types.RunRunning}, nil)
	if got := g.check(); got != runnerProbeNone || g.stamped() {
		t.Fatalf("present check: %v stamped=%v", got, g.stamped())
	}
	g.gp.set(runner.Status{State: types.RunStopped}, nil)
	if got := g.check(); got != runnerProbeAbsent {
		t.Fatalf("absent check again: %v", got)
	}
	zero := 0
	g.gp.set(runner.Status{State: types.RunStopped, ExitCode: &zero}, nil)
	if got := g.check(); got != runnerProbeNone || g.stamped() {
		t.Fatalf("a stop with an exit code: %v stamped=%v", got, g.stamped())
	}
}

func TestRunnerProbePG_CheckErrorNeitherSetsNorClears(t *testing.T) {
	daemon := errors.New("docker: inspect: context deadline exceeded")
	t.Run("error", func(t *testing.T) {
		g := goneProbeFixture(t, t.Context())
		g.gp.set(runner.Status{State: types.RunStopped}, daemon)
		if got := g.check(); got != runnerProbeNone || g.stamped() {
			t.Fatalf("a failed check: %v stamped=%v", got, g.stamped())
		}
		g.gp.set(runner.Status{State: types.RunStopped}, nil)
		if got := g.check(); got != runnerProbeAbsent {
			t.Fatalf("absent check: %v", got)
		}
		v := g.stampAt()
		g.gp.set(runner.Status{State: types.RunStopped}, daemon)
		if got := g.check(); got != runnerProbeNone || !sameAt(g.stampAt(), v) {
			t.Fatalf("a failed check after a stamp: %v stamp=%v want %v", got, g.stampAt(), v)
		}
		g.ageStamp()
		if got := g.check(); got != runnerProbeNone {
			t.Fatalf("a failed check of a due stamp: %v; want none", got)
		}
		g.checkStillRunning()
	})
	t.Run("unobservable", func(t *testing.T) {
		g := goneProbeFixture(t, t.Context())
		g.exec(`UPDATE agent_runs SET machine_kind='macos_vm' WHERE id=$1`)
		if got := g.check(); got != runnerProbeAbsent {
			t.Fatalf("absent check: %v", got)
		}
		wire := &runnerwire.Error{Code: runnerwire.CodeInternal, Message: "runner: this sandbox's machine kind cannot be observed on this runner now (macOS VMs are off)"}
		if errors.Is(wire, runner.ErrSandboxGone) {
			t.Fatal("the unobservable answer reads as gone")
		}
		g.gp.set(runner.Status{}, wire)
		g.ageStamp()
		if got := g.check(); got != runnerProbeNone {
			t.Fatalf("an unobservable check of a due stamp: %v; want none", got)
		}
		g.checkStillRunning()
	})
}

func TestRunnerProbePG_ProbeAndStateConfirmEachOther(t *testing.T) {
	t.Run("check then STATE", func(t *testing.T) {
		g := goneProbeFixture(t, cancelledContext(t))
		if got := g.check(); got != runnerProbeAbsent {
			t.Fatalf("absent check: %v", got)
		}
		g.ageStamp()
		if err := finalStatePage(t, g.srv, g.rn.ID, g.gone); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
			t.Fatalf("absent page admitted: %v", err)
		}
		if err := g.srv.OfflineFromRunner(t.Context(), g.rn.ID); err != nil {
			t.Fatal(err)
		}
		g.checkGoneEnd(g.getRun())
	})
	t.Run("STATE then check", func(t *testing.T) {
		g := goneProbeFixture(t, cancelledContext(t))
		if err := finalStatePage(t, g.srv, g.rn.ID, g.gone); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
			t.Fatalf("absent page admitted: %v", err)
		}
		g.ageStamp()
		if got := g.check(); got != runnerProbeEnded {
			t.Fatalf("a check 31 s after a STATE report: %v; want ended", got)
		}
		g.checkGoneEnd(g.getRun())
	})
	t.Run("same moment never confirms", func(t *testing.T) {
		t.Run("check then STATE", func(t *testing.T) {
			g := goneProbeFixture(t, cancelledContext(t))
			if got := g.check(); got != runnerProbeAbsent {
				t.Fatalf("absent check: %v", got)
			}
			if err := finalStatePage(t, g.srv, g.rn.ID, g.gone); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
				t.Fatalf("absent page admitted: %v", err)
			}
			if err := g.srv.OfflineFromRunner(t.Context(), g.rn.ID); err != nil {
				t.Fatal(err)
			}
			g.checkStillRunning()
		})
		t.Run("STATE then check", func(t *testing.T) {
			g := goneProbeFixture(t, cancelledContext(t))
			if err := finalStatePage(t, g.srv, g.rn.ID, g.gone); !errors.Is(err, store.ErrRunnerReconciliationRequired) {
				t.Fatalf("absent page admitted: %v", err)
			}
			if got := g.check(); got != runnerProbeAbsent {
				t.Fatalf("a check at once after a STATE report: %v; want absent", got)
			}
			g.checkStillRunning()
		})
	})
}

func TestRunnerProbePG_WatcherKeepsWatchingUntilConfirmed(t *testing.T) {
	prev := reconcileWatchIntervalNS.Swap(int64(5 * time.Millisecond))
	t.Cleanup(func() { reconcileWatchIntervalNS.Store(prev) })
	g := goneProbeFixture(t, t.Context())
	run := g.getRun()

	wctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	g.srv.reconcileWatch(wctx, run.ID, g.ref, run.AgentExecID)
	err := wctx.Err()
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the watcher returned before its context ended (%v, %d checks)", err, g.gp.calls())
	}
	if n := g.gp.calls(); n < 2 {
		t.Fatalf("checks = %d, want at least 2", n)
	}
	if !g.stamped() {
		t.Fatal("the watcher's checks left no stamp")
	}
	g.checkStillRunning()

	g.ageStamp()
	wctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	g.srv.reconcileWatch(wctx, run.ID, g.ref, run.AgentExecID)
	err = wctx.Err()
	cancel()
	if err != nil {
		t.Fatalf("the watcher of a confirmed run ran out its context: %v", err)
	}
	g.checkGoneEnd(g.getRun())
}

func TestRunnerProbePG_SweepEndsAConfirmedRun(t *testing.T) {
	g := goneProbeFixture(t, t.Context())
	ctx := t.Context()
	if err := g.st.SetRunAgentExecID(ctx, g.run.ID, "exec-1"); err != nil {
		t.Fatal(err)
	}
	g.exec(`UPDATE agent_runs SET sandbox_absent_at=now()-interval '31 seconds', watcher_heartbeat=now()-interval '1 hour' WHERE id=$1`)
	if err := g.srv.sweepRunWatchers(ctx); err != nil {
		t.Fatal(err)
	}
	g.checkGoneEnd(g.getRun())
}

func TestRunnerProbePG_StaleSessionCheckChangesNothing(t *testing.T) {
	g := goneProbeFixture(t, t.Context())
	ctx := t.Context()
	stale, err := g.st.RunnerProbeClock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := finalStatePage(t, g.srv, g.rn.ID, g.present); err != nil {
		t.Fatalf("present page refused: %v", err)
	}
	g.exec(`UPDATE agent_runs SET sandbox_absent_at=now()-interval '31 seconds' WHERE id=$1`)
	v := g.stampAt()
	if due, stamped, err := g.st.NoteRunnerSandboxProbe(ctx, g.rn.ID, g.run.ID, g.ref, true, stale); due || stamped || err != nil {
		t.Fatalf("a check over a superseded session: due=%v stamped=%v %v", due, stamped, err)
	}
	if !sameAt(g.stampAt(), v) {
		t.Fatalf("a check over a superseded session moved the stamp: %v want %v", g.stampAt(), v)
	}
	if ok, err := g.st.MarkRunnerRunGoneProbed(ctx, g.rn.ID, g.run.ID, g.ref, stale, time.Now()); ok || err != nil {
		t.Fatalf("a check over a superseded session claimed: %v %v", ok, err)
	}
	g.checkStillRunning()
}
