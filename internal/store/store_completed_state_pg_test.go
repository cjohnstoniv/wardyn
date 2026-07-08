// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres regression test for the COMPLETED run-state cluster. Guarded by
// WARDYN_TEST_PG. Run with: WARDYN_TEST_PG=postgres://... go test ./internal/store/...
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestUpdateRunState_RunningToCompleted_PG is the regression for the top
// critical finding: before migration 0003 the agent_runs.state CHECK omitted
// 'COMPLETED', so this UPDATE failed with a constraint violation, the run never
// reached a terminal state, and the credential-revocation cascade never fired.
// This test asserts the completion transition actually persists end to end.
func TestUpdateRunState_RunningToCompleted_PG(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	now := time.Now().UTC()
	run := types.AgentRun{
		ID:               uuid.New(),
		CreatedAt:        now,
		UpdatedAt:        now,
		CreatedBy:        "tester@example.com",
		Agent:            "claude-code",
		Repo:             "octocat/Hello-World",
		Task:             "regression: completion transition",
		ConfinementClass: types.CC2,
		State:            types.RunRunning,
		SPIFFEID:         "spiffe://wardyn.test/agent-run/" + uuid.NewString(),
		RunnerTarget:     "docker",
	}
	if _, err := store.CreateRun(ctx, pool, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, run.ID)
	})

	// The conditional transition the completion watcher performs.
	ok, err := store.UpdateRunStateIf(ctx, pool, run.ID, types.RunRunning, types.RunCompleted)
	if err != nil {
		t.Fatalf("RUNNING->COMPLETED update errored (the 0003 CHECK migration is "+
			"missing or wrong): %v", err)
	}
	if !ok {
		t.Fatal("RUNNING->COMPLETED transition did not apply")
	}

	got, err := store.GetRun(ctx, pool, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.State != types.RunCompleted {
		t.Fatalf("expected state COMPLETED, got %q", got.State)
	}
}
