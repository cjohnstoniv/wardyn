// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run Wardyn launches itself is placed by no pool, so its row says remote at create (newStepRun is the one
// constructor every sign-in, scan and probe goes through) instead of reading as "not recorded".
func TestStepRun_StampsTheRemotePlacement(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, &dispatchTestStore{}))
	gov := stepRunGoverned(governanceCeiling{}, false, false)
	for _, task := range []string{harnessLoginTask, "source scan"} {
		run, _, err := srv.newStepRun(context.Background(), uuid.New(), "owner", task, types.CC2, gov, nil)
		if err != nil {
			t.Fatal(err)
		}
		if run.Placement != types.PlacementRemote || run.RunnerID != nil || run.RunnerPoolID != nil {
			t.Errorf("%s run: placement %q runner %v pool %v, want remote and unbound", task, run.Placement, run.RunnerID, run.RunnerPoolID)
		}
	}
}

// run.create records where the run goes for an unpooled run too, and the names of the pool and runner a
// pooled one is bound to.
func TestRunPlacement_AuditFields(t *testing.T) {
	unpooled := runPlacement{}.auditFields(types.AgentRun{}, "")
	for _, k := range []string{"placement", "resources_applied"} {
		if _, ok := unpooled[k]; !ok {
			t.Errorf("an unpooled run.create lacks %q: %v", k, unpooled)
		}
	}
	for _, k := range []string{"runner_pool_id", "runner_pool_name", "runner_name", "runner_id", "remote_runner_id"} {
		if _, ok := unpooled[k]; ok {
			t.Errorf("an unpooled run.create carries %q: %v", k, unpooled)
		}
	}

	pool := types.ResolvedRunnerPool{ID: uuid.New(), Name: "Build farm"}
	rid := uuid.New()
	run := types.AgentRun{Placement: types.PlacementLocal, RunnerID: &rid}
	got := runPlacement{pooled: true, pool: pool}.auditFields(run, "laptop")
	if got["placement"] != types.PlacementLocal || got["runner_pool_name"] != "Build farm" || got["runner_name"] != "laptop" || got["runner_pool_id"] != pool.ID {
		t.Errorf("a pooled run.create = %v", got)
	}
	_, named := runPlacement{pooled: true, pool: pool}.auditFields(run, "")["runner_name"]
	if named {
		t.Error("an unknown runner name is recorded as empty, not omitted")
	}
}

// A provider sign-in holds the person's sign-in credentials and runs on the server's own substrate: one that
// carries a runner is refused before any sandbox is created, while any other run bound to a runner still goes.
func TestCreateSandboxAtTarget_RefusesASignInBoundToARunner(t *testing.T) {
	h := newHarness(t)
	rn := &fakeRunner{}
	cfg := baseTestConfig(h, &dispatchTestStore{})
	cfg.Runner = rn
	srv := New(cfg)
	rid := uuid.New()
	_, err := srv.createSandboxAtTarget(context.Background(), types.AgentRun{ID: uuid.New(), Task: harnessLoginTask, RunnerID: &rid}, runner.SandboxSpec{})
	if !errors.Is(err, runner.ErrExecutorUnavailable) || rn.createCalls != 0 {
		t.Fatalf("a sign-in bound to a runner: err %v, %d sandboxes created; want the refusal and none", err, rn.createCalls)
	}
	ordinary := types.AgentRun{ID: uuid.New(), Task: "fix the tests", RunnerID: &rid}
	if _, err := srv.createSandboxAtTarget(context.Background(), ordinary, runner.SandboxSpec{}); err != nil || rn.createCalls != 1 {
		t.Fatalf("an ordinary run bound to a runner: err %v, %d sandboxes created; want it to go on", err, rn.createCalls)
	}
}
