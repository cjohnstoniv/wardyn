// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

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

// oldRunsByCreatorOrSponsorSQL is ListRunsPageByCreatorOrSponsor as 0.9.3 shipped it, kept to hold the
// rewrite to the same answer.
const oldRunsByCreatorOrSponsorSQL = `SELECT id FROM agent_runs WHERE created_by = $1 OR created_by IN
	(SELECT '` + types.ServicePrincipalPrefix + `' || name FROM service_identities WHERE sponsor = $1) ORDER BY created_at DESC`

// seedMemberRuns writes runs of several creators with distinct created_at, interleaved, and the service
// identities that make some of them a sponsor's.
func seedMemberRuns(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, svc := range []struct{ name, sponsor string }{
		{"deploy-bot", "sponsor@corp.example"}, {"nightly-bot", "sponsor@corp.example"},
		{"other-bot", "other@corp.example"}, {"self-bot", types.ServicePrincipalPrefix + "self-bot"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO service_identities (name, sponsor, created_by) VALUES ($1, $2, 'seed')`, svc.name, svc.sponsor); err != nil {
			t.Fatalf("seed service identity %s: %v", svc.name, err)
		}
	}
	creators := []string{
		"sponsor@corp.example", types.ServicePrincipalPrefix + "deploy-bot", "other@corp.example", types.ServicePrincipalPrefix + "nightly-bot",
		"sponsor@corp.example", types.ServicePrincipalPrefix + "other-bot", "lonely@corp.example", types.ServicePrincipalPrefix + "self-bot",
		types.ServicePrincipalPrefix + "deploy-bot", "other@corp.example", "sponsor@corp.example",
	}
	base := time.Now().UTC().Truncate(time.Second)
	for i := range 44 {
		run := newRun(types.RunCompleted)
		run.CreatedBy = creators[i%len(creators)]
		run.CreatedAt = base.Add(-time.Duration(i) * time.Minute)
		persistRun(t, ctx, pool, run)
	}
}

func idsOfRuns(runs []types.AgentRun) []uuid.UUID {
	out := make([]uuid.UUID, len(runs))
	for i, r := range runs {
		out[i] = r.ID
	}
	return out
}

// Each page of the member list, for each kind of caller, is exactly what the OR query returned, in order:
// a sponsor, a member who sponsors nothing, one who sponsors a bot that is not theirs, a service principal
// that sponsors itself (the arms must not return its rows twice), and a caller with no runs.
func TestPG_ListRunsPageByCreatorOrSponsorMatchesTheOrQuery(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	seedMemberRuns(t, ctx, pool)

	for _, who := range []string{"sponsor@corp.example", "lonely@corp.example", "other@corp.example",
		types.ServicePrincipalPrefix + "self-bot", "nobody@corp.example"} {
		rows, err := pool.Query(ctx, oldRunsByCreatorOrSponsorSQL, who)
		if err != nil {
			t.Fatalf("old query for %s: %v", who, err)
		}
		all, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			t.Fatalf("old query for %s: %v", who, err)
		}
		for _, p := range []store.Page{{}, {Limit: 3}, {Limit: 3, Offset: 3}, {Limit: 4, Offset: 9}, {Limit: 50}, {Limit: 5, Offset: 40}, {Limit: 7, Offset: 100}} {
			want := all[min(p.Offset, len(all)):]
			if p.Limit > 0 && p.Limit < len(want) {
				want = want[:p.Limit]
			}
			got, err := pg.ListRunsPageByCreatorOrSponsor(ctx, who, p)
			if err != nil {
				t.Fatalf("%s %+v: %v", who, p, err)
			}
			if gotIDs := idsOfRuns(got); !slices.Equal(gotIDs, want) && (len(gotIDs) != 0 || len(want) != 0) {
				t.Errorf("%s page %+v: got %v, want %v", who, p, gotIDs, want)
			}
		}
	}
}

// planTracer records the SQL and arguments of the last statement a pool ran.
type planTracer struct {
	sql  string
	args []any
}

func (p *planTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	p.sql, p.args = d.SQL, d.Args
	return ctx
}
func (p *planTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// planOf is the plan Postgres picks for the last statement run through the tracing pool.
func planOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tr *planTracer) string {
	t.Helper()
	rows, err := pool.Query(ctx, "EXPLAIN (COSTS OFF) "+tr.sql, tr.args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	return strings.Join(lines, "\n")
}

// planOfCall is the plan Postgres picks for the last statement call runs through a store on a tracing pool.
func planOfCall(t *testing.T, ctx context.Context, pool *pgxpool.Pool, call func(store.PG)) string {
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
	return planOf(t, ctx, pool, tr)
}

// A member with a few runs among a long history is found by agent_runs_created_by_idx, not by walking every
// run newest-first and testing each against a hashed sub-plan, which is what the OR query did (it read 302k
// rows to return 5 on the synthetic table in the lane's EXPLAIN).
func TestPG_ListRunsPageByCreatorOrSponsorUsesTheCreatorIndex(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	seedMemberRuns(t, ctx, pool)
	// History from other creators, so a plan that walks created_at has many rows to discard.
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_runs (id, created_at, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
		SELECT gen_random_uuid(), now() - interval '1 day' - g * interval '1 second', 'history-' || (g % 3000), 'claude-code', 'o/r', 'CC2', 'COMPLETED', 'spiffe://x/' || g, 'docker'
		FROM generate_series(1, 150000) g`); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE agent_runs`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	cfg := pool.Config().Copy()
	tr := &planTracer{}
	cfg.ConnConfig.Tracer = tr
	traced, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(traced.Close)

	if _, err := store.NewPG(traced).ListRunsPageByCreatorOrSponsor(ctx, "lonely@corp.example", store.Page{Limit: 50}); err != nil {
		t.Fatalf("list: %v", err)
	}
	plan := planOf(t, ctx, pool, tr)
	t.Logf("plan for a member with few runs:\n%s", plan)
	if strings.Contains(plan, "hashed SubPlan") || !strings.Contains(plan, "agent_runs_created_by_idx") {
		t.Errorf("the member list does not use agent_runs_created_by_idx:\n%s", plan)
	}
	if fmt.Sprint(tr.args[0]) != "lonely@corp.example" {
		t.Errorf("explained the wrong statement: %q %v", tr.sql, tr.args)
	}
}
