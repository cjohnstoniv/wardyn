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
	if ok, err := pg.SetRunLocalFolderStatusIf(ctx, r.ID, store.FolderStatusGuard{}, want); !ok || err != nil {
		t.Fatalf("set: %v %v", ok, err)
	}
	got, err := pg.RunLocalFolderStatus(ctx, r.ID)
	if err != nil || got.Seed == nil || got.Back == nil || !got.Seed.At.Equal(at) || got.Seed.Files != 2 || len(got.Seed.Refused) != 1 || got.Back.Files != 1 {
		t.Fatalf("read back %+v %+v %v", got.Seed, got.Back, err)
	}
	if ok, _ := pg.SetRunLocalFolderStatusIf(ctx, plain.ID, store.FolderStatusGuard{}, want); ok {
		t.Error("a run with no local folder took a pass record")
	}
}

// A record's generation is 0 when the column is NULL or its JSON has no gen: the bump moves either to 1 and
// keeps every other field, a write at 0 lands on a gen-less record, and a write at a stale generation does not.
func TestPG_FolderGen_NullAndLegacyRows(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	runner := uuid.New()
	newFolderRun := func() uuid.UUID {
		r := newRun(types.RunRunning)
		r.LocalFolder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work", RunnerID: &runner, Path: "/Users/alice/code/my-app", Direction: types.ComputerFolderBoth}
		if _, err := pg.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id = $1`, r.ID) })
		return r.ID
	}

	null := newFolderRun()
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET local_folder_status = NULL WHERE id = $1`, null); err != nil {
		t.Fatal(err)
	}
	if st, ok, err := pg.BumpRunLocalFolderGen(ctx, null, store.FolderStatusGuard{Gen: 0}); !ok || err != nil || st.Gen != 1 || st.Back != nil {
		t.Fatalf("bump of a NULL record = %+v %v %v, want gen 1 and nothing else", st, ok, err)
	}

	legacy := newFolderRun()
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET local_folder_status = '{"back":{"at":"2026-11-01T00:00:00Z","files":4,"bytes":9,"refused":[]}}'::jsonb WHERE id = $1`, legacy); err != nil {
		t.Fatal(err)
	}
	if st, err := pg.RunLocalFolderStatus(ctx, legacy); err != nil || st.Gen != 0 || st.Back == nil {
		t.Fatalf("a gen-less record reads %+v %v, want gen 0 with its back", st, err)
	}
	if ok, err := pg.SetRunLocalFolderStatusIf(ctx, legacy, store.FolderStatusGuard{Gen: 1}, types.RunLocalFolderStatus{}); ok || err != nil {
		t.Fatalf("a write at gen 1 on a gen-less record landed: %v %v", ok, err)
	}
	st, ok, err := pg.BumpRunLocalFolderGen(ctx, legacy, store.FolderStatusGuard{Gen: 0})
	if !ok || err != nil || st.Gen != 1 || st.Back == nil || st.Back.Files != 4 {
		t.Fatalf("bump of a gen-less record = %+v %v %v, want gen 1 with its back kept", st, ok, err)
	}
	if _, ok, _ := pg.BumpRunLocalFolderGen(ctx, legacy, store.FolderStatusGuard{Gen: 0}); ok {
		t.Error("a second bump from gen 0 landed")
	}
	st.Settled = 1
	if ok, err := pg.SetRunLocalFolderStatusIf(ctx, legacy, store.FolderStatusGuard{Gen: 1}, st); !ok || err != nil {
		t.Fatalf("a write at the owned gen: %v %v", ok, err)
	}
	if got, err := pg.RunLocalFolderStatus(ctx, legacy); err != nil || got.Gen != 1 || got.Settled != 1 || got.Back == nil {
		t.Errorf("read back %+v %v, want gen 1, settled 1, back kept", got, err)
	}

	gen0 := newFolderRun()
	if ok, err := pg.SetRunLocalFolderStatusIf(ctx, gen0, store.FolderStatusGuard{Gen: 0}, types.RunLocalFolderStatus{Seed: &types.FolderPassRecord{Files: 1, Refused: []types.FolderRefusal{}}}); !ok || err != nil {
		t.Fatalf("a write at gen 0 on a fresh run: %v %v", ok, err)
	}
}
