// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sandboxOrphanStore is a minimal Store whose ListRuns feeds the sandbox orphan
// sweep. It embeds store.Store (nil) so it does NOT satisfy RunWatcherLeaser —
// hasLease is false, exactly the single-process demo path.
type sandboxOrphanStore struct {
	store.Store
	runs []types.AgentRun
}

func (s *sandboxOrphanStore) ListRuns(context.Context) ([]types.AgentRun, error) { return s.runs, nil }

// sandboxOrphanRunner models the substrate side: `live` are the run ids with a
// labeled container still alive; SweepOrphanedSandboxes asks isOrphan about each
// and records which it tore down. (The real docker driver adds the ContainerList
// + age gate; this fake exercises the control-plane verdict wiring — D13's
// "TEST (fake runner)".)
type sandboxOrphanRunner struct {
	*fakeRunner
	live     []uuid.UUID
	tornDown []uuid.UUID
}

func (r *sandboxOrphanRunner) SweepOrphanedSandboxes(_ context.Context, _ time.Duration, isOrphan func(uuid.UUID) bool) (int, error) {
	var n int
	for _, id := range r.live {
		if isOrphan(id) {
			r.tornDown = append(r.tornDown, id)
			n++
		}
	}
	return n, nil
}

// TestSweepOrphanedSandboxes_TearsDownRefEmptyTerminalRun pins D13: a run that
// crashed before SetSandboxRef leaves its labeled container running under a
// terminal, ref-empty row that every ref-keyed teardown skips. The sandbox
// orphan sweep must identify it and tear it down — while leaving a live,
// ref-tracked run's container alone.
func TestSweepOrphanedSandboxes_TearsDownRefEmptyTerminalRun(t *testing.T) {
	h := newHarness(t)

	orphan := uuid.New()  // terminal, ref EMPTY -> leaked container, must be swept
	live := uuid.New()    // RUNNING with a ref -> tracked, must NOT be swept
	missing := uuid.New() // no run row at all -> pure leak, must be swept

	st := &sandboxOrphanStore{runs: []types.AgentRun{
		{ID: orphan, State: types.RunFailed, SandboxRef: ""},
		{ID: live, State: types.RunRunning, SandboxRef: "sandbox-live"},
	}}
	rn := &sandboxOrphanRunner{fakeRunner: &fakeRunner{}, live: []uuid.UUID{orphan, live, missing}}

	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	srv := New(cfg)

	if err := srv.sweepOrphanedSandboxes(context.Background()); err != nil {
		t.Fatalf("sweepOrphanedSandboxes: %v", err)
	}

	swept := map[uuid.UUID]bool{}
	for _, id := range rn.tornDown {
		swept[id] = true
	}
	if !swept[orphan] {
		t.Error("a terminal ref-empty run's labeled container must be torn down (the D13 leak)")
	}
	if !swept[missing] {
		t.Error("a labeled container with no run row at all must be torn down")
	}
	if swept[live] {
		t.Error("a live, ref-tracked run's container must NOT be swept")
	}
}
