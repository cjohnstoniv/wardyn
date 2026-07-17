// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// U145 counterfactual: the live completion watcher (startCompletionWatcher) and
// the boot reconciler (reconcileFinalize) are the two run-finalize paths that
// used to inline the SAME terminal sequence and had to be hand-kept in sync.
// They now both route through finalizeRunTail, so this test drives each path and
// asserts the identical terminal side effects: a success audit, exactly one
// revoke-cascade, and exactly one sandbox teardown. If either caller stops
// calling the shared tail (re-inlines and drops, e.g., the revoke or the
// teardown), the paths silently diverge and one of these assertions fails.

// syncAudit is a mutex-guarded audit recorder — the completion watcher records
// from its detached goroutine, so recRecorder's lockless append would race.
type syncAudit struct {
	mu     sync.Mutex
	events []types.AuditEvent
}

func (a *syncAudit) Record(_ context.Context, ev types.AuditEvent) error {
	a.mu.Lock()
	a.events = append(a.events, ev)
	a.mu.Unlock()
	return nil
}

func (a *syncAudit) has(runID uuid.UUID, action, outcome string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.events {
		ev := &a.events[i]
		if ev.Action == action && ev.Outcome == outcome && ev.RunID != nil && *ev.RunID == runID {
			return true
		}
	}
	return false
}

// finalizeTailRunner exits cleanly (Wait → 0) so the completion watcher takes its
// inline finalize path, and counts sandbox teardowns.
type finalizeTailRunner struct {
	*fakeRunner
	mu    sync.Mutex
	stops int
}

func (r *finalizeTailRunner) Wait(context.Context, string) (int, error) { return 0, nil }

func (r *finalizeTailRunner) StopSandbox(context.Context, string) error {
	r.mu.Lock()
	r.stops++
	r.mu.Unlock()
	return nil
}

func (r *finalizeTailRunner) stopCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stops
}

func newFinalizeTailServer(t *testing.T, st *dispatchTestStore, brk *raceBroker, rn runner.Runner, audit *syncAudit) *Server {
	t.Helper()
	h := newHarness(t)
	baseCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	cfg.Broker = brk
	cfg.Audit = audit
	cfg.BaseCtx = baseCtx
	return New(cfg)
}

func newFinalizeRun() types.AgentRun {
	now := time.Now().UTC()
	return types.AgentRun{
		ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunRunning,
		RunnerTarget: "docker", Task: "do the thing",
	}
}

func TestFinalizeTail_CompletionWatcherAndReconcile_ShareTerminalSequence(t *testing.T) {
	// Path A — the live completion watcher on a clean agent exit (Wait → 0).
	runA := newFinalizeRun()
	stA := &dispatchTestStore{run: runA, state: types.RunRunning}
	brkA := &raceBroker{}
	rnA := &finalizeTailRunner{fakeRunner: &fakeRunner{}}
	auditA := &syncAudit{}
	srvA := newFinalizeTailServer(t, stA, brkA, rnA, auditA)

	srvA.startCompletionWatcher(runA.ID, "ref-a", "exec-a")

	// The watcher is detached: wait for BOTH the terminal state and the revoke
	// (the cascade fires after the CAS, so a poll that stops at "terminal" can
	// read revocations=0 microseconds early).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) &&
		!(stA.State() == types.RunCompleted && brkA.revocations(runA.ID) > 0) {
		time.Sleep(10 * time.Millisecond)
	}

	if got := stA.State(); got != types.RunCompleted {
		t.Fatalf("watcher: a clean exit 0 must finalize COMPLETED, got %q", got)
	}
	if revs := brkA.revocations(runA.ID); revs != 1 {
		t.Errorf("watcher: the shared tail must revoke exactly once, got %d", revs)
	}
	if rnA.stopCount() != 1 {
		t.Errorf("watcher: the shared tail must tear the sandbox down once, got %d", rnA.stopCount())
	}
	if !auditA.has(runA.ID, "run.complete", "success") {
		t.Error("watcher: the shared tail must emit run.complete/success")
	}

	// Path B — the boot reconciler finalizing a stranded run. Same tail, distinct
	// audit context (run.reconcile) and CAS entry (reads current state first).
	runB := newFinalizeRun()
	stB := &dispatchTestStore{run: runB, state: types.RunRunning}
	brkB := &raceBroker{}
	rnB := &finalizeTailRunner{fakeRunner: &fakeRunner{}}
	auditB := &syncAudit{}
	srvB := newFinalizeTailServer(t, stB, brkB, rnB, auditB)

	srvB.reconcileFinalize(context.Background(), runB.ID, types.RunFailed, "ref-b", "reconciled")

	if got := stB.State(); got != types.RunFailed {
		t.Fatalf("reconcile: run must finalize FAILED, got %q", got)
	}
	if revs := brkB.revocations(runB.ID); revs != 1 {
		t.Errorf("reconcile: the shared tail must revoke exactly once, got %d", revs)
	}
	if rnB.stopCount() != 1 {
		t.Errorf("reconcile: the shared tail must tear the sandbox down once, got %d", rnB.stopCount())
	}
	if !auditB.has(runB.ID, "run.reconcile", "success") {
		t.Error("reconcile: the shared tail must emit run.reconcile/success")
	}
}

var _ runner.Runner = (*finalizeTailRunner)(nil)
