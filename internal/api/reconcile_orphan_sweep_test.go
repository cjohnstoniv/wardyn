// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A terminal run's SandboxRef only survives finalizeRunTail non-empty when
// its StopSandbox call failed — and ReconcileOnBoot's
// `isTerminalRunState(run.State) { continue }` guard means the main loop
// never looks at that run again. The abandoned container (and its proxy
// sidecar, which resolved injected credential VALUES into memory at startup)
// would be audited once, at finalize time, and then be permanent. These
// tests pin
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

func (s *orphanSweepStore) SetRunDiskMiB(context.Context, uuid.UUID, int) error { return nil }

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
// that guard: on its own, ReconcileOnBoot's terminal guard skips this run
// outright and StopSandbox is never called for it — the container leaks
// forever. The sweep tears it down and clears the ref so the run drops out of
// future sweeps.
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
	srv := testServer(t, cfg)

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

// TestReconcileOnBoot_OrphanSweepRetriesRevoke: a terminal run's SandboxRef
// survives only when its original finalizeRunTail teardown failed, but
// finalizeRunTail runs revokeRunCascade before that teardown — so the run
// reaching this boot pass may carry an un-revoked identity/broker credential
// independent of whether the retried teardown itself succeeds. An orphan
// sweep that retried only StopSandbox would leave that credential
// un-revoked. The boot pass must re-run the revoke cascade for every
// terminal run it walks.
func TestReconcileOnBoot_OrphanSweepRetriesRevoke(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	now := time.Now().UTC()
	st := &orphanSweepStore{runs: []types.AgentRun{{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunFailed,
		RunnerTarget: "docker", SandboxRef: "orphan-container-2",
	}}}
	rn := &orphanSweepRunner{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	cfg.Broker = h.broker
	srv := testServer(t, cfg)

	if err := srv.ReconcileOnBoot(context.Background()); err != nil {
		t.Fatalf("ReconcileOnBoot: %v", err)
	}

	revoked := false
	for _, id := range h.broker.revoked {
		if id == runID {
			revoked = true
		}
	}
	if !revoked {
		t.Errorf("boot orphan sweep must re-run the credential revoke cascade for %s; broker.revoked=%v", runID, h.broker.revoked)
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
	srv := testServer(t, cfg)

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

// refusedStopRunner answers each ref's direct stop and kill with the error given, and records kills.
type refusedStopRunner struct {
	*fakeRunner
	mu     sync.Mutex
	stops  map[string]error
	kills  map[string]error
	killed []string
}

func (r *refusedStopRunner) StopSandbox(_ context.Context, ref string) error { return r.stops[ref] }

func (r *refusedStopRunner) KillSandbox(_ context.Context, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.killed = append(r.killed, ref)
	return r.kills[ref]
}

// TestReconcileOrphanSweep_KillsARunnerSandboxWhoseStopIsRefused: an online runner's record kept only
// for containment refuses the sweep's direct stop and leaves no answer to follow at the next boot, so
// the sweep follows the refusal with one kill, which such a record takes. A stop queued for an offline
// runner and a sandbox on no runner get no kill, and each failed chain is audited once.
func TestReconcileOrphanSweep_KillsARunnerSandboxWhoseStopIsRefused(t *testing.T) {
	h := newHarness(t)
	runnerID := uuid.New()
	refused := placement.RunnerRefPrefix(runnerID.String()) + "refused"
	pending := placement.RunnerRefPrefix(runnerID.String()) + "pending"
	stuck := placement.RunnerRefPrefix(runnerID.String()) + "stuck"
	local := "local"
	now := time.Now().UTC()
	var runs []types.AgentRun
	for _, ref := range []string{refused, pending, stuck, local} {
		run := types.AgentRun{ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
			Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunFailed, RunnerTarget: "docker", SandboxRef: ref}
		if ref != local {
			run.RunnerID = &runnerID
		}
		runs = append(runs, run)
	}
	st := &orphanSweepStore{runs: runs}
	refusal := errors.New("containment-only identity refuses this operation")
	rn := &refusedStopRunner{fakeRunner: &fakeRunner{},
		stops: map[string]error{refused: refusal, pending: runner.ErrPendingOnRunner, stuck: refusal, local: refusal},
		kills: map[string]error{stuck: context.DeadlineExceeded}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	srv := testServer(t, cfg)

	if err := srv.reconcileOrphanedSandbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rn.killed, ","); got != refused+","+stuck {
		t.Fatalf("kills %q, want one each for the refused stops on the runner", got)
	}
	for i, want := range []string{"", pending, stuck, local} {
		if got := st.runs[i].SandboxRef; got != want {
			t.Errorf("run %d: ref %q, want %q", i, got, want)
		}
		failures := 0
		for _, ev := range h.audit.snapshot() {
			if ev.RunID != nil && *ev.RunID == st.runs[i].ID && ev.Action == "sandbox.orphan.sweep" && ev.Outcome == "failure" {
				failures++
			}
		}
		if wantFailures := min(len(want), 1); failures != wantFailures {
			t.Errorf("run %d: %d failure rows, want %d", i, failures, wantFailures)
		}
	}
}

// orphanDeadlineRunner records whether each substrate call of the orphan passes carried a deadline.
type orphanDeadlineRunner struct {
	*fakeRunner
	stopBounded, sweepBounded bool
}

func (r *orphanDeadlineRunner) StopSandbox(ctx context.Context, _ string) error {
	_, r.stopBounded = ctx.Deadline()
	return nil
}

func (r *orphanDeadlineRunner) SweepOrphanedSandboxes(ctx context.Context, _ time.Duration, _ func(uuid.UUID) bool) (int, error) {
	_, r.sweepBounded = ctx.Deadline()
	return 0, nil
}

// A substrate that accepts the call and never answers must not hold boot (the control plane would never
// serve) or the single-threaded sweeper: the boot teardown of an orphan and the orphan sweep each run
// under a deadline of their own, whatever the caller's context.
func TestOrphanPassesBoundTheirSubstrateCalls(t *testing.T) {
	h := newHarness(t)
	now := time.Now().UTC()
	st := &orphanSweepStore{runs: []types.AgentRun{{ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunFailed, RunnerTarget: "docker", SandboxRef: "local"}}}
	rn := &orphanDeadlineRunner{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	srv := testServer(t, cfg)
	if err := srv.reconcileOrphanedSandbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := srv.sweepOrphanedSandboxes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rn.stopBounded || !rn.sweepBounded {
		t.Fatalf("orphan teardown bounded %v, orphan sweep bounded %v; want both", rn.stopBounded, rn.sweepBounded)
	}
}
