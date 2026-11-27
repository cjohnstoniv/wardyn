// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func cloneStatus(id uuid.UUID, state, repos string) store.RunCloneStatus {
	return store.RunCloneStatus{RunID: id, State: state, Repos: json.RawMessage(repos), ObservedAt: time.Now().UTC()}
}

func cloneStatusRunIDs(t *testing.T, s store.PG) []uuid.UUID {
	t.Helper()
	runs, err := s.ListCloneStatusRuns(context.Background())
	if err != nil {
		t.Fatalf("ListCloneStatusRuns: %v", err)
	}
	var ids []uuid.UUID
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	return ids
}

// A cloning row is replaced once by the final state; a final row is never overwritten, and a second cloning mark
// writes nothing.
func TestRunCloneStatus_FinalReplacesCloningOnce_PG(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))

	if _, err := s.GetRunCloneStatus(ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("no row: err %v, want ErrNotFound", err)
	}
	steps := []struct {
		put       store.RunCloneStatus
		wantWrote bool
		wantState string
	}{
		{cloneStatus(run.ID, store.CloneStateCloning, `[]`), true, store.CloneStateCloning},
		{cloneStatus(run.ID, store.CloneStateCloning, `[]`), false, store.CloneStateCloning},
		{cloneStatus(run.ID, store.CloneStateReported, `[{"dest":"/w/a","outcome":"ok"}]`), true, store.CloneStateReported},
		{cloneStatus(run.ID, store.CloneStateNotReported, `[{"dest":"/w/a","outcome":"not_reported"}]`), false, store.CloneStateReported},
		{cloneStatus(run.ID, store.CloneStateCloning, `[]`), false, store.CloneStateReported},
	}
	for i, st := range steps {
		wrote, err := s.PutRunCloneStatus(ctx, st.put)
		if err != nil || wrote != st.wantWrote {
			t.Fatalf("step %d: wrote %v, err %v; want wrote %v", i, wrote, err, st.wantWrote)
		}
		got, err := s.GetRunCloneStatus(ctx, run.ID)
		if err != nil || got.State != st.wantState {
			t.Fatalf("step %d: state %q, err %v; want %q", i, got.State, err, st.wantState)
		}
	}
	got, _ := s.GetRunCloneStatus(ctx, run.ID)
	var repos []struct{ Dest, Outcome string }
	if err := json.Unmarshal(got.Repos, &repos); err != nil || len(repos) != 1 || repos[0].Outcome != "ok" {
		t.Errorf("repos %s (%v), want the reported entry kept", got.Repos, err)
	}
}

func TestRunCloneStatus_DeletedWithRun_PG(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	if _, err := s.PutRunCloneStatus(ctx, cloneStatus(run.ID, store.CloneStateReported, `[]`)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM agent_runs WHERE id = $1`, run.ID); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	if _, err := s.GetRunCloneStatus(ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("after the run was deleted: err %v, want ErrNotFound", err)
	}
}

// A final row drops the run from the list; a cloning row keeps it. Only a RUNNING run with a sandbox, not lost or
// paused, is listed.
func TestRunCloneStatus_ListSkipsRecorded_PG(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	mk := func(state types.RunState, ref string) types.AgentRun {
		r := newRun(state)
		r.SandboxRef = ref
		return persistRun(t, ctx, pool, r)
	}
	fresh, cloning, final := mk(types.RunRunning, "ctr-fresh"), mk(types.RunRunning, "ctr-cloning"), mk(types.RunRunning, "ctr-final")
	noSandbox, starting, paused := mk(types.RunRunning, ""), mk(types.RunStarting, "ctr-start"), mk(types.RunRunning, "ctr-paused")
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET paused_at=now() WHERE id=$1`, paused.ID); err != nil {
		t.Fatal(err)
	}
	for id, state := range map[uuid.UUID]string{cloning.ID: store.CloneStateCloning, final.ID: store.CloneStateNone} {
		if _, err := s.PutRunCloneStatus(ctx, cloneStatus(id, state, `[]`)); err != nil {
			t.Fatal(err)
		}
	}
	ids := cloneStatusRunIDs(t, s)
	for name, id := range map[string]uuid.UUID{"a run with no row": fresh.ID, "a run with a cloning row": cloning.ID} {
		if !slices.Contains(ids, id) {
			t.Errorf("%s is not listed: %v", name, ids)
		}
	}
	for name, id := range map[string]uuid.UUID{"a final row": final.ID, "no sandbox": noSandbox.ID, "starting": starting.ID, "paused": paused.ID} {
		if slices.Contains(ids, id) {
			t.Errorf("a run with %s is listed", name)
		}
	}
}

// A revive that starts the agent again forgets the run's clone status: the row is gone and the run is listed again.
func TestRunCloneStatus_Delete_PG(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	s := store.NewPG(pool)
	r := newRun(types.RunRunning)
	r.SandboxRef = "ctr-delete"
	run := persistRun(t, ctx, pool, r)
	if _, err := s.PutRunCloneStatus(ctx, cloneStatus(run.ID, store.CloneStateReported, `[{"dest":"/w/a","outcome":"failed"}]`)); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cloneStatusRunIDs(t, s), run.ID) {
		t.Fatal("a run with a final row is listed")
	}
	if err := s.DeleteRunCloneStatus(ctx, run.ID); err != nil {
		t.Fatalf("DeleteRunCloneStatus: %v", err)
	}
	if _, err := s.GetRunCloneStatus(ctx, run.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("after the delete: err %v, want ErrNotFound", err)
	}
	if !slices.Contains(cloneStatusRunIDs(t, s), run.ID) {
		t.Error("the run is not listed again after the delete")
	}
	if err := s.DeleteRunCloneStatus(ctx, run.ID); err != nil {
		t.Errorf("deleting a run with no row: %v", err)
	}
}
