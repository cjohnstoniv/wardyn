// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// deadlineRunner reports what the k8s driver reports for an agent pod the kubelet failed for
// activeDeadlineSeconds (internal/runner/k8s/deadline_test.go pins that status against the
// driver itself): a terminal RunFailed carrying the reason, and no exit code.
type deadlineRunner struct {
	*fakeRunner
	mu      sync.Mutex
	stopped []string
}

func (r *deadlineRunner) AgentStatus(context.Context, string, string) (runner.Status, error) {
	return runner.Status{State: types.RunFailed, Message: "DeadlineExceeded: Pod was active on the node longer than the specified deadline"}, nil
}

func (r *deadlineRunner) StopSandbox(_ context.Context, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = append(r.stopped, ref)
	return nil
}

// TestSweepRunWatchers_DeadlineExceededRunIsFinalizedAndTornDown: a run whose agent pod failed for
// its deadline, with its watcher lease stale (wardynd was down), is finalized FAILED by ONE sweep
// pass and torn down through the runner, which on k8s deletes the Secret, both NetworkPolicies and
// the pods. No code beyond the existing reconciler is involved.
func TestSweepRunWatchers_DeadlineExceededRunIsFinalizedAndTornDown(t *testing.T) {
	h := newHarness(t)
	rn := &deadlineRunner{fakeRunner: &fakeRunner{}}
	run := execRun(t, "agent-exec-id")
	fake := &bootReconcileStore{run: run}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = rn
	cfg.Broker = h.broker
	cfg.BaseCtx = testBaseCtx(t)
	srv := New(cfg)

	if err := srv.sweepRunWatchers(context.Background()); err != nil {
		t.Fatalf("sweepRunWatchers: %v", err)
	}
	if to, got := fake.finalTransition(); !got || to != types.RunFailed {
		t.Fatalf("finalized=%v to=%q, want FAILED from one sweep pass", got, to)
	}
	rn.mu.Lock()
	defer rn.mu.Unlock()
	if len(rn.stopped) != 1 || rn.stopped[0] != run.SandboxRef {
		t.Errorf("StopSandbox calls = %v, want one for %q", rn.stopped, run.SandboxRef)
	}
}

// steppedClockRunner fails every AgentStatus probe, and each failed probe moves the test's clock
// forward by step, so the reconciler's wall-clock ceiling is crossed after a known probe count
// without any sleeping.
type steppedClockRunner struct {
	*fakeRunner
	mu    sync.Mutex
	calls int
	step  time.Duration
	start time.Time
}

func (r *steppedClockRunner) AgentStatus(context.Context, string, string) (runner.Status, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return runner.Status{}, errors.New("k8s: agent status: get pod: connection refused")
}

func (r *steppedClockRunner) now() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.start.Add(time.Duration(r.calls) * r.step)
}

func (r *steppedClockRunner) probes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestReconcileWatch_ProbeErrorsDoNotFinalizeBeforeTheCeiling: a persistently unreadable agent is
// "not yet" until reconcileProbeErrorCeiling has passed, then finalized FAILED. The deadline path
// adds no second rule beside it. The clock moves ten minutes per failed probe, so the ceiling (30
// minutes) is crossed on the fourth: the first error starts the run, the fourth is 30 minutes on.
func TestReconcileWatch_ProbeErrorsDoNotFinalizeBeforeTheCeiling(t *testing.T) {
	prev := reconcileWatchIntervalNS.Swap(int64(2 * time.Millisecond))
	t.Cleanup(func() { reconcileWatchIntervalNS.Store(prev) })

	h := newHarness(t)
	run := execRun(t, "agent-exec-id")
	rn := &steppedClockRunner{fakeRunner: &fakeRunner{}, step: reconcileProbeErrorCeiling / 3, start: time.Now()}
	fake := &bootReconcileStore{run: run, finals: make(chan types.RunState, 2)}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = rn
	cfg.Broker = h.broker
	cfg.Now = rn.now
	ctx := testBaseCtx(t)
	cfg.BaseCtx = ctx
	srv := New(cfg)

	go srv.reconcileWatch(ctx, run.ID, run.SandboxRef, run.AgentExecID)

	select {
	case to := <-fake.finals:
		if to != types.RunFailed {
			t.Fatalf("finalized %q, want FAILED", to)
		}
		if n := rn.probes(); n != 4 {
			t.Fatalf("finalized after %d failed probes, want 4: the ceiling is %s of continuous errors, no more and no less", n, reconcileProbeErrorCeiling)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("a persistently unreadable agent was never finalized (%d probes)", rn.probes())
	}
}

var _ runner.Runner = (*deadlineRunner)(nil)
var _ runner.Runner = (*steppedClockRunner)(nil)
