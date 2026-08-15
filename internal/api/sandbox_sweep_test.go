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

// sweepAliveRunner reports EVERY ref as still RUNNING — the orphan shape a
// failed StopSandbox/RevokeRun step in finalizeRunTail (or handleKillRun)
// leaves behind: a terminal run row with a live sandbox nothing else revisits.
type sweepAliveRunner struct {
	*fakeRunner
	stopped []string
}

func (r *sweepAliveRunner) Status(context.Context, string) (runner.Status, error) {
	return runner.Status{State: types.RunRunning}, nil
}
func (r *sweepAliveRunner) StopSandbox(_ context.Context, ref string) error {
	r.stopped = append(r.stopped, ref)
	return nil
}

// sweepGoneRunner reports every ref as already stopped — the normal,
// successfully-torn-down case the sweep must leave alone.
type sweepGoneRunner struct {
	*fakeRunner
	stopped []string
}

func (r *sweepGoneRunner) Status(context.Context, string) (runner.Status, error) {
	return runner.Status{State: types.RunStopped}, nil
}
func (r *sweepGoneRunner) StopSandbox(_ context.Context, ref string) error {
	r.stopped = append(r.stopped, ref) // must never be called in the "gone" test
	return nil
}

// sweepStore serves a fixed run list to ListRuns; the sweep never writes state.
type sweepStore struct {
	store.Store
	runs []types.AgentRun
}

func (s *sweepStore) ListRuns(context.Context) ([]types.AgentRun, error) { return s.runs, nil }

func sweepRun(state types.RunState, sandboxRef string) types.AgentRun {
	now := time.Now().UTC()
	return types.AgentRun{
		ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: state,
		RunnerTarget: "docker", SandboxRef: sandboxRef,
	}
}

// TestSweepTerminalSandboxes_TearsDownOrphanedLiveSandbox is the regression for
// W22-S1-7: a terminal run (COMPLETED here — FAILED/STOPPED/ARCHIVED/KILLED hit
// the identical gap) whose sandbox is STILL running because a prior finalize's
// teardown/revoke step failed has NO other in-product retry surface —
// ReconcileOnBoot skips terminal runs outright (reconcile.go), and
// handleKillRun 409s a non-KILLED terminal run rather than corrupt its
// recorded outcome. The sweep must find it by PROBING the runner (never
// trusting the row's own state), tear the sandbox down, and re-run the revoke
// cascade. A non-terminal run and a terminal run with no SandboxRef are both
// left untouched by the sweep.
func TestSweepTerminalSandboxes_TearsDownOrphanedLiveSandbox(t *testing.T) {
	h := newHarness(t)
	orphan := sweepRun(types.RunCompleted, "sbx-orphan")
	noSandbox := sweepRun(types.RunFailed, "")             // nothing to probe
	stillRunning := sweepRun(types.RunRunning, "sbx-live") // non-terminal: not the sweep's job
	fake := &sweepStore{runs: []types.AgentRun{orphan, noSandbox, stillRunning}}
	rr := &sweepAliveRunner{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = rr
	cfg.Broker = h.broker
	srv := New(cfg)

	swept, err := srv.SweepTerminalSandboxes(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 1 {
		t.Errorf("swept = %d, want 1 (only the orphan is terminal with a live sandbox)", swept)
	}
	if len(rr.stopped) != 1 || rr.stopped[0] != "sbx-orphan" {
		t.Errorf("StopSandbox calls = %v, want exactly [sbx-orphan]", rr.stopped)
	}
	revoked := false
	for _, id := range h.broker.revoked {
		if id == orphan.ID {
			revoked = true
		}
	}
	if !revoked {
		t.Errorf("sweep must re-run the credential revoke cascade for the orphan %s; broker.revoked=%v", orphan.ID, h.broker.revoked)
	}
}

// TestSweepTerminalSandboxes_LeavesSettledSandboxesAlone is the counterfactual:
// a terminal run whose sandbox the runner reports as ALREADY gone (the normal,
// successful-teardown case) must not be re-torn-down or re-revoked.
func TestSweepTerminalSandboxes_LeavesSettledSandboxesAlone(t *testing.T) {
	h := newHarness(t)
	settled := sweepRun(types.RunCompleted, "sbx-settled")
	fake := &sweepStore{runs: []types.AgentRun{settled}}
	rr := &sweepGoneRunner{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(h, fake)
	cfg.Runner = rr
	cfg.Broker = h.broker
	srv := New(cfg)

	swept, err := srv.SweepTerminalSandboxes(context.Background())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept != 0 {
		t.Errorf("swept = %d, want 0 (the sandbox is already gone)", swept)
	}
	if len(rr.stopped) != 0 {
		t.Errorf("must not call StopSandbox on an already-gone sandbox; stopped=%v", rr.stopped)
	}
	if len(h.broker.revoked) != 0 {
		t.Errorf("must not re-revoke a run whose sandbox already settled; broker.revoked=%v", h.broker.revoked)
	}
}
