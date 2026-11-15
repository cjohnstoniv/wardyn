// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for agent_runs.lifetime_cap_at and lifetime_cap_source
// (migration 0159): the latest end a run's pool and deployment allow, counted from
// launch. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"fmt"
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
		// The store re-anchors the end and the cap on the database's clock, keeping their spans from created_at
		// (stampRunTimes); each column rounds to a microsecond on its own, hence the tolerance.
		if !spanFrom(run.CreatedAt, run.EndsAt, end.Sub(r.CreatedAt)) || !spanFrom(run.CreatedAt, run.LifetimeCapAt, capAt.Sub(r.CreatedAt)) ||
			run.LifetimeCapSource != types.LimitSourceDeployment {
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

// TestPG_ListRunsFiltered_NoEndLeavesOutLeaseEndedRuns: a RUNNING run whose lease ended (lost_reason
// 'ended') is not live, so "No end" does not list it, as the live-runs count does not.
func TestPG_ListRunsFiltered_NoEndLeavesOutLeaseEndedRuns(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	owner := "no-end-ended-" + uuid.NewString()

	live := newFilterRun(owner, types.RunRunning)
	ended := newFilterRun(owner, types.RunRunning)
	for _, r := range []types.AgentRun{live, ended} {
		if _, err := pg.CreateRun(ctx, r); err != nil {
			t.Fatalf("create: %v", err)
		}
		id := r.ID
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, id) })
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_reason = 'ended' WHERE id = $1`, ended.ID); err != nil {
		t.Fatal(err)
	}
	got, err := pg.ListRunsFiltered(ctx, store.RunFilter{Owner: owner, NoEnd: true}, store.Page{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != live.ID {
		t.Errorf("no_end filter returned %d rows %v; want only the run whose lease has not ended", len(got), got)
	}
}

// spanFrom reports whether at is span after from, to within the two microseconds two columns' own rounding allows.
func spanFrom(from time.Time, at *time.Time, span time.Duration) bool {
	return at != nil && (at.Sub(from)-span).Abs() <= 2*time.Microsecond
}

// A new run's created_at, ends_at and lifetime_cap_at are on the database's clock whatever the replica's: with
// store.PG's app clock 2 minutes ahead of Postgres and 2 minutes behind it, created_at lands inside the database's
// own readings around the create (less 30 s for the app reading taken before them), and the end and the cap sit at
// their spans from it. A cap an elevation set is the elevation's end, kept as given; an end inside it keeps its
// span, and an end the api cut to it is stored as the cap exactly. Before, all three were the app clock's, 2
// minutes off every reader's now(); without the cut rule, the cut end at +2 minutes landed 2 minutes short of the
// cap.
func TestPG_CreateRun_StampsAreTheDatabasesClock(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	for _, skew := range []time.Duration{2 * time.Minute, -2 * time.Minute} {
		pg := store.NewPG(pool)
		pg.Now = func() time.Time { return time.Now().Add(skew) }
		for _, c := range []struct {
			name     string
			elevated bool
			endIn    time.Duration // the asked end from CreatedAt; 0 is the end cut to the cap
		}{
			{"deployment cap", false, time.Hour},
			{"elevation cap, end inside it", true, 10 * time.Minute},
			{"elevation cap, end cut to it", true, 0},
		} {
			t.Run(fmt.Sprintf("%s, app clock %v", c.name, skew), func(t *testing.T) {
				r := newFilterRun("stamp-"+uuid.NewString(), types.RunRunning)
				r.CreatedAt = pg.Now().UTC()
				end, capAt, source := r.CreatedAt.Add(c.endIn), r.CreatedAt.Add(2*time.Hour), types.LimitSourceDeployment
				if c.elevated {
					capAt, source = time.Now().UTC().Add(30*time.Minute).Truncate(time.Microsecond), types.LimitSourceElevation
				}
				if c.endIn == 0 {
					end = capAt
				}
				r.EndsAt, r.LifetimeCapAt, r.LifetimeCapSource = &end, &capAt, source
				before, err := pg.DatabaseNow(ctx)
				if err != nil {
					t.Fatal(err)
				}
				created, err := pg.CreateRun(ctx, r)
				if err != nil {
					t.Fatalf("create: %v", err)
				}
				t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id=$1`, r.ID) })
				after, err := pg.DatabaseNow(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if created.CreatedAt.Before(before.Add(-30*time.Second)) || created.CreatedAt.After(after) {
					t.Fatalf("created_at = %v, want inside the database's [%v, %v]", created.CreatedAt, before, after)
				}
				switch {
				case c.elevated && c.endIn == 0:
					if created.LifetimeCapAt == nil || !created.LifetimeCapAt.Equal(capAt) || created.EndsAt == nil || !created.EndsAt.Equal(capAt) {
						t.Fatalf("cap %v, end %v; want both at the elevation's end %v", created.LifetimeCapAt, created.EndsAt, capAt)
					}
				case c.elevated:
					if created.LifetimeCapAt == nil || !created.LifetimeCapAt.Equal(capAt) || !spanFrom(created.CreatedAt, created.EndsAt, c.endIn) {
						t.Fatalf("cap %v, end %v; want the cap at %v and the end %v after created_at %v", created.LifetimeCapAt, created.EndsAt, capAt, c.endIn, created.CreatedAt)
					}
				default:
					if !spanFrom(created.CreatedAt, created.EndsAt, time.Hour) || !spanFrom(created.CreatedAt, created.LifetimeCapAt, 2*time.Hour) {
						t.Fatalf("end %v, cap %v; want 1 h and 2 h after created_at %v", created.EndsAt, created.LifetimeCapAt, created.CreatedAt)
					}
				}
			})
		}
	}
}
