// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// A run's audit reads carry a recorded_at bound (runAuditFromSQL) so they skip the partitions from before the run.
// Guarded by WARDYN_TEST_PG, like every store_*_pg_test.go file.
package store_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// seedRunAudit appends n rows for runID, with actions run.a0, run.a1, ..., and returns their ids in seq order.
func seedRunAudit(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runID uuid.UUID, n int) []uuid.UUID {
	t.Helper()
	var ids []uuid.UUID
	for i := 0; i < n; i++ {
		rid := runID
		ev := types.AuditEvent{
			ID:        uuid.New(),
			Time:      time.Now().UTC(),
			RunID:     &rid,
			ActorType: types.ActorSystem,
			Actor:     "wardynd",
			Action:    fmt.Sprintf("run.a%d", i),
			Outcome:   "success",
		}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("seed audit row %d: %v", i, err)
		}
		ids = append(ids, ev.ID)
	}
	return ids
}

// The bound drops nothing: a run, and a run id with no agent_runs row (no bound), each read back every row they
// wrote through every per-run read the bound reaches.
func TestPG_RunAuditReadsKeepEveryRowOfTheRun(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newFilterRun("audit-bound-"+uuid.NewString(), types.RunRunning))
	gone := uuid.New()
	want := map[uuid.UUID][]uuid.UUID{
		run.ID: seedRunAudit(t, ctx, pool, run.ID, 3),
		gone:   seedRunAudit(t, ctx, pool, gone, 2),
	}
	for id, ids := range want {
		id, ids := id, ids
		reads := map[string]func() ([]types.AuditEvent, error){
			"page asc": func() ([]types.AuditEvent, error) { return pg.QueryAuditEventsPage(ctx, id, store.Page{Limit: 10}) },
			"page newest": func() ([]types.AuditEvent, error) {
				return pg.QueryAuditEventsPage(ctx, id, store.Page{Limit: 10, Newest: true})
			},
			"filtered page": func() ([]types.AuditEvent, error) {
				return pg.QueryAuditEventsFilteredPage(ctx, &id, store.AuditFilter{ActionPrefix: "run."}, store.Page{Limit: 10})
			},
			"filtered after": func() ([]types.AuditEvent, error) {
				evs, _, err := pg.QueryAuditEventsFilteredAfter(ctx, &id, store.AuditFilter{}, 0, 10)
				return evs, err
			},
			"query": func() ([]types.AuditEvent, error) { return pg.QueryAuditEvents(ctx, id, 0) },
		}
		for name, read := range reads {
			evs, err := read()
			if err != nil {
				t.Fatalf("%s for %s: %v", name, id, err)
			}
			got := auditIDs(evs)
			wantIDs := slices.Clone(ids)
			if name == "page newest" {
				slices.Reverse(wantIDs)
			}
			if !slices.Equal(got, wantIDs) {
				t.Errorf("%s for %s: got %v, want %v", name, id, got, wantIDs)
			}
		}
	}
}

// analyzedPlanOfCall is planOfCall with EXPLAIN ANALYZE, so a plan line says whether a partition was executed.
func analyzedPlanOfCall(t *testing.T, ctx context.Context, pool *pgxpool.Pool, call func(store.PG)) string {
	t.Helper()
	cfg := pool.Config().Copy()
	tr := &planTracer{}
	cfg.ConnConfig.Tracer = tr
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(traced.Close)
	call(store.NewPG(traced))
	return analyzedPlan(t, ctx, pool, tr.sql, tr.args)
}

func analyzedPlan(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args []any) string {
	t.Helper()
	rows, err := pool.Query(ctx, "EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF, SUMMARY OFF) "+sql, args...)
	if err != nil {
		t.Fatalf("explain analyze: %v", err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("explain analyze: %v", err)
	}
	return strings.Join(lines, "\n")
}

// A run created after the legacy partition's upper bound does not read the legacy partition; the read rc.2 sent does.
func TestPG_RunAuditReadsSkipPartitionsBeforeTheRun(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	run := persistRun(t, ctx, pool, newFilterRun("audit-bound-"+uuid.NewString(), types.RunRunning))
	// audit_events_legacy ends at the migration's cutover, close to now in a fresh database; the twelve months ahead
	// exist. A run created 40 days on bounds its read past the legacy partition.
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET created_at = now() + interval '40 days' WHERE id = $1`, run.ID); err != nil {
		t.Fatalf("move created_at: %v", err)
	}
	bounded := analyzedPlanOfCall(t, ctx, pool, func(pg store.PG) {
		if _, err := pg.QueryAuditEventsPage(ctx, run.ID, store.Page{Limit: 50}); err != nil {
			t.Fatalf("read: %v", err)
		}
	})
	// The read as rc.2 sent it, frozen here.
	const cols = `id, time, run_id, actor_type, actor, action, target, outcome, source_ip, data`
	control := analyzedPlan(t, ctx, pool, `SELECT `+cols+` FROM audit_events WHERE run_id=$1 ORDER BY seq ASC LIMIT 51`, []any{run.ID})
	t.Logf("plan with the bound:\n%s\nplan without it (rc.2):\n%s", bounded, control)

	for _, l := range strings.Split(bounded, "\n") {
		if strings.Contains(l, "audit_events_legacy") && !strings.Contains(l, "(never executed)") {
			t.Errorf("the bounded read executes the legacy partition: %q", l)
		}
	}
	executed := false
	for _, l := range strings.Split(control, "\n") {
		if strings.Contains(l, "audit_events_legacy") && !strings.Contains(l, "(never executed)") {
			executed = true
		}
	}
	if !executed {
		t.Errorf("the control read does not execute the legacy partition, so this test proves nothing:\n%s", control)
	}
}
