// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed test for agent_runs.allowed_models (migration 0255). Guarded by WARDYN_TEST_PG;
// skipped cleanly when unset.
package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_RunAllowedModels_RecordAndClear(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	r := newRun(types.RunRunning)
	if _, err := pg.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id = $1`, r.ID) })

	if got, err := pg.RunAllowedModels(ctx, r.ID); got != nil || err != nil {
		t.Fatalf("a run that never dispatched read back %s %v", got, err)
	}
	rec := []byte(`{"models":["model-a","small-model-a"],"profile":{"id":"p1","name":"Contractors"}}`)
	if err := pg.SetRunAllowedModels(ctx, r.ID, rec); err != nil {
		t.Fatal(err)
	}
	got, err := pg.RunAllowedModels(ctx, r.ID)
	if err != nil || string(mustCompact(t, got)) != string(mustCompact(t, rec)) {
		t.Fatalf("read back %s %v, want %s", got, err, rec)
	}
	if err := pg.SetRunAllowedModels(ctx, r.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := pg.RunAllowedModels(ctx, r.ID); got != nil || err != nil {
		t.Fatalf("a cleared record read back %s %v", got, err)
	}
	if _, err := pg.RunAllowedModels(ctx, uuid.New()); err != store.ErrNotFound {
		t.Fatalf("an unknown run read back %v, want ErrNotFound", err)
	}
}

// mustCompact re-marshals raw through a map, so two spellings of one object compare equal.
func mustCompact(t *testing.T, raw []byte) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
