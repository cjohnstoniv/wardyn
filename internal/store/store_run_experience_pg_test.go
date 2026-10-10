// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for agent_runs.experience (migration 0193): the stored
// run mode every interactive door reads. Guarded by WARDYN_TEST_PG; skipped
// cleanly when unset.
package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunExperience_RoundTrips: each mode survives create, read and list, and
// the stored mode reaches the one predicate the doors call.
func TestPG_RunExperience_RoundTrips(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	for _, tc := range []struct {
		experience     types.RunExperience
		backgroundOnly bool
	}{
		{types.ExperienceBackground, true},
		{types.ExperienceInteractive, false},
	} {
		r := newRun(types.RunRunning)
		r.Experience = tc.experience
		// The legacy flag points the other way on purpose: the door must read the stored mode.
		r.Interactive = tc.experience == types.ExperienceBackground
		created, err := pg.CreateRun(ctx, r)
		if err != nil {
			t.Fatalf("create %s run: %v", tc.experience, err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, r.ID) })
		got, err := pg.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range []types.AgentRun{created, got} {
			if run.Experience != tc.experience || run.BackgroundOnly() != tc.backgroundOnly {
				t.Errorf("%s run read back as experience %q, BackgroundOnly %v", tc.experience, run.Experience, run.BackgroundOnly())
			}
		}
		runs, err := pg.ListRuns(ctx)
		if err != nil {
			t.Fatal(err)
		}
		listed := false
		for _, run := range runs {
			if run.ID == r.ID {
				listed = run.Experience == tc.experience
			}
		}
		if !listed {
			t.Errorf("%s run is missing from the list or lost its experience", tc.experience)
		}
	}
}

// TestPG_RunExperience_LegacyRowReadsEmptyAndIsNotBackgroundOnly: a run from an
// older client or from before 0.9 has no mode and keeps every door's behaviour.
func TestPG_RunExperience_LegacyRowReadsEmptyAndIsNotBackgroundOnly(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	id := uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO agent_runs
		(id, created_at, created_by, agent, repo, task, confinement_class, state, spiffe_id, runner_target)
		VALUES ($1, now(), 'legacy@example.com', 'claude-code', 'acme/r', 't', 'CC1', 'PENDING', 'spiffe://x', 'docker')`, id)
	if err != nil {
		t.Fatalf("insert legacy-shaped row: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, id) })
	got, err := store.NewPG(pool).GetRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Experience != "" || got.BackgroundOnly() {
		t.Fatalf("a legacy row read as experience %q, BackgroundOnly %v", got.Experience, got.BackgroundOnly())
	}
}

// TestPG_RunExperience_ClosedSet: the column holds the two modes and nothing else.
func TestPG_RunExperience_ClosedSet(t *testing.T) {
	pool := runsPGPool(t)
	r := newRun(types.RunPending)
	r.Experience = "batch"
	if _, err := store.NewPG(pool).CreateRun(context.Background(), r); err == nil {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, r.ID)
		t.Fatal("an unknown run mode was stored")
	}
}
