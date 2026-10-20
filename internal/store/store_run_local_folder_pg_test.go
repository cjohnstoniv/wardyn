// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Postgres-backed tests for agent_runs.local_folder and its seed (migration
// 0195). Guarded by WARDYN_TEST_PG; skipped cleanly when unset.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_RunLocalFolder_SeedIsRecordedOnce(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	r := newRun(types.RunRunning)
	r.LocalFolder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work"}
	if _, err := pg.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	plain := newRun(types.RunRunning)
	if _, err := pg.CreateRun(ctx, plain); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id IN ($1, $2)`, r.ID, plain.ID)
	})

	folder, seed, err := pg.RunLocalFolderSeed(ctx, r.ID)
	if err != nil || folder == nil || *folder != *r.LocalFolder || seed.SeededAt != nil || seed.Error != "" {
		t.Fatalf("read back %+v %+v %v", folder, seed, err)
	}
	if ok, err := pg.SetRunLocalFolderSeed(ctx, r.ID, ""); !ok || err != nil {
		t.Fatalf("first seed: %v %v", ok, err)
	}
	if ok, err := pg.SetRunLocalFolderSeed(ctx, r.ID, "late failure"); ok || err != nil {
		t.Errorf("a second outcome was recorded: %v %v", ok, err)
	}
	if _, seed, _ := pg.RunLocalFolderSeed(ctx, r.ID); seed.SeededAt == nil || seed.Error != "" {
		t.Errorf("seed = %+v, want the first outcome only", seed)
	}

	if folder, _, err := pg.RunLocalFolderSeed(ctx, plain.ID); folder != nil || err != nil {
		t.Errorf("a run with no local folder read back %+v %v", folder, err)
	}
	if ok, _ := pg.SetRunLocalFolderSeed(ctx, plain.ID, ""); ok {
		t.Error("a run with no local folder took a seed")
	}
}

func TestPG_RunLocalFolder_AComputerFolderKeepsItsRunnerAndItsPasses(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	runner := uuid.New()
	r := newRun(types.RunRunning)
	r.LocalFolder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work", RunnerID: &runner, Path: "/Users/alice/code/my-app", Direction: types.ComputerFolderBoth}
	if _, err := pg.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	plain := newRun(types.RunRunning)
	if _, err := pg.CreateRun(ctx, plain); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id IN ($1, $2)`, r.ID, plain.ID)
	})

	folder, _, err := pg.RunLocalFolderSeed(ctx, r.ID)
	if err != nil || folder == nil || folder.RunnerID == nil || *folder.RunnerID != runner || folder.Path != r.LocalFolder.Path || folder.Direction != types.ComputerFolderBoth {
		t.Fatalf("read back %+v %v", folder, err)
	}
	if st, err := pg.RunLocalFolderStatus(ctx, r.ID); err != nil || st.Seed != nil || st.Back != nil {
		t.Fatalf("a fresh run's passes = %+v %v, want none", st, err)
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	want := types.RunLocalFolderStatus{
		Seed: &types.FolderPassRecord{At: at, Files: 2, Bytes: 18, Refused: []types.FolderRefusal{{Path: "link", Reason: "symlink"}}},
		Back: &types.FolderPassRecord{At: at, Files: 1, Bytes: 5, Refused: []types.FolderRefusal{}, Error: ""},
	}
	if ok, err := pg.SetRunLocalFolderStatus(ctx, r.ID, want); !ok || err != nil {
		t.Fatalf("set: %v %v", ok, err)
	}
	got, err := pg.RunLocalFolderStatus(ctx, r.ID)
	if err != nil || got.Seed == nil || got.Back == nil || !got.Seed.At.Equal(at) || got.Seed.Files != 2 || len(got.Seed.Refused) != 1 || got.Back.Files != 1 {
		t.Fatalf("read back %+v %+v %v", got.Seed, got.Back, err)
	}
	if ok, _ := pg.SetRunLocalFolderStatus(ctx, plain.ID, want); ok {
		t.Error("a run with no local folder took a pass record")
	}
}
