// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for agent_runs.lifetime_cap_at and lifetime_cap_source
// (migration 0159): the latest end a run's pool and deployment allow, counted from
// launch. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunLifetimeCap_RoundTrips: ends_at and the cap, with the source that bound
// it, survive create, read, list and the filtered list.
func TestPG_RunLifetimeCap_RoundTrips(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	r := newFilterRun("lifetime-"+uuid.NewString(), types.RunRunning)
	end := r.CreatedAt.Add(24 * time.Hour).Truncate(time.Microsecond)
	capAt := r.CreatedAt.Add(14 * 24 * time.Hour).Truncate(time.Microsecond)
	r.EndsAt, r.LifetimeCapAt, r.LifetimeCapSource = &end, &capAt, types.LimitSourceDeployment
	created, err := pg.CreateRun(ctx, r)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, r.ID) })
	got, err := pg.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := pg.ListRunsFiltered(ctx, store.RunFilter{Owner: r.CreatedBy}, store.Page{})
	if err != nil || len(listed) != 1 {
		t.Fatalf("filtered list = %d rows, %v", len(listed), err)
	}
	for name, run := range map[string]types.AgentRun{"create": created, "get": got, "list": listed[0]} {
		if run.EndsAt == nil || !run.EndsAt.Equal(end) || run.LifetimeCapAt == nil || !run.LifetimeCapAt.Equal(capAt) || run.LifetimeCapSource != types.LimitSourceDeployment {
			t.Errorf("%s: ends %v, cap %v (%q); want %v, %v (deployment)", name, run.EndsAt, run.LifetimeCapAt, run.LifetimeCapSource, end, capAt)
		}
	}
}

// TestPG_RunLifetimeCap_LegacyRowReadsAsBefore: a row written before the column
// reads with no cap and no source, and the closed source set holds.
func TestPG_RunLifetimeCap_LegacyRowReadsAsBefore(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO agent_runs
		(id, created_at, created_by, agent, repo, task, confinement_class, state, spiffe_id, runner_target)
		VALUES ($1, now(), 'legacy@example.com', 'claude-code', 'acme/r', 't', 'CC1', 'PENDING', 'spiffe://x', 'docker')`, id); err != nil {
		t.Fatalf("insert legacy-shaped row: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, id) })
	got, err := store.NewPG(pool).GetRun(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.LifetimeCapAt != nil || got.LifetimeCapSource != "" || got.EndsAt != nil {
		t.Errorf("a legacy row read ends %v, cap %v (%q); want none", got.EndsAt, got.LifetimeCapAt, got.LifetimeCapSource)
	}

	bad := newFilterRun("lifetime-bad", types.RunPending)
	bad.LifetimeCapSource = "somebody"
	if _, err := store.NewPG(pool).CreateRun(ctx, bad); err == nil {
		_, _ = pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, bad.ID)
		t.Fatal("an unknown cap source was stored")
	}
}

// TestPG_ListRunsFiltered_NoEndKeepsLiveRunsWithNoEnd: the "No end" filter selects
// live runs with ends_at NULL only: not a run with an end, not a finished one, and not
// a QUEUED run, whose end is empty only until its start writes it.
func TestPG_ListRunsFiltered_NoEndKeepsLiveRunsWithNoEnd(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	owner := "no-end-" + uuid.NewString()

	endsLater := newFilterRun(owner, types.RunRunning)
	at := endsLater.CreatedAt.Add(time.Hour)
	endsLater.EndsAt = &at
	noEnd := newFilterRun(owner, types.RunRunning)
	finished := newFilterRun(owner, types.RunCompleted)
	queued := newFilterRun(owner, types.RunPending)
	for _, r := range []types.AgentRun{endsLater, noEnd, finished, queued} {
		if _, err := pg.CreateRun(ctx, r); err != nil {
			t.Fatalf("create: %v", err)
		}
		id := r.ID
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, id) })
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state = 'QUEUED' WHERE id = $1`, queued.ID); err != nil {
		t.Fatal(err)
	}
	got, err := pg.ListRunsFiltered(ctx, store.RunFilter{Owner: owner, NoEnd: true}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != noEnd.ID {
		t.Errorf("no_end filter returned %d rows %v; want only the live run with no end", len(got), got)
	}
	if all, err := pg.ListRunsFiltered(ctx, store.RunFilter{Owner: owner}, store.Page{}); err != nil || len(all) != 4 {
		t.Errorf("unfiltered = %d rows, %v; want 4", len(all), err)
	}
}
