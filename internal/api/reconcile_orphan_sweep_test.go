// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// P1 W15-W15c-terminal-lifecycle-4: a terminal run's SandboxRef only survives
// finalizeRunTail non-empty when its StopSandbox call FAILED — and
// ReconcileOnBoot's `isTerminalRunState(run.State) { continue }` guard means
// that run is never looked at again. The abandoned container (and its proxy
// sidecar, which resolved injected credential VALUES into memory at startup)
// is audited once, at finalize time, and then permanent. These tests pin
// reconcileOrphanedSandbox: ReconcileOnBoot must retry the teardown of every
// terminal run that still carries a ref, clearing the ref on success.

// orphanSweepStore is an in-memory Store exposing just the surface
// ReconcileOnBoot's terminal-run sweep drives: ListRuns + SetSandboxRef.
// Deliberately NOT the PG harness — this invariant must be provable without
// WARDYN_TEST_PG.
type orphanSweepStore struct {
	store.Store
	mu          sync.Mutex
	runs        []types.AgentRun
	clearedRefs []uuid.UUID
}

func (s *orphanSweepStore) ListRuns(context.Context) ([]types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]types.AgentRun, len(s.runs))
	copy(out, s.runs)
	return out, nil
}

func (s *orphanSweepStore) SetSandboxRef(_ context.Context, id uuid.UUID, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.runs {
		if s.runs[i].ID == id {
			s.runs[i].SandboxRef = ref
		}
	}
	if ref == "" {
		s.clearedRefs = append(s.clearedRefs, id)
	}
	return nil
}

// orphanSweepRunner records every StopSandbox call and can be told to fail on
// a given ref.
type orphanSweepRunner struct {
	*fakeRunner
	mu      sync.Mutex
	stopped []string
	failRef string
}

func (r *orphanSweepRunner) StopSandbox(_ context.Context, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopped = append(r.stopped, ref)
	if ref == r.failRef {
		return context.DeadlineExceeded
	}
	return nil
}

func (r *orphanSweepRunner) stopCount(ref string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.stopped {
		if s == ref {
			n++
		}
	}
	return n
}

// TestReconcileOnBoot_SweepsOrphanedTerminalSandbox is the counterfactual for
// W15-W15c-terminal-lifecycle-4: on base 763beb5, ReconcileOnBoot's terminal
// guard skips this run outright and StopSandbox is never called for it — the
// container leaks forever. After the fix, the sweep tears it down and clears
// the ref so the run drops out of future sweeps.
func TestReconcileOnBoot_SweepsOrphanedTerminalSandbox(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	now := time.Now().UTC()
	st := &orphanSweepStore{runs: []types.AgentRun{{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunFailed,
		RunnerTarget: "docker", SandboxRef: "orphan-container-1",
	}}}
	rn := &orphanSweepRunner{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	srv := New(cfg)

	if err := srv.ReconcileOnBoot(context.Background()); err != nil {
		t.Fatalf("ReconcileOnBoot: %v", err)
	}

	if n := rn.stopCount("orphan-container-1"); n != 1 {
		t.Fatalf("terminal run's orphaned sandbox must be torn down exactly once on boot, got %d StopSandbox calls", n)
	}
	st.mu.Lock()
	gotRef := st.runs[0].SandboxRef
	cleared := len(st.clearedRefs)
	st.mu.Unlock()
	if gotRef != "" {
		t.Errorf("SandboxRef must be cleared after a successful sweep teardown, got %q", gotRef)
	}
	if cleared != 1 {
		t.Errorf("expected exactly one cleared ref, got %d", cleared)
	}
}

// TestReconcileOnBoot_OrphanSweepFailureAudited: when the retried teardown
// STILL fails, the ref must stay set (so the NEXT boot retries again) and the
// failure must be audited with teardown_error — silence would re-create the
// exact "audited once, then permanent" gap this fix closes.
func TestReconcileOnBoot_OrphanSweepFailureAudited(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	now := time.Now().UTC()
	st := &orphanSweepStore{runs: []types.AgentRun{{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunFailed,
		RunnerTarget: "docker", SandboxRef: "still-stuck",
	}}}
	rn := &orphanSweepRunner{fakeRunner: &fakeRunner{}, failRef: "still-stuck"}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	srv := New(cfg)

	if err := srv.ReconcileOnBoot(context.Background()); err != nil {
		t.Fatalf("ReconcileOnBoot: %v", err)
	}

	if n := rn.stopCount("still-stuck"); n != 1 {
		t.Fatalf("sweep must attempt the retry exactly once per boot, got %d", n)
	}
	st.mu.Lock()
	gotRef := st.runs[0].SandboxRef
	st.mu.Unlock()
	if gotRef != "still-stuck" {
		t.Errorf("a still-failing teardown must leave the ref set for the next boot to retry, got %q", gotRef)
	}
	var found *types.AuditEvent
	for i := range h.audit.events {
		ev := &h.audit.events[i]
		if ev.RunID != nil && *ev.RunID == runID && strings.Contains(string(ev.Data), "teardown_error") {
			found = ev
		}
	}
	if found == nil {
		t.Fatal("a still-failing orphan-sweep teardown must be audited with teardown_error")
	}
}
