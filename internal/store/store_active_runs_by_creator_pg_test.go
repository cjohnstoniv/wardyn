// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_ActiveRunsByCreator is the read the login supersede makes before a new
// sign-in launches (internal/api/harnesscred_supersede.go): THIS person's
// still-live runs of THIS lane, and nobody else's.
//
// Every arm here is a run the supersede must NOT return, because each one is a
// run the kill cascade would otherwise end: another person's sign-in sandbox,
// the same person's ordinary work, their login box on a different agent, and
// their own sign-in that has already finished.
func TestPG_ActiveRunsByCreator(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	const (
		mine    = "member@corp.example"
		theirs  = "other@corp.example"
		task    = "harness login"
		agent   = "aws-sso"
		another = "claude-code"
	)

	live := newRun(types.RunRunning)
	live.CreatedBy, live.Task, live.Agent = mine, task, agent
	live = persistRun(t, ctx, pool, live)

	pending := newRun(types.RunPending)
	pending.CreatedBy, pending.Task, pending.Agent = mine, task, agent
	pending = persistRun(t, ctx, pool, pending)

	for _, r := range []struct {
		name string
		mut  func(*types.AgentRun)
	}{
		{"another person's sign-in", func(r *types.AgentRun) { r.CreatedBy, r.Task, r.Agent = theirs, task, agent }},
		{"my own ordinary work", func(r *types.AgentRun) { r.CreatedBy, r.Task, r.Agent = mine, "ship the thing", agent }},
		{"my login box on another agent", func(r *types.AgentRun) { r.CreatedBy, r.Task, r.Agent = mine, task, another }},
	} {
		run := newRun(types.RunRunning)
		r.mut(&run)
		persistRun(t, ctx, pool, run)
	}
	ended := newRun(types.RunKilled)
	ended.CreatedBy, ended.Task, ended.Agent = mine, task, agent
	persistRun(t, ctx, pool, ended)

	got, err := pg.ActiveRunsByCreator(ctx, mine, task, agent)
	if err != nil {
		t.Fatalf("ActiveRunsByCreator: %v", err)
	}
	want := map[uuid.UUID]bool{live.ID: true, pending.ID: true}
	if len(got) != len(want) {
		t.Fatalf("got %d runs, want %d — a supersede acts on every row this returns", len(got), len(want))
	}
	for _, r := range got {
		if !want[r.ID] {
			t.Errorf("run %s (%s/%s/%s state=%s) is not this caller's live sign-in",
				r.ID, r.CreatedBy, r.Task, r.Agent, r.State)
		}
		if r.SPIFFEID == "" {
			t.Errorf("run %s came back with no SPIFFE id — the column list is short of runCols", r.ID)
		}
	}
}
