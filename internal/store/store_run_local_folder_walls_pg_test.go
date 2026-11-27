// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for agent_runs.local_folder_walls: a run's walls for folders from a person's computer,
// kept on its row where audit retention cannot drop them. Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRunLocalFolderWalls_RoundTrip_PG: a run with no record reads the zero record (no walls), a written record
// reads back whole with its per-wall layers, and a run the table does not hold is ErrNotFound both ways.
func TestRunLocalFolderWalls_RoundTrip_PG(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunPending)
	persistRun(t, ctx, pool, r)

	got, err := pg.RunLocalFolderWalls(ctx, r.ID)
	if err != nil || !reflect.DeepEqual(got, types.LocalFolderWallsRecord{}) {
		t.Fatalf("before any write: %+v, %v; want the zero record", got, err)
	}
	rec := types.LocalFolderWallsRecord{
		Walls: &types.LocalFolderRules{DenyPull: true, MaxPushMiB: 10, Deny: []string{"*.pem", "a,b"}},
		Layers: map[string]types.LocalFolderWallLayer{
			"deny_pull":    {Layer: "boundary"},
			"max_push_mib": {Layer: "unit", Name: "eng", Locked: true},
		},
	}
	if err := pg.KeepRunLocalFolderWalls(ctx, r.ID, rec); err != nil {
		t.Fatalf("keep: %v", err)
	}
	if got, err = pg.RunLocalFolderWalls(ctx, r.ID); err != nil || !reflect.DeepEqual(got, rec) {
		t.Errorf("after the write: %+v, %v; want %+v", got, err, rec)
	}
	if err := pg.KeepRunLocalFolderWalls(ctx, uuid.New(), rec); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("keep for an absent run = %v, want ErrNotFound", err)
	}
	if _, err := pg.RunLocalFolderWalls(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("read of an absent run = %v, want ErrNotFound", err)
	}
}

// TestRunLocalFolderWalls_FirstWriteWins_PG: the record is written once at dispatch; a second write, even a
// looser one, leaves the first as it was.
func TestRunLocalFolderWalls_FirstWriteWins_PG(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	r := newRun(types.RunPending)
	persistRun(t, ctx, pool, r)

	first := types.LocalFolderWallsRecord{Walls: &types.LocalFolderRules{DenyRemote: true}, Layers: map[string]types.LocalFolderWallLayer{"deny_remote": {Layer: "boundary"}}}
	if err := pg.KeepRunLocalFolderWalls(ctx, r.ID, first); err != nil {
		t.Fatalf("first keep: %v", err)
	}
	if err := pg.KeepRunLocalFolderWalls(ctx, r.ID, types.LocalFolderWallsRecord{Walls: &types.LocalFolderRules{}}); err != nil {
		t.Fatalf("second keep: %v", err)
	}
	if got, err := pg.RunLocalFolderWalls(ctx, r.ID); err != nil || !reflect.DeepEqual(got, first) {
		t.Errorf("after a second write: %+v, %v; want the first %+v", got, err, first)
	}
}
