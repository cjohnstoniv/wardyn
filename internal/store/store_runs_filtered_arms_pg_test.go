// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// ListRunsFiltered's reads of a member's sponsored runs and of the runs that can need attention. Guarded by
// WARDYN_TEST_PG, like every store_*_pg_test.go file.
package store_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A member with a few runs among a long history, listed with their sponsored service runs, is found by
// agent_runs_created_by_idx on every read the list makes, not by walking the table and testing each row against
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
	if n := strings.Count(plan, "agent_runs_created_by_idx"); n < 1 {
		t.Errorf("the member list does not use agent_runs_created_by_idx (%d uses):\n%s", n, plan)
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
