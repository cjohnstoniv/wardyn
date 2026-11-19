// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
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

// selftesterRunner is a local runner that also offers runner.Selftester and
// records the argv of each Exec.
type selftesterRunner struct {
	*quickExecRunner
	argvs     [][]string
	selftests int
}

func (r *selftesterRunner) Exec(ctx context.Context, ref string, argv []string) (string, error) {
	r.argvs = append(r.argvs, argv)
	return r.quickExecRunner.Exec(ctx, ref, argv)
}

func (r *selftesterRunner) Selftest(context.Context, string) (int, error) {
	r.selftests++
	return 0, nil
}

// TestStartAgentOrIdle_BYOILocalSelftestUnchanged: a run on the
// organisation's own executors keeps its selftest as an Exec of the selftest
// argv and a Wait, then the task Exec; the runner's selftest call is not used.
func TestStartAgentOrIdle_BYOILocalSelftestUnchanged(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	now := time.Now().UTC()
	run := types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunRunning,
		RunnerTarget: "docker", Task: "do the thing",
	}
	st := &dispatchTestStore{run: run, state: types.RunRunning}
	rn := &selftesterRunner{quickExecRunner: &quickExecRunner{fakeRunner: &fakeRunner{}}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	srv := New(cfg)

	srv.startAgentOrIdle(context.Background(), run, "fake-"+runID.String(), "wardyn-byoi/custom:latest", false, runnerExecution{}, nil)

	want := [][]string{{"/usr/local/bin/agent-run", "--selftest"}, {"/usr/local/bin/agent-run", "do the thing"}}
	if !slices.EqualFunc(rn.argvs, want, slices.Equal) || rn.selftests != 0 {
		t.Fatalf("Exec argvs = %v, selftest calls = %d; want %v and none", rn.argvs, rn.selftests, want)
	}
	if got := st.State(); got == types.RunFailed {
		t.Fatalf("run state = %q, want not FAILED", got)
	}
}

// An interactive run with no task on a runner holds no generation, so its warn-only selftest does not run:
// no call, no failure row, and a row that says it did not run.
func TestStartAgentOrIdle_BYOIInteractiveNoTaskOnRunner_SelftestNotRun(t *testing.T) {
	h := newHarness(t)
	runID, runnerID := uuid.New(), uuid.New()
	now := time.Now().UTC()
	run := types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunRunning,
		RunnerTarget: "docker", RunnerID: &runnerID,
	}
	st := &dispatchTestStore{run: run, state: types.RunRunning}
	rn := &selftesterRunner{quickExecRunner: &quickExecRunner{fakeRunner: &fakeRunner{}}}
	audit := &syncAudit{}
	cfg := baseTestConfig(h, st)
	cfg.Runner, cfg.Audit = rn, audit
	srv := New(cfg)

	srv.startAgentOrIdle(context.Background(), run, "fake-"+runID.String(), "wardyn-byoi/custom:latest", true, runnerExecution{}, nil)

	if rn.selftests != 0 || len(rn.argvs) != 0 {
		t.Fatalf("selftest calls = %d, Exec argvs = %v; want neither", rn.selftests, rn.argvs)
	}
	if audit.has(runID, "run.selftest", "failure") {
		t.Fatal("a selftest that did not run recorded a failure")
	}
	rows := audit.eventsFor(runID, "run.selftest")
	if len(rows) != 1 || rows[0].Outcome != "success" || !strings.Contains(string(rows[0].Data), "not run: interactive") {
		t.Fatalf("run.selftest rows = %+v, want the one saying it did not run", rows)
	}
	if got := st.State(); got != types.RunRunning {
		t.Fatalf("run state = %q, want RUNNING", got)
	}
}

// The skip is only for a warn-only run with no generation: a fail-closed gate never skips its check, and a
// warn-only call under an issued generation still checks.
func TestByoiSelftest_OnRunnerSkipIsWarnOnlyWithoutAGeneration(t *testing.T) {
	h := newHarness(t)
	runID, runnerID := uuid.New(), uuid.New()
	now := time.Now().UTC()
	run := types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: "t@example.com",
		Agent: "claude-code", ConfinementClass: types.CC1, State: types.RunRunning,
		RunnerTarget: "docker", RunnerID: &runnerID,
	}
	issued := runnerExecution{runnerID: runnerID, runID: runID, generation: uuid.New(), operation: runnerwire.MethodExec}
	for _, tc := range []struct {
		name       string
		execution  runnerExecution
		failClosed bool
	}{
		{"fail-closed with no generation", runnerExecution{}, true},
		{"warn-only under an issued generation", issued, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rn := &selftesterRunner{quickExecRunner: &quickExecRunner{fakeRunner: &fakeRunner{}}}
			cfg := baseTestConfig(h, &dispatchTestStore{run: run, state: types.RunRunning})
			cfg.Runner = rn
			New(cfg).byoiSelftest(context.Background(), run, "fake-"+runID.String(), tc.execution, tc.failClosed)
			if rn.selftests != 1 {
				t.Fatalf("selftest calls = %d, want the check to run", rn.selftests)
			}
		})
	}
}
