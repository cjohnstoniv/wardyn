// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// failingSelftestRunner answers every Wait with a nonzero exit: the BYOI
// selftest fails.
type failingSelftestRunner struct{ *quickExecRunner }

func (r *failingSelftestRunner) Wait(context.Context, string) (int, error) { return 1, nil }

// TestStartAgentOrIdle_BYOIFailingSelftestRefusesTheRun: a nonzero selftest
// exit refuses the run with its reason, and the task is never exec'd.
func TestStartAgentOrIdle_BYOIFailingSelftestRefusesTheRun(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	now := time.Now().UTC()
	run := types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunRunning,
		RunnerTarget: "docker", Task: "do the thing",
	}
	st := &dispatchTestStore{run: run, state: types.RunRunning}
	rn := &failingSelftestRunner{&quickExecRunner{fakeRunner: &fakeRunner{}}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	srv := New(cfg)

	srv.startAgentOrIdle(context.Background(), run, "fake-"+runID.String(), "wardyn-byoi/custom:latest", false, runnerExecution{}, nil)

	if rn.execs != 1 {
		t.Fatalf("Exec calls = %d, want the selftest only", rn.execs)
	}
	if got := st.State(); got != types.RunFailed {
		t.Fatalf("run state = %q, want FAILED", got)
	}
	const want = "the BYOI image failed its agent-run --selftest (missing shell/harness binary, or a nonzero selftest exit)"
	if got := st.FailureHint(); got != want {
		t.Fatalf("failure reason = %q, want %q", got, want)
	}
}
