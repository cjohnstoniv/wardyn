// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// execLessCapsRunner reports CC3 resolving to the exec-less krun substrate
// label (Capabilities().Resolved), and counts StopSandbox calls. Embeds
// fakeRunner (whose Exec counts calls via execCount()).
type execLessCapsRunner struct {
	*fakeRunner
	stopped int
}

func (r *execLessCapsRunner) Capabilities(context.Context) (runner.Capabilities, error) {
	return runner.Capabilities{
		Driver:             "fake",
		ConfinementClasses: []types.ConfinementClass{types.CC1, types.CC2, types.CC3},
		Resolved:           map[types.ConfinementClass]string{types.CC3: "oci/krun"},
		StructuralEgress:   true,
	}, nil
}

func (r *execLessCapsRunner) StopSandbox(context.Context, string) error {
	r.stopped++
	return nil
}

// Wait returns immediately (exit 0) rather than fakeRunner's block-on-
// ctx.Done(): on unfixed code this runner's Exec (via the embedded fakeRunner)
// gets called for the selftest, and a blocking Wait would otherwise stall this
// test for the full 2-minute byoiSelftestTimeout before it can even fail.
func (r *execLessCapsRunner) Wait(context.Context, string) (int, error) { return 0, nil }

var _ runner.Runner = (*execLessCapsRunner)(nil)

// TestStartAgentOrIdle_BYOIOnExecLessRuntime_RefusedWithoutWastingTheSlot is
// the W15-W15f-exec-lane-runtime-4 regression: on base 763beb5, byoiSelftest
// runs unconditionally for a BYOI image — on an exec-less (krun/CC3) runtime
// that consumes the sandbox's ONE process, so the immediately-following task
// Exec is guaranteed to fail against an already-exited container (Exec is
// called TWICE: once for the selftest, once for the task). The fix refuses the
// combination up front: Exec must never be called at all, the run must land
// FAILED, and the sandbox must be torn down exactly once.
func TestStartAgentOrIdle_BYOIOnExecLessRuntime_RefusedWithoutWastingTheSlot(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	now := time.Now().UTC()
	run := types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC3, State: types.RunRunning,
		RunnerTarget: "docker", Task: "do the thing",
	}
	st := &dispatchTestStore{run: run, state: types.RunRunning}
	rn := &execLessCapsRunner{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	srv := New(cfg)

	srv.startAgentOrIdle(context.Background(), run, "fake-"+runID.String(), "wardyn-byoi/custom:latest", false)

	if n := rn.execCount(); n != 0 {
		t.Fatalf("Exec must never be called for a BYOI image on an exec-less runtime (a selftest exec would consume the sandbox's only process, guaranteeing the task exec that follows fails); got %d Exec calls", n)
	}
	if got := st.State(); got != types.RunFailed {
		t.Fatalf("run state = %q, want FAILED (refused up front, not left RUNNING for a doomed exec)", got)
	}
	if rn.stopped != 1 {
		t.Errorf("sandbox must be torn down exactly once, got %d StopSandbox calls", rn.stopped)
	}
	found := false
	for _, ev := range h.audit.events {
		if ev.Action == "run.selftest" && ev.Outcome == "failure" && ev.RunID != nil && *ev.RunID == runID &&
			strings.Contains(string(ev.Data), "exec-less") {
			found = true
		}
	}
	if !found {
		t.Error("expected a run.selftest/failure audit event explaining the exec-less refusal")
	}
}

// quickExecRunner is a minimal Runner whose Exec counts calls and whose Wait
// returns immediately (exit 0) — unlike fakeRunner's Wait, which blocks on
// ctx.Done() and would otherwise stall byoiSelftest for the full 2-minute gate
// timeout in a fast unit test.
type quickExecRunner struct {
	*fakeRunner
	execs int
}

func (r *quickExecRunner) Exec(context.Context, string, []string) (string, error) {
	r.execs++
	return "exec-id", nil
}

func (r *quickExecRunner) Wait(context.Context, string) (int, error) { return 0, nil }

var _ runner.Runner = (*quickExecRunner)(nil)

// TestStartAgentOrIdle_BYOIOnExecCapableRuntime_StillRunsSelftest is the
// counterpart: a BYOI image on an ordinary exec-capable runtime (CC1/runc, or
// CC3/Kata — Resolved carries no "oci/krun" prefix) must still run the
// selftest exec as before; the new guard must not over-trigger on every CC3
// run, only krun's exec-less one.
func TestStartAgentOrIdle_BYOIOnExecCapableRuntime_StillRunsSelftest(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	now := time.Now().UTC()
	run := types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunRunning,
		RunnerTarget: "docker", Task: "do the thing",
	}
	st := &dispatchTestStore{run: run, state: types.RunRunning}
	rn := &quickExecRunner{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	srv := New(cfg)

	srv.startAgentOrIdle(context.Background(), run, "fake-"+runID.String(), "wardyn-byoi/custom:latest", false)

	// The selftest exec (1) plus the task exec (2) both fire on an ordinary
	// (exec-capable) runtime.
	if n := rn.execs; n != 2 {
		t.Fatalf("expected selftest exec + task exec on an exec-capable runtime, got %d Exec calls", n)
	}
}
