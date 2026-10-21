// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed test for agent_runs.model_allowlists (migration 0200). Guarded by
// WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_RunModelAllowlists_RecordReplaceAndClear(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	r := newRun(types.RunRunning)
	if _, err := pg.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id = $1`, r.ID) })

	if got, err := pg.RunModelAllowlists(ctx, r.ID); got != nil || err != nil {
		t.Fatalf("a run that never dispatched read back %v %v", got, err)
	}
	if err := pg.SetRunModelAllowlists(ctx, r.ID, map[string][]string{"laptop-gpu": {"qwen2.5-coder:7b", "llama3.2:3b"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := pg.RunModelAllowlists(ctx, r.ID); !slices.Equal(got["laptop-gpu"], []string{"qwen2.5-coder:7b", "llama3.2:3b"}) {
		t.Fatalf("read back %v, want the recorded list", got)
	}
	if err := pg.SetRunModelAllowlists(ctx, r.ID, map[string][]string{"laptop-gpu": {"llama3.2:3b"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := pg.RunModelAllowlists(ctx, r.ID); !slices.Equal(got["laptop-gpu"], []string{"llama3.2:3b"}) || len(got) != 1 {
		t.Fatalf("read back %v, want the later dispatch's list alone", got)
	}
	if err := pg.SetRunModelAllowlists(ctx, r.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := pg.RunModelAllowlists(ctx, r.ID); got != nil || err != nil {
		t.Fatalf("a cleared record read back %v %v", got, err)
	}
	if _, err := pg.RunModelAllowlists(ctx, uuid.New()); err != store.ErrNotFound {
		t.Fatalf("an unknown run read back %v, want ErrNotFound", err)
	}
}
