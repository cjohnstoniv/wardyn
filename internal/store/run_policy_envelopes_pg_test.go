// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runPolicyEnvelopeRowsOf is the effective-policy reader's row read for one run (runPolicyRows): the newest 5
// rows under the first of actions that has any, whose data contains dataContains, oldest first.
func runPolicyEnvelopeRowsOf(t *testing.T, ctx context.Context, pg store.PG, run uuid.UUID, actions []string, dataContains string) []types.AuditEvent {
	t.Helper()
	for _, action := range actions {
		rows, err := pg.QueryAuditEventsFilteredPage(ctx, &run, store.AuditFilter{Action: action, DataContains: dataContains},
			store.Page{Limit: 5, Newest: true})
		if err != nil {
			t.Fatalf("filtered read: %v", err)
		}
		if len(rows) > 0 {
			slices.Reverse(rows)
			return rows
		}
	}
	return nil
}

// runPolicyEnvelopeSeed appends a policy row for run.
func runPolicyEnvelopeSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, run uuid.UUID, action string, data any) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	auditWallsSeed(t, ctx, pool, &run, action, raw)
}

// runPolicyEnvelopeIDs is the ids of rows, in order.
func runPolicyEnvelopeIDs(rows []types.AuditEvent) []uuid.UUID {
	out := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// For a generation-1 run, a generation-3 run and a run with no resolve row, the batch read gives each run what
// the effective-policy reader reads for it on its own: RunPolicyState, its newest resolve rows, and, for the
// generation-3 run, generation 3's newest rows. A run the table does not hold is absent.
func TestRunPolicyEnvelopes_PG(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	resolve := []string{"run.policy.resolve"}

	gen1 := auditWallsRun(t, ctx, pool, types.RunRunning, nil)
	for i := range 7 {
		runPolicyEnvelopeSeed(t, ctx, pool, gen1, "run.policy.resolve", map[string]any{"allowed_domains": []string{fmt.Sprintf("h%d.example", i)}})
	}
	runPolicyEnvelopeSeed(t, ctx, pool, gen1, "run.create", map[string]any{"agent": "shell"})

	gen3 := auditWallsRun(t, ctx, pool, types.RunRunning, nil)
	runPolicyEnvelopeSeed(t, ctx, pool, gen3, "run.policy.resolve", map[string]any{"allowed_domains": []string{"a.example"}})
	gens, err := json.Marshal([]types.PolicyGeneration{{Generation: 1, Digest: "d1"}, {Generation: 2, Digest: "d2"}, {Generation: 3, Digest: "d3"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET policy_generations = $2::jsonb WHERE id = $1`, gen3, string(gens)); err != nil {
		t.Fatalf("set generations: %v", err)
	}
	runPolicyEnvelopeSeed(t, ctx, pool, gen3, "run.policy.generation", map[string]any{"generation": 2, "policy": map[string]any{"allow_all_egress": true}})
	for range 6 {
		runPolicyEnvelopeSeed(t, ctx, pool, gen3, "run.policy.generation", map[string]any{"generation": 3, "policy": map[string]any{"allow_all_egress": false}})
	}

	bare := auditWallsRun(t, ctx, pool, types.RunRunning, nil)
	absent := uuid.New()

	got, err := pg.RunPolicyEnvelopes(ctx, []uuid.UUID{gen1, gen3, bare, absent}, resolve)
	if err != nil {
		t.Fatalf("envelopes: %v", err)
	}
	if _, ok := got[absent]; ok {
		t.Error("a run the table does not hold has an envelope")
	}
	for _, run := range []uuid.UUID{gen1, gen3, bare} {
		env, ok := got[run]
		if !ok {
			t.Errorf("run %s has no envelope", run)
			continue
		}
		state, found, err := pg.RunPolicyState(ctx, run)
		if err != nil || !found {
			t.Fatalf("policy state of %s: found %v, %v", run, found, err)
		}
		if !reflect.DeepEqual(env.State, state) {
			t.Errorf("run %s state %+v, want RunPolicyState's %+v", run, env.State, state)
		}
		if w := runPolicyEnvelopeIDs(runPolicyEnvelopeRowsOf(t, ctx, pg, run, resolve, "")); !slices.Equal(runPolicyEnvelopeIDs(env.Resolve), w) {
			t.Errorf("run %s resolve rows %v, want the reader's %v", run, runPolicyEnvelopeIDs(env.Resolve), w)
		}
		var wantGen []uuid.UUID
		if n := state.CurrentGeneration(); n >= 2 {
			wantGen = runPolicyEnvelopeIDs(runPolicyEnvelopeRowsOf(t, ctx, pg, run, []string{"run.policy.generation"}, fmt.Sprintf(`{"generation":%d}`, n)))
		}
		if gotGen := runPolicyEnvelopeIDs(env.Generation); !slices.Equal(gotGen, wantGen) && (len(gotGen) > 0 || len(wantGen) > 0) {
			t.Errorf("run %s generation rows %v, want the reader's %v", run, gotGen, wantGen)
		}
	}
	if n := len(got[gen1].Resolve); n != 5 {
		t.Errorf("generation-1 run has %d resolve rows, want the newest 5", n)
	}
	if n := len(got[gen3].Generation); n != 5 {
		t.Errorf("generation-3 run has %d generation rows, want generation 3's newest 5", n)
	}
	if got[gen1].Generation != nil || got[bare].Generation != nil || len(got[bare].Resolve) != 0 {
		t.Error("a run below generation 2 has generation rows, or the run with no resolve row has one")
	}

	t.Run("explain", func(t *testing.T) {
		tr := &auditWallsTracer{}
		traced := store.NewPG(auditWallsTraced(t, ctx, pool, tr))
		if _, err := traced.RunPolicyEnvelopes(ctx, []uuid.UUID{gen1, gen3, bare}, resolve); err != nil {
			t.Fatalf("envelopes: %v", err)
		}
		auditWallsExplain(t, ctx, pool, tr, "RunPolicyEnvelopes")
	})
}
