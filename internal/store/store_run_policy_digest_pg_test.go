// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for agent_runs.policy_digest: the SHA-256 of a run's effective policy, written
// once at dispatch. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRunPolicyDigest_SetOnce: a run starts with none, the first write sticks and reads back through the
// run and through RunPolicyDigest, the same value again is a no-op, and a different one is refused and
// changes nothing.
func TestRunPolicyDigest_SetOnce(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunPending)
	persistRun(t, ctx, pool, r)

	if d, found, err := store.RunPolicyDigest(ctx, pool, r.ID); err != nil || !found || d != "" {
		t.Fatalf("before any write: %q, %v, %v; want empty digest of a found run", d, found, err)
	}
	if err := pg.SetRunPolicyDigest(ctx, r.ID, "d1"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := pg.SetRunPolicyDigest(ctx, r.ID, "d1"); err != nil {
		t.Errorf("the same digest again = %v, want nil", err)
	}
	if err := pg.SetRunPolicyDigest(ctx, r.ID, "d2"); !errors.Is(err, store.ErrPolicyDigestSet) {
		t.Errorf("a different digest = %v, want ErrPolicyDigestSet", err)
	}
	if d, _, _ := store.RunPolicyDigest(ctx, pool, r.ID); d != "d1" {
		t.Errorf("stored digest = %q, want d1 untouched", d)
	}
	got, err := pg.GetRun(ctx, r.ID)
	if err != nil || got.PolicyDigest != "d1" {
		t.Errorf("GetRun digest = %q, %v; want d1", got.PolicyDigest, err)
	}
	if err := pg.SetRunPolicyDigest(ctx, uuid.New(), "d1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an absent run = %v, want ErrNotFound", err)
	}
	if _, found, err := store.RunPolicyDigest(ctx, pool, uuid.New()); err != nil || found {
		t.Errorf("an absent run: found %v, err %v; want found false", found, err)
	}
}
