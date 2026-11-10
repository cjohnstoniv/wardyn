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

// A give-up lands at any generation, once: it records back, drops resume and moves the generation on, so
// the owner that was resuming the pass cannot settle it afterwards; a second give-up does not land, and a
// NULL or gen-less record is given up like any other.
func TestPG_FolderGiveUp_EndsAResumeOnceAndFencesItsOwner(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	runner := uuid.New()
	newFolderRun := func(status string) uuid.UUID {
		r := newRun(types.RunStopped)
		r.LocalFolder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work", RunnerID: &runner, Path: "/Users/alice/code/my-app", Direction: types.ComputerFolderBoth}
		if _, err := pg.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id = $1`, r.ID) })
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET local_folder_status = $2::jsonb WHERE id = $1`, r.ID, status); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	givenUp := types.FolderPassRecord{Error: "given up", Refused: []types.FolderRefusal{}}
	due := store.FolderGiveUpWhen{GiveUpAt: time.Now().Add(-time.Minute)}

	// A later resume's owner bumped to gen 3 and is passing when the give-up lands.
	id := newFolderRun(`{"gen":2,"settled":2,"resume":{"stalled":0,"unsynced":3}}`)
	owned, ok, err := pg.BumpRunLocalFolderGen(ctx, id, store.FolderStatusGuard{Gen: 2})
	if !ok || err != nil || owned.Gen != 3 {
		t.Fatalf("bump = %+v %v %v", owned, ok, err)
	}
	gen, ok, err := pg.GiveUpRunLocalFolder(ctx, id, due, givenUp)
	if !ok || err != nil || gen != 4 {
		t.Fatalf("give-up = %d %v %v, want gen 4", gen, ok, err)
	}
	owned.Back, owned.Resume, owned.Settled = &types.FolderPassRecord{Files: 3, Refused: []types.FolderRefusal{}}, nil, 3
	if ok, err := pg.SetRunLocalFolderStatusIf(ctx, id, store.FolderStatusGuard{Gen: 3}, owned); ok || err != nil {
		t.Fatalf("the old owner's settle landed after the give-up: %v %v", ok, err)
	}
	got, err := pg.RunLocalFolderStatus(ctx, id)
	if err != nil || got.Gen != 4 || got.Resume != nil || got.Back == nil || got.Back.Error != "given up" || got.Settled != 2 {
		t.Fatalf("read back %+v %v, want gen 4, the given-up back, no resume, settled kept", got, err)
	}
	if _, ok, err := pg.GiveUpRunLocalFolder(ctx, id, due, givenUp); ok || err != nil {
		t.Errorf("a second give-up landed: %v %v", ok, err)
	}

	for name, status := range map[string]string{"null": "", "gen-less": `{"seed":{"at":"2026-11-01T00:00:00Z","files":1,"bytes":1,"refused":[]}}`} {
		id := newFolderRun(`{}`)
		q := `UPDATE agent_runs SET local_folder_status = NULL WHERE id = $1`
		args := []any{id}
		if status != "" {
			q, args = `UPDATE agent_runs SET local_folder_status = $2::jsonb WHERE id = $1`, []any{id, status}
		}
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
		if gen, ok, err := pg.GiveUpRunLocalFolder(ctx, id, due, givenUp); !ok || err != nil || gen != 1 {
			t.Errorf("%s: give-up = %d %v %v, want gen 1", name, gen, ok, err)
		}
		if got, err := pg.RunLocalFolderStatus(ctx, id); err != nil || got.Back == nil || got.Gen != 1 || (status != "" && got.Seed == nil) {
			t.Errorf("%s: read back %+v %v, want the given-up back at gen 1 with the rest kept", name, got, err)
		}
	}
}

// A give-up never lands on a live run, nor before its time unless the caller observed D1.
func TestPG_FolderGiveUp_NeverOnALiveRunOrBeforeItsTime(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	runner := uuid.New()
	newFolderRun := func(state types.RunState) uuid.UUID {
		r := newRun(state)
		r.LocalFolder = &types.RunLocalFolder{Name: "my-app", RemoteDir: "/home/agent/work", RunnerID: &runner, Path: "/Users/alice/code/my-app", Direction: types.ComputerFolderBoth}
		if _, err := pg.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agent_runs WHERE id = $1`, r.ID) })
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET local_folder_status = '{"gen":1,"resume":{"stalled":0,"unsynced":2}}'::jsonb WHERE id = $1`, r.ID); err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	back := types.FolderPassRecord{Error: "given up", Refused: []types.FolderRefusal{}}
	past, future := time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	for name, tc := range map[string]struct {
		state types.RunState
		when  store.FolderGiveUpWhen
		want  bool
	}{
		"running, past its time":       {types.RunRunning, store.FolderGiveUpWhen{GiveUpAt: past}, false},
		"running, D1":                  {types.RunRunning, store.FolderGiveUpWhen{GiveUpAt: future, D1: true}, false},
		"terminal, before its time":    {types.RunStopped, store.FolderGiveUpWhen{GiveUpAt: future}, false},
		"terminal, D1 before its time": {types.RunStopped, store.FolderGiveUpWhen{GiveUpAt: future, D1: true}, true},
		"terminal, past its time":      {types.RunStopped, store.FolderGiveUpWhen{GiveUpAt: past}, true},
	} {
		id := newFolderRun(tc.state)
		if _, ok, err := pg.GiveUpRunLocalFolder(ctx, id, tc.when, back); ok != tc.want || err != nil {
			t.Errorf("%s: applied %v %v, want %v", name, ok, err, tc.want)
		}
		if got, _ := pg.RunLocalFolderStatus(ctx, id); !tc.want && (got.Resume == nil || got.Back != nil || got.Gen != 1) {
			t.Errorf("%s: a give-up that did not apply changed the record: %+v", name, got)
		}
	}
}
