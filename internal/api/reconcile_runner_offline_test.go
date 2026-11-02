// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// offlineHereRunner answers every agent probe as a replica with no session to the run's runner does
// (remote Link.Call's errOffline), and counts the probes.
type offlineHereRunner struct {
	*fakeRunner
	mu    sync.Mutex
	calls int
}

func (r *offlineHereRunner) AgentStatus(context.Context, string, string) (runner.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return runner.Status{}, fmt.Errorf("%w: no session", runner.ErrRunnerOffline)
}

func (r *offlineHereRunner) probes() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// offlineHereServer is a server whose runner has no session here, watching run, a runner run on a
// registered runner unless ref replaces its sandbox ref.
func offlineHereServer(t *testing.T, ref string) (*Server, *offlineHereRunner, *bootReconcileStore, types.AgentRun) {
	t.Helper()
	prev := reconcileWatchIntervalNS.Swap(int64(2 * time.Millisecond))
	t.Cleanup(func() { reconcileWatchIntervalNS.Store(prev) })
	h := newHarness(t)
	rid := uuid.New()
	run := execRun(t, "exec-1")
	run.Placement, run.RunnerID, run.SandboxRef = types.PlacementLocal, &rid, placement.RunnerRefPrefix(rid.String())+"sbx"
	if ref != "" {
		run.SandboxRef = ref
	}
	rn := &offlineHereRunner{fakeRunner: &fakeRunner{}}
	fake := &bootReconcileStore{run: run}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = rn
	cfg.Broker = h.broker
	return testServer(t, cfg), rn, fake, run
}

// TestReconcileWatch_RunnerRunOfflineHereStops pins L4(c) (#3667 F1): a watcher whose probe of a
// runner run answers ErrRunnerOffline stops at once, leaving the lease to expire so a replica holding
// the runner's session adopts the run. Any other ref keeps today's backoff.
func TestReconcileWatch_RunnerRunOfflineHereStops(t *testing.T) {
	t.Run("runner ref", func(t *testing.T) {
		srv, rn, _, run := offlineHereServer(t, "")
		done := make(chan struct{})
		srv.goBackground(func() {
			srv.reconcileWatch(srv.cfg.BaseCtx, run.ID, run.SandboxRef, run.AgentExecID)
			close(done)
		})
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("a watcher with no session to the run's runner kept watching (%d probes)", rn.probes())
		}
		if n := rn.probes(); n != 1 {
			t.Fatalf("probes = %d, want 1", n)
		}
	})
	t.Run("container ref", func(t *testing.T) {
		srv, rn, _, run := offlineHereServer(t, "sleep-infinity-"+uuid.NewString())
		done := make(chan struct{})
		srv.goBackground(func() {
			srv.reconcileWatch(srv.cfg.BaseCtx, run.ID, run.SandboxRef, run.AgentExecID)
			close(done)
		})
		time.Sleep(200 * time.Millisecond)
		select {
		case <-done:
			t.Fatal("a watcher of a container ref stopped on an offline error; want backoff")
		default:
		}
		if n := rn.probes(); n < 2 {
			t.Fatalf("probes = %d, want at least 2", n)
		}
	})
}

// TestSweepRunWatchers_RunnerRunOfflineHereGetsNoWatcher pins L4(a) (#3667 F1): the sweep attaches no
// watcher, so no lease stamp, to a runner run whose probe answers ErrRunnerOffline here.
func TestSweepRunWatchers_RunnerRunOfflineHereGetsNoWatcher(t *testing.T) {
	srv, rn, fake, _ := offlineHereServer(t, "")
	fake.beats = make(chan uuid.UUID, 4)
	if err := srv.sweepRunWatchers(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-fake.beats:
		t.Fatalf("a watcher attached to run %s with no session to its runner", id)
	case <-time.After(200 * time.Millisecond):
	}
	if n := rn.probes(); n != 1 {
		t.Fatalf("probes = %d, want 1", n)
	}
}
