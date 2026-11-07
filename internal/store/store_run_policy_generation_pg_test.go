// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for agent_runs.policy_generations: each revive's policy generation, with the
// run tokens each was enforced under. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRunPolicyGeneration_AdvanceAndAbandon: a revive starts generation 2 and moves the run's digest;
// an abandoned one puts the digest back and its number is never reused; RunPolicyStateOf reads the
// generations and the live token lineage in one row read.
func TestRunPolicyGeneration_AdvanceAndAbandon(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunRunning)
	persistRun(t, ctx, pool, r)
	if err := pg.SetRunPolicyDigest(ctx, r.ID, "d1"); err != nil {
		t.Fatal(err)
	}
	if gen, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, "d2", []string{"jti-1", ""}, "jti-2"); err != nil || gen != 2 {
		t.Fatalf("advance = %d, %v; want 2", gen, err)
	}
	if _, _, err := pg.RetireRunTokenLineage(ctx, r.ID, "jti-2"); err != nil {
		t.Fatal(err)
	}
	st, found, err := store.RunPolicyStateOf(ctx, pool, r.ID)
	if err != nil || !found {
		t.Fatalf("state: found %v, %v", found, err)
	}
	if st.Digest != "d2" || len(st.Generations) != 2 || st.Stamp(true, "jti-1") != (types.PolicyStamp{Digest: "d1", Generation: 1}) ||
		st.Stamp(true, "jti-2") != (types.PolicyStamp{Digest: "d2", Generation: 2}) {
		t.Errorf("state after advance = %+v", st)
	}
	if got, _ := pg.GetRun(ctx, r.ID); got.PolicyDigest != "d2" {
		t.Errorf("GetRun digest = %q, want the current generation's d2", got.PolicyDigest)
	}
	if err := pg.AbandonRunPolicyGeneration(ctx, r.ID, 2); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := store.RunPolicyStateOf(ctx, pool, r.ID); st.Digest != "d1" || st.Stamp(false, "") != (types.PolicyStamp{Digest: "d1", Generation: 1}) {
		t.Errorf("state after abandon = %+v, want d1 current", st)
	}
	if st, found, err := pg.RunPolicyState(ctx, r.ID); err != nil || !found || st.CurrentGeneration() != 1 {
		t.Errorf("RunPolicyState after abandon = %+v (found %v, err %v), want generation 1 current", st, found, err)
	}
	if gen, err := pg.AdvanceRunPolicyGeneration(ctx, r.ID, "d3", nil, "jti-3"); err != nil || gen != 3 {
		t.Errorf("the next advance = %d, %v; want 3", gen, err)
	}
	if _, err := pg.AdvanceRunPolicyGeneration(ctx, uuid.New(), "d", nil, "j"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an absent run = %v, want ErrNotFound", err)
	}
}
