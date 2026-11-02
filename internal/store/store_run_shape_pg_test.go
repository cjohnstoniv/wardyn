// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for agent_runs.shape (migration 0245): the New Run shape
// a run was set up from. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunShape_RoundTrips: each shape, and none, survives create, read and list.
func TestPG_RunShape_RoundTrips(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	for _, shape := range []types.RunShape{"", types.RunShapeAgentTask, types.RunShapeEnvironment, types.RunShapeApplication} {
		r := newRun(types.RunRunning)
		r.Shape = shape
		created := persistRun(t, ctx, pool, r)
		got, err := pg.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, run := range []types.AgentRun{created, got} {
			if run.Shape != shape {
				t.Errorf("a %q run read back as shape %q", shape, run.Shape)
			}
		}
		runs, err := pg.ListRuns(ctx)
		if err != nil {
			t.Fatal(err)
		}
		listed := false
		for _, run := range runs {
			if run.ID == r.ID {
				listed = run.Shape == shape
			}
		}
		if !listed {
			t.Errorf("a %q run is missing from the list or lost its shape", shape)
		}
	}
}

// TestPG_RunShape_ClosedSet: the column holds the three shapes or empty, and nothing else.
func TestPG_RunShape_ClosedSet(t *testing.T) {
	pool := runsPGPool(t)
	r := newRun(types.RunPending)
	r.Shape = "service"
	if _, err := store.NewPG(pool).CreateRun(context.Background(), r); err == nil {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, r.ID)
		t.Fatal("an unknown run shape was stored")
	}
}

// TestPG_RunShape_QueuedRunKeepsItThroughPromotion: a waiting run is written with its shape, and the
// promotion that moves it to PENDING returns and keeps that shape.
func TestPG_RunShape_QueuedRunKeepsItThroughPromotion(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 1)
	now := time.Now().UTC()
	r := poolRun(p, types.RunQueued, "alice")
	r.Shape = types.RunShapeApplication
	queued, err := pg.CreateQueuedRun(ctx, r, store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if queued.Shape != types.RunShapeApplication {
		t.Fatalf("queued run read back as shape %q, want application", queued.Shape)
	}
	promoted, _, err := pg.PromoteRunQueue(ctx, pass(p), admitAll)
	if err != nil || len(promoted) != 1 || promoted[0].Run.ID != r.ID {
		t.Fatalf("promote = %+v, %v; want the one waiting run", promoted, err)
	}
	if promoted[0].Run.Shape != types.RunShapeApplication {
		t.Errorf("promotion returned shape %q, want application", promoted[0].Run.Shape)
	}
	if got := runState(t, pg, r.ID); got.State != types.RunPending || got.Shape != types.RunShapeApplication {
		t.Errorf("promoted run is %s with shape %q, want PENDING with application", got.State, got.Shape)
	}
}
