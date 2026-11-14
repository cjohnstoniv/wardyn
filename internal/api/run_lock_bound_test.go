// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// lockFreesWithin runs fn on a context with no deadline, as the holders here get, and fails unless fn comes back
// within 10 s with the run's lock free.
func lockFreesWithin(t *testing.T, s *Server, runID uuid.UUID, what string, fn func(context.Context)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel) // ends a call the bound did not, so a red run leaks nothing
	done := make(chan struct{})
	go func() { defer close(done); fn(ctx) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s held the run's lock past its bound: a call that never answers never ended", what)
	}
	_, unlock, ok := s.tryLockRunOp(context.Background(), runID)
	if !ok {
		t.Fatalf("%s left the run's lock held", what)
	}
	unlock()
}

// hungClearStore is orphanSweepStore whose clear of a sandbox ref never answers: it waits for its context to end.
type hungClearStore struct {
	*orphanSweepStore
	clears atomic.Int32
}

func (s *hungClearStore) SetSandboxRef(ctx context.Context, id uuid.UUID, ref string) error {
	if ref != "" {
		return s.orphanSweepStore.SetSandboxRef(ctx, id, ref)
	}
	s.clears.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

// TestReconcileOrphanedSandbox_AHungStoreCallEndsAtItsBound: the boot orphan pass holds a terminal run's lock on the
// boot context, which has no deadline. A store call under it that never answers must end at heldStoreCallBound: the
// pass returns and the run's lock is free again.
func TestReconcileOrphanedSandbox_AHungStoreCallEndsAtItsBound(t *testing.T) {
	old := heldStoreCallBound
	heldStoreCallBound = 50 * time.Millisecond
	t.Cleanup(func() { heldStoreCallBound = old })
	h := newHarness(t)
	now := time.Now().UTC()
	run := types.AgentRun{ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunFailed, RunnerTarget: "docker", SandboxRef: "orphan-hung-clear"}
	st := &hungClearStore{orphanSweepStore: &orphanSweepStore{runs: []types.AgentRun{run}}}
	rn := &orphanSweepRunner{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	srv := testServer(t, cfg)

	lockFreesWithin(t, srv, run.ID, "the boot orphan pass", func(ctx context.Context) {
		if err := srv.reconcileOrphanedSandbox(ctx); err != nil {
			t.Error(err)
		}
	})
	if rn.stopCount("orphan-hung-clear") != 1 || st.clears.Load() == 0 {
		t.Fatalf("stops %d, ref clears %d; want the sandbox stopped and its ref clear reached",
			rn.stopCount("orphan-hung-clear"), st.clears.Load())
	}
}

// hungPullRunner is reviveRunner whose proxy image pull never answers: it waits for its context to end.
type hungPullRunner struct {
	*reviveRunner
	pulls atomic.Int32
}

func (r *hungPullRunner) EnsureProxyImage(ctx context.Context) error {
	r.pulls.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

// TestReviveRunProxy_AHostedRunsHungPullEndsAtItsBound: a revive holds the run's lock from before its claim, on the
// request's context. A hosted run's proxy image pull that never answers must end at reviveRunnerTimeout, as a runner
// run's does: the revive is refused and the run's lock is free again.
func TestReviveRunProxy_AHostedRunsHungPullEndsAtItsBound(t *testing.T) {
	old := reviveRunnerTimeout
	reviveRunnerTimeout = 2 * time.Second
	t.Cleanup(func() { reviveRunnerTimeout = old })
	f := newReviveFixture(t)
	if f.run.RunnerID != nil {
		t.Fatal("fixture: want a hosted run")
	}
	rn := &hungPullRunner{reviveRunner: f.rr}
	f.srv.cfg.Runner = rn
	var rerr *reviveError
	lockFreesWithin(t, f.srv, f.run.ID, "the revive", func(ctx context.Context) {
		_, rerr = f.srv.reviveRunProxy(ctx, f.run, types.ActorHuman, "synthetic-owner", true)
	})
	if rerr == nil || rn.pulls.Load() == 0 {
		t.Fatalf("revive error %v, pulls %d; want the pull reached and the revive refused", rerr, rn.pulls.Load())
	}
}

// hungStatusRunner is sweepAliveRunner whose status probe never answers: it waits for its context to end.
type hungStatusRunner struct {
	*sweepAliveRunner
	probes atomic.Int32
}

func (r *hungStatusRunner) Status(ctx context.Context, _ string) (runner.Status, error) {
	r.probes.Add(1)
	<-ctx.Done()
	return runner.Status{}, ctx.Err()
}

// hungStopRunner is sweepAliveRunner whose sandbox stop never answers: it waits for its context to end.
type hungStopRunner struct {
	*sweepAliveRunner
	stops atomic.Int32
}

func (r *hungStopRunner) StopSandbox(ctx context.Context, _ string) error {
	r.stops.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

// TestSweepTerminalSandboxes_AHungRuntimeCallEndsAtItsBound: the operator's sweep holds each terminal run's lock on
// the request's context, which has no deadline. A status probe or a sandbox stop that never answers must end at its
// own bound (sweepProbeBound, sweepStopBound): the sweep returns and the run's lock is free again.
func TestSweepTerminalSandboxes_AHungRuntimeCallEndsAtItsBound(t *testing.T) {
	oldProbe, oldStop := sweepProbeBound, sweepStopBound
	sweepProbeBound, sweepStopBound = 50*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { sweepProbeBound, sweepStopBound = oldProbe, oldStop })

	t.Run("a status probe", func(t *testing.T) {
		h := newHarness(t)
		orphan := sweepRun(types.RunCompleted, "sbx-hung-probe")
		cfg := baseTestConfig(h, &sweepStore{runs: []types.AgentRun{orphan}})
		rn := &hungStatusRunner{sweepAliveRunner: &sweepAliveRunner{fakeRunner: &fakeRunner{}}}
		cfg.Runner = rn
		srv := New(cfg)
		var swept int
		lockFreesWithin(t, srv, orphan.ID, "the sweep", func(ctx context.Context) {
			swept, _ = srv.SweepTerminalSandboxes(ctx)
		})
		if swept != 0 || rn.probes.Load() == 0 {
			t.Fatalf("swept %d, probes %d; want the probe reached and the run left for the next pass", swept, rn.probes.Load())
		}
	})
	t.Run("a sandbox stop", func(t *testing.T) {
		h := newHarness(t)
		orphan := sweepRun(types.RunCompleted, "sbx-hung-stop")
		cfg := baseTestConfig(h, &sweepStore{runs: []types.AgentRun{orphan}})
		rn := &hungStopRunner{sweepAliveRunner: &sweepAliveRunner{fakeRunner: &fakeRunner{}}}
		cfg.Runner = rn
		cfg.Broker = h.broker
		srv := New(cfg)
		lockFreesWithin(t, srv, orphan.ID, "the sweep", func(ctx context.Context) {
			_, _ = srv.SweepTerminalSandboxes(ctx)
		})
		if rn.stops.Load() == 0 {
			t.Fatal("the sweep never reached the sandbox stop")
		}
	})
}

// hungRevokeIdentity is an identity.Provider whose run revoke never answers: it waits for its context to end.
type hungRevokeIdentity struct {
	identity.Provider
	calls atomic.Int32
}

func (i *hungRevokeIdentity) RevokeRun(ctx context.Context, _ uuid.UUID) error {
	i.calls.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

// TestEndHold_AHungRevokeEndsAtItsBound: the completion watcher and a person's bring-back hold a run's end on a
// context with no deadline. A revoke that never answers must end at heldRevokeBound, never at the hold's deadline
// alone: the holder returns and the run's lock is free again.
func TestEndHold_AHungRevokeEndsAtItsBound(t *testing.T) {
	old := heldRevokeBound
	heldRevokeBound = 50 * time.Millisecond
	t.Cleanup(func() { heldRevokeBound = old })

	t.Run("the completion watcher's tail", func(t *testing.T) {
		s, _, _, run, _, _ := computerFolderFixture(t, types.ComputerFolderBoth)
		idp := &hungRevokeIdentity{Provider: s.cfg.Identity}
		s.cfg.Identity = idp
		lockFreesWithin(t, s, run.ID, "the watcher's tail", func(ctx context.Context) {
			hctx, unlock, err := s.holdRunEnd(ctx, run.ID, endHoldDedicated)
			if err != nil {
				t.Error(err)
				return
			}
			defer unlock()
			s.finalizeRunTail(hctx, run.ID, "sbx", "run.complete", "success", map[string]any{"exit_code": 0})
		})
		if idp.calls.Load() == 0 {
			t.Fatal("the tail never reached the revoke")
		}
	})
	t.Run("a person's bring-back", func(t *testing.T) {
		s, _, _, run, _, _ := bringBackFixture(t)
		idp := &hungRevokeIdentity{Provider: s.cfg.Identity}
		s.cfg.Identity = idp
		cookie := bringBackOwnerSession(t)
		var res *bringBackResult
		lockFreesWithin(t, s, run.ID, "the bring-back", func(context.Context) { res = bringBack(t, s, cookie, run) })
		if res.code != http.StatusOK || idp.calls.Load() == 0 {
			t.Fatalf("bring-back = %d %s, revokes %d; want it answered with the revoke reached", res.code, res.body, idp.calls.Load())
		}
	})
}

// hungMarkStore is owedStore whose first owed mark never answers: it waits for its context to end, as a store that
// stalls does. Later marks are owedStore's.
type hungMarkStore struct {
	*owedStore
	hung atomic.Bool
}

func (s *hungMarkStore) MarkRevokeOwed(ctx context.Context, id uuid.UUID) error {
	if s.hung.CompareAndSwap(false, true) {
		<-ctx.Done()
		return ctx.Err()
	}
	return s.owedStore.MarkRevokeOwed(ctx, id)
}

// TestEndHold_ARevokeCutAtItsBoundStaysOwed: a revoke cut at heldRevokeBound stays owed for the revoke queue once its
// owed mark is written, and the queue retries it. A hung identity revoke leaves the first mark written. A hung first
// mark leaves the second one, on its own teardown bound (settleRevokeOwed), and nothing more is promised there.
func TestEndHold_ARevokeCutAtItsBoundStaysOwed(t *testing.T) {
	old := heldRevokeBound
	heldRevokeBound = 50 * time.Millisecond
	t.Cleanup(func() { heldRevokeBound = old })
	ctx := context.Background()

	t.Run("a hung identity revoke", func(t *testing.T) {
		st, id := newOwedStore(), uuid.New()
		srv, _ := revokeOwedServer(t, st, newFlakyBroker(0))
		idp := &hungRevokeIdentity{Provider: srv.cfg.Identity}
		srv.cfg.Identity = idp
		heldCall(ctx, heldRevokeBound, func(ctx context.Context) { _ = srv.revokeRunCascade(ctx, id) })
		if idp.calls.Load() == 0 || !st.isOwed(id) {
			t.Fatalf("identity calls %d, owed %v; want the revoke reached and left owed", idp.calls.Load(), st.isOwed(id))
		}
		srv.cfg.Identity = idp.Provider
		st.age(id)
		srv.revokeOwedPass(ctx, nil)
		if st.isOwed(id) {
			t.Fatal("the revoke queue did not retry and clear a revoke cut at its bound")
		}
	})
	t.Run("a hung owed mark", func(t *testing.T) {
		st, id := &hungMarkStore{owedStore: newOwedStore()}, uuid.New()
		srv, _ := revokeOwedServer(t, st, newFlakyBroker(1))
		heldCall(ctx, heldRevokeBound, func(ctx context.Context) { _ = srv.revokeRunCascade(ctx, id) })
		if !st.hung.Load() || st.markCalls() != 1 || !st.isOwed(id) {
			t.Fatalf("first mark hung %v, later marks %d, owed %v; want the second mark written on its own bound",
				st.hung.Load(), st.markCalls(), st.isOwed(id))
		}
		st.age(id)
		srv.revokeOwedPass(ctx, nil)
		if st.isOwed(id) {
			t.Fatal("the revoke queue did not retry and clear a revoke whose first mark hung")
		}
	})
}

// liveCtxRecorder is recRecorder that refuses a row whose context has ended, as the store's insert does.
type liveCtxRecorder struct{ recRecorder }

func (r *liveCtxRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.recRecorder.Record(ctx, ev)
}

// TestSweepTerminalSandboxes_AStopCutAtItsBoundIsStillRecorded: a sandbox stop the sweep cuts at sweepStopBound still
// gets its sandbox.sweep failure row, written on a bound of its own (settleAfterTeardown), not on the stop's ended
// context.
func TestSweepTerminalSandboxes_AStopCutAtItsBoundIsStillRecorded(t *testing.T) {
	old := sweepStopBound
	sweepStopBound = 50 * time.Millisecond
	t.Cleanup(func() { sweepStopBound = old })
	h := newHarness(t)
	orphan := sweepRun(types.RunCompleted, "sbx-cut-stop")
	cfg := baseTestConfig(h, &sweepStore{runs: []types.AgentRun{orphan}})
	rec := &liveCtxRecorder{}
	cfg.Audit = rec
	cfg.Runner = &hungStopRunner{sweepAliveRunner: &sweepAliveRunner{fakeRunner: &fakeRunner{}}}
	cfg.Broker = h.broker
	srv := New(cfg)
	_, _ = srv.SweepTerminalSandboxes(context.Background())
	if findAudit(rec.snapshot(), orphan.ID, sandboxSweepAction, "failure") == nil {
		t.Fatal("a stop cut at its bound left no sandbox.sweep failure row")
	}
}
