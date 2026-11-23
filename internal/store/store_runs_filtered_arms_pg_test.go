// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// ListRunsFiltered's reads of a member's sponsored runs and of the runs that can need attention. Guarded by
// WARDYN_TEST_PG, like every store_*_pg_test.go file.
package store_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A member with a few runs among a long history, listed with their sponsored service runs, is found by
// agent_runs_created_by_idx on each arm the list reads, not by walking the table and testing each row against
// a hashed sub-plan (the OR form).
func TestPG_ListRunsFiltered_SponsoredMemberUsesTheCreatorIndex(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	seedMemberRuns(t, ctx, pool)
	// History from other creators, ended a minute after they began, so a plan that walks a time order has many
	// rows to discard.
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_runs (id, created_at, ended_at, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
		SELECT gen_random_uuid(), now() - interval '1 day' - g * interval '1 second', now() - interval '1 day' - g * interval '1 second' + interval '1 minute',
		       'history-' || (g % 3000), 'claude-code', 'o/r', 'CC2', 'COMPLETED', 'spiffe://x/' || g, 'docker'
		FROM generate_series(1, 150000) g`); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE agent_runs`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	plan := planOfCall(t, ctx, pool, func(pg store.PG) {
		if _, err := pg.ListRunsFiltered(ctx, store.RunFilter{Owner: "lonely@corp.example", WithSponsored: true}, store.Page{Limit: 50}); err != nil {
			t.Fatalf("list: %v", err)
		}
	})
	t.Logf("plan for a member with few runs:\n%s", plan)
	for _, bad := range []string{"hashed SubPlan", "Seq Scan on agent_runs", "agent_runs_end_time_idx", "agent_runs_created_at_idx"} {
		if strings.Contains(plan, bad) {
			t.Errorf("the member list's plan contains %q:\n%s", bad, plan)
		}
	}
	// The page reads two arms, live and ended, and each one reads through the creator index.
	if n := strings.Count(plan, "agent_runs_created_by_idx"); n < 2 {
		t.Errorf("the member list's arms do not each use agent_runs_created_by_idx (%d uses):\n%s", n, plan)
	}
}

// The attention candidate clause keeps every live run that can need attention and drops the rest: a lost run, a
// waiting run and a run with a PENDING approval stay; a plain running run and one whose approval is decided go.
// A lease-ended run is not live, so it is out of the "active" read either way.
func TestPG_ListRunsFiltered_AttentionCandidatesKeepEveryRunThatCanNeedAttention(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	owner := "attention-" + uuid.NewString()

	plain := persistRun(t, ctx, pool, newFilterRun(owner, types.RunRunning))
	lost := persistRun(t, ctx, pool, newFilterRun(owner, types.RunRunning))
	stampEnded(t, ctx, pool, lost.ID, types.RunRunning, nil, timePtr(time.Now().UTC()), types.LostReboot)
	leaseEnded := persistRun(t, ctx, pool, newFilterRun(owner, types.RunRunning))
	stampEnded(t, ctx, pool, leaseEnded.ID, types.RunRunning, nil, timePtr(time.Now().UTC()), types.LostEnded)
	waiting := persistRun(t, ctx, pool, newFilterRun(owner, types.RunWaiting))
	pending := persistRun(t, ctx, pool, newFilterRun(owner, types.RunRunning))
	if _, err := pg.CreateApproval(ctx, newApproval(pending.ID, time.Now().UTC())); err != nil {
		t.Fatalf("pending approval: %v", err)
	}
	decided := persistRun(t, ctx, pool, newFilterRun(owner, types.RunRunning))
	done := newApproval(decided.ID, time.Now().UTC())
	done.State = types.ApprovalApproved
	if _, err := pg.CreateApproval(ctx, done); err != nil {
		t.Fatalf("approved approval: %v", err)
	}

	ids := func(f store.RunFilter) []uuid.UUID {
		t.Helper()
		got, err := pg.ListRunsFiltered(ctx, f, store.Page{})
		if err != nil {
			t.Fatalf("list %+v: %v", f, err)
		}
		out := idsOfRuns(got)
		slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		return out
	}
	sorted := func(rs ...types.AgentRun) []uuid.UUID {
		out := idsOfRuns(rs)
		slices.SortFunc(out, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
		return out
	}

	got := ids(store.RunFilter{Owner: owner, Statuses: []string{"active"}, AttentionCandidates: true})
	if want := sorted(lost, waiting, pending); !slices.Equal(got, want) {
		t.Errorf("candidates = %v, want lost, waiting and pending %v", got, want)
	}
	got = ids(store.RunFilter{Owner: owner, Statuses: []string{"active"}})
	if want := sorted(plain, lost, waiting, pending, decided); !slices.Equal(got, want) {
		t.Errorf("without the flag = %v, want all five live runs %v", got, want)
	}
}

// seedArmsRuns writes the runs the arms test compares: the member identities' runs of seedMemberRuns plus
// 84 runs over the same creators in every shape the two arms tell apart. Ended runs end in the reverse of
// their creation order, some at one shared time, so an arm ordered by creation time returns another page.
func seedArmsRuns(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (unitA, unitB, portalP, portalQ uuid.UUID) {
	t.Helper()
	seedMemberRuns(t, ctx, pool)
	unitA, unitB, portalP, portalQ = uuid.New(), uuid.New(), uuid.New(), uuid.New()
	creators := []string{"sponsor@corp.example", types.ServicePrincipalPrefix + "deploy-bot", "other@corp.example",
		types.ServicePrincipalPrefix + "nightly-bot", "lonely@corp.example", types.ServicePrincipalPrefix + "self-bot",
		types.ServicePrincipalPrefix + "other-bot"}
	now := time.Now().UTC().Truncate(time.Second)
	tie := now.Add(-2 * time.Hour)
	for i := range 84 {
		run := newFilterRun(creators[i%len(creators)], types.RunRunning)
		run.CreatedAt = now.Add(-time.Duration(i+1) * 37 * time.Second)
		run.Repo = fmt.Sprintf("acme/repo-%d", i%2)
		if i%5 == 0 {
			run.Title = "needle " + run.Title
		}
		switch i % 3 {
		case 0:
			run.UnitID = &unitA
		case 1:
			run.UnitID = &unitB
		}
		if i%11 == 3 {
			run.Service = &types.RunService{Restart: types.ServiceRestartOnFailure, Port: 8080}
		}
		persistRun(t, ctx, pool, run)
		// The i-th run is created i*37 s before the first, and ends later the older it is.
		end := now.Add(-3*time.Hour + time.Duration(i)*time.Minute)
		switch i % 12 {
		case 0, 1: // live
		case 2:
			stampEnded(t, ctx, pool, run.ID, types.RunRunning, nil, timePtr(now.Add(-5*time.Minute)), types.LostReboot)
		case 3:
			stampEnded(t, ctx, pool, run.ID, types.RunRunning, nil, timePtr(end), types.LostEnded)
		case 4:
			stampEnded(t, ctx, pool, run.ID, types.RunCompleted, timePtr(end), nil, "")
		case 5:
			stampEnded(t, ctx, pool, run.ID, types.RunCompleted, timePtr(tie), nil, "")
		case 6:
			stampEnded(t, ctx, pool, run.ID, types.RunFailed, timePtr(end), nil, "")
		case 7:
			stampEnded(t, ctx, pool, run.ID, types.RunKilled, timePtr(now.Add(-time.Hour)), nil, "")
		case 8:
			stampEnded(t, ctx, pool, run.ID, types.RunKilled, timePtr(now.Add(-30*time.Hour)), nil, "")
		case 9:
			stampEnded(t, ctx, pool, run.ID, types.RunCompleted, nil, nil, "")
		case 10:
			stampEnded(t, ctx, pool, run.ID, types.RunCompleted, timePtr(now.Add(-40*time.Hour)), nil, "")
		case 11:
			stampEnded(t, ctx, pool, run.ID, types.RunFailed, timePtr(tie), nil, "")
		}
		if i%9 == 0 {
			via := portalP
			if i%18 == 0 {
				via = portalQ
			}
			if _, err := pool.Exec(ctx, `UPDATE agent_runs SET created_via = $2, portal_drives = true WHERE id = $1`, run.ID, via); err != nil {
				t.Fatalf("mark a portal run: %v", err)
			}
		}
	}
	return unitA, unitB, portalP, portalQ
}

// The two-arm read returns the page the single ordered query of v0.10.2 returned, for every filter the board
// sends and every page, and CountHiddenRuns still counts what that query hid. The frozen query is in
// store_runs_filtered_export_test.go.
func TestPG_ListRunsFiltered_ArmsMatchTheSingleQuery(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	unitA, unitB, portalP, _ := seedArmsRuns(t, ctx, pool)

	const sponsor, lonely, other = "sponsor@corp.example", "lonely@corp.example", "other@corp.example"
	self := types.ServicePrincipalPrefix + "self-bot"
	day := 24 * time.Hour
	var filters []store.RunFilter
	add := func(fs ...store.RunFilter) { filters = append(filters, fs...) }
	add(store.RunFilter{}, store.RunFilter{Owner: sponsor}, store.RunFilter{Owner: sponsor, WithSponsored: true})
	for _, st := range []string{"active", "ended", "failed", "killed"} {
		add(store.RunFilter{Statuses: []string{st}})
	}
	add(store.RunFilter{Statuses: []string{"active", "killed"}}, store.RunFilter{Statuses: []string{"unknown"}},
		store.RunFilter{EndedWithin: day}, store.RunFilter{EndedWithin: day, IncludeKilled: true},
		store.RunFilter{IncludeKilled: true}, store.RunFilter{NoEnd: true}, store.RunFilter{ServiceOnly: true},
		store.RunFilter{Workspace: "acme/repo-1"}, store.RunFilter{Query: "needle"})
	for _, who := range []string{sponsor, lonely, other, self, "nobody@corp.example"} {
		add(store.RunFilter{Owner: who, WithSponsored: true},
			store.RunFilter{Owner: who, WithSponsored: true, EndedWithin: day},
			store.RunFilter{Owner: who, WithSponsored: true, Statuses: []string{"ended"}})
	}
	add(store.RunFilter{Scope: &store.RunUnitScope{IDs: []uuid.UUID{unitA}}},
		store.RunFilter{Scope: &store.RunUnitScope{IDs: []uuid.UUID{unitA, store.DefaultUnitID}}},
		store.RunFilter{Scope: &store.RunUnitScope{IDs: []uuid.UUID{unitA}, Portal: portalP}},
		store.RunFilter{Scope: &store.RunUnitScope{IDs: []uuid.UUID{unitB, store.DefaultUnitID}, Portal: portalP}, EndedWithin: day},
		store.RunFilter{Owner: sponsor, WithSponsored: true, Scope: &store.RunUnitScope{IDs: []uuid.UUID{unitA}}})
	pages := []store.Page{{}, {Limit: 3}, {Limit: 3, Offset: 3}, {Limit: 7, Offset: 9}, {Limit: 50}, {Limit: 5, Offset: 55}}

	for i, f := range filters {
		for _, p := range pages {
			want, err := store.ListRunsFilteredV0102(ctx, pool, f, p)
			if err != nil {
				t.Fatalf("v0.10.2 read, filter %d %+v, page %+v: %v", i, f, p, err)
			}
			got, err := pg.ListRunsFiltered(ctx, f, p)
			if err != nil {
				t.Fatalf("filter %d %+v, page %+v: %v", i, f, p, err)
			}
			if gotIDs := idsOfRuns(got); !slices.Equal(gotIDs, want) && (len(gotIDs) != 0 || len(want) != 0) {
				t.Errorf("filter %d %+v, page %+v:\n got  %v\n want %v", i, f, p, gotIDs, want)
			}
		}
		wantOlder, wantKilled, err := store.CountHiddenRunsV0102(ctx, pool, f)
		if err != nil {
			t.Fatalf("v0.10.2 count, filter %d: %v", i, err)
		}
		older, killed, err := pg.CountHiddenRuns(ctx, f)
		if err != nil {
			t.Fatalf("count, filter %d: %v", i, err)
		}
		if older != wantOlder || killed != wantKilled {
			t.Errorf("filter %d %+v: hidden counts = %d, %d, want %d, %d", i, f, older, killed, wantOlder, wantKilled)
		}
	}

	// A member never reads another member's runs, or the service runs another member sponsors.
	got, err := pg.ListRunsFiltered(ctx, store.RunFilter{Owner: other, WithSponsored: true}, store.Page{})
	if err != nil {
		t.Fatalf("other@'s list: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("other@'s list is empty; the seed holds runs of other@ and of other-bot")
	}
	for _, r := range got {
		if r.CreatedBy != other && r.CreatedBy != types.ServicePrincipalPrefix+"other-bot" {
			t.Errorf("other@'s list holds a run of %q", r.CreatedBy)
		}
	}
}

// The ended arm reads agent_runs_end_time_idx for the window and the order, not every row of the table. No planner
// setting is changed: a plan forced by one proves nothing about production.
func TestPG_ListRunsFiltered_EndedArmUsesTheEndTimeIndex(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO agent_runs (id, created_at, ended_at, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
		SELECT gen_random_uuid(), now() - g * interval '17 seconds' - interval '1 minute', now() - g * interval '17 seconds',
		       'history-' || (g % 3000), 'claude-code', 'o/r', 'CC2', 'COMPLETED', 'spiffe://x/' || g, 'docker'
		FROM generate_series(1, 150000) g`); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE agent_runs`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	plan := planOfCall(t, ctx, pool, func(pg store.PG) {
		if _, err := pg.ListRunsFiltered(ctx, store.RunFilter{EndedWithin: 24 * time.Hour}, store.Page{Limit: 50}); err != nil {
			t.Fatalf("list: %v", err)
		}
	})
	t.Logf("plan for the ended arm over 150,000 ended runs:\n%s", plan)
	if !strings.Contains(plan, "agent_runs_end_time_idx") {
		t.Errorf("the ended arm does not use agent_runs_end_time_idx:\n%s", plan)
	}
	if strings.Contains(plan, "Seq Scan on agent_runs") {
		t.Errorf("the ended arm scans the whole table:\n%s", plan)
	}
}
