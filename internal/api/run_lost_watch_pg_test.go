// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRunLostPG_ARebootedRunnerRunEndsByItsExitReceipt is #3665's real reboot.
// A runner's host reboots under a headless run: its Docker forgets the exec and
// the container exits 143, so the org's probe and the runner's own observer
// both read {FAILED, exit 143} (runner/docker/driver_exec.go AgentStatus,
// runner/offline/observation.go observeAgent). The watcher leaves the run
// alone, and the runner's exit receipt then ends it FAILED with run.complete.
// A reboot mark from the watcher would get that receipt refused.
func TestRunLostPG_ARebootedRunnerRunEndsByItsExitReceipt(t *testing.T) {
	pool, cfg, rn, run, exit, _ := runnerExitAPIFixture(t)
	ctx := t.Context()
	st := cfg.Store.(store.PG)
	if err := st.BindRunnerExecution(ctx, rn.ID, run.ID, exit.Generation, exit.ExecID); err != nil {
		t.Fatal(err)
	}
	srv := New(cfg)
	defer srv.WaitBackground()
	got, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	code := 143
	if !srv.keepRebootedRun(ctx, got, runner.Status{State: types.RunFailed, ExitCode: &code}) {
		t.Fatal("the watcher went on to finalize a headless runner run")
	}
	if got, _ := st.GetRun(ctx, run.ID); got.LostAt != nil || got.State != types.RunRunning {
		t.Fatalf("state %s, lost %v %q; want RUNNING with no lost mark", got.State, got.LostAt, got.LostReason)
	}
	exit.Code = code
	event := runnerwire.Event{Kind: runnerwire.EventAgentExit, RunID: &run.ID, Data: mustJSON(exit)}
	if err := srv.ExitFromRunner(ctx, rn.ID, event); err != nil {
		t.Fatal(err)
	}
	if err := srv.SweepRunnerExits(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetRun(ctx, run.ID); got.State != types.RunFailed {
		t.Fatalf("state %s after the runner's exit receipt, want FAILED", got.State)
	}
	assertRunnerExitPhase(t, pool, run.ID, true, true, 1)
}
