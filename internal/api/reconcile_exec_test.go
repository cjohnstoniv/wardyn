// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// execExitRunner models the exact U008/U039 shape: the sandbox CONTAINER is still
// up (an idle `sleep infinity`, so container-level Status reports RUNNING), but the
// AGENT exec has already exited. AgentStatus — given the exec id persisted at Exec
// — reports the agent's real exit; the old container-Status boot reconciler never
// could, so a crashed-and-restarted exec run was stranded RUNNING forever with a
// live sandbox + un-revoked credentials.
type execExitRunner struct {
	*fakeRunner
	agentExit int
	stopped   []string
}

func (r *execExitRunner) Status(context.Context, string) (runner.Status, error) {
	return runner.Status{State: types.RunRunning}, nil // idle container still up
}
func (r *execExitRunner) AgentStatus(_ context.Context, _, execID string) (runner.Status, error) {
	if execID == "" { // exec-less / main-process: container IS the agent
		return runner.Status{State: types.RunRunning}, nil
	}
	code := r.agentExit
	return runner.Status{State: types.RunStopped, ExitCode: &code}, nil
}
func (r *execExitRunner) StopSandbox(_ context.Context, ref string) error {
	r.stopped = append(r.stopped, ref)
	return nil
}

// bootReconcileStore serves a single stranded RUNNING run to ReconcileOnBoot and
// records the terminal transition the reconciler applies (via UpdateRunStateIf).
type bootReconcileStore struct {
	store.Store
	run          types.AgentRun
	toState      types.RunState
	transitioned bool
}

func (s *bootReconcileStore) ListRuns(context.Context) ([]types.AgentRun, error) {
	return []types.AgentRun{s.run}, nil
}
func (s *bootReconcileStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	if id == s.run.ID {
		return s.run, nil
	}
	return types.AgentRun{}, store.ErrNotFound
}
func (s *bootReconcileStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, _, to types.RunState) (bool, error) {
	s.toState, s.transitioned = to, true
	return true, nil
}

func execRun(t *testing.T, agentExecID string) types.AgentRun {
	t.Helper()
	runID := uuid.New()
	now := time.Now().UTC()
	return types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunRunning,
		RunnerTarget: "docker",
		SandboxRef:   "sleep-infinity-" + runID.String(),
		AgentExecID:  agentExecID, // the value persisted at Exec time (U008)
	}
}

// TestReconcileOnBoot_ExecRunFinalizesFromAgentExit is the U008/U039 regression:
// after a restart, a RUNNING exec-based run whose agent has exited (but whose idle
// sandbox container is still up) MUST finalize + revoke + tear down, not strand.
// The reconciler now observes AgentStatus (the persisted exec id) instead of
// container Status, which for an idle `sleep infinity` reports RUNNING forever.
// Counterfactual: the runner's container Status IS RUNNING here — with the old
// code the run is re-attached and never finalized, so `transitioned` stays false.
func TestReconcileOnBoot_ExecRunFinalizesFromAgentExit(t *testing.T) {
	h := newHarness(t)
	fr := &execExitRunner{fakeRunner: &fakeRunner{}, agentExit: 0}
	run := execRun(t, "agent-exec-id")
	fake := &bootReconcileStore{run: run}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = fr
	cfg.Broker = h.broker
	srv := New(cfg)

	if err := srv.ReconcileOnBoot(context.Background()); err != nil {
		t.Fatalf("reconcile on boot: %v", err)
	}

	if !fake.transitioned || fake.toState != types.RunCompleted {
		t.Fatalf("an exec run whose agent exited 0 must finalize COMPLETED via AgentStatus (U008/U039); transitioned=%v to=%q — container Status alone reports RUNNING forever", fake.transitioned, fake.toState)
	}
	if len(fr.stopped) == 0 {
		t.Error("finalize must tear the still-up idle sandbox down")
	}
	revoked := false
	for _, id := range h.broker.revoked {
		if id == run.ID {
			revoked = true
		}
	}
	if !revoked {
		t.Errorf("finalize must run the credential revoke cascade for %s (the C3 property U039 says was defeated); broker.revoked=%v", run.ID, h.broker.revoked)
	}
}

// TestReconcileOnBoot_ExecRunFailsFromNonZeroExit checks the exit-code mapping — a
// non-zero agent exit finalizes FAILED, not COMPLETED.
func TestReconcileOnBoot_ExecRunFailsFromNonZeroExit(t *testing.T) {
	h := newHarness(t)
	fr := &execExitRunner{fakeRunner: &fakeRunner{}, agentExit: 1}
	fake := &bootReconcileStore{run: execRun(t, "agent-exec-id")}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = fr
	cfg.Broker = h.broker
	srv := New(cfg)

	if err := srv.ReconcileOnBoot(context.Background()); err != nil {
		t.Fatalf("reconcile on boot: %v", err)
	}
	if !fake.transitioned || fake.toState != types.RunFailed {
		t.Fatalf("a non-zero agent exit must finalize FAILED; transitioned=%v to=%q", fake.transitioned, fake.toState)
	}
}

var _ runner.Runner = (*execExitRunner)(nil)
