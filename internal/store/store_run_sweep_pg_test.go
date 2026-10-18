// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The narrow sweep reads must return what the Go filter over ListRuns returned, in the same order: they
// replaced that filter in the boot reconcile and the hourly sweeps. Every run state meets every sandbox_ref
// shape, so a state the SQL list forgot, or a ref test written the wrong way round, changes an answer.
func TestPG_RunSweepReadsMatchTheListRunsFilter(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	states := []types.RunState{types.RunQueued, types.RunPending, types.RunStarting, types.RunRunning, types.RunWaiting,
		types.RunStopped, types.RunArchived, types.RunFailed, types.RunKilled, types.RunCompleted}
	base := time.Now().UTC().Truncate(time.Second)
	var ids []uuid.UUID
	i := 0
	for _, st := range states {
		for _, ref := range []string{"", "sandbox-" + string(st)} {
			run := newRun(st)
			run.SandboxRef = ref
			run.CreatedAt = base.Add(-time.Duration(i) * time.Minute) // distinct, so the order is total
			persistRun(t, ctx, pool, run)
			ids = append(ids, run.ID)
			i++
		}
	}

	all, err := pg.ListRuns(ctx)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	idsOf := func(runs []types.AgentRun, keep func(types.AgentRun) bool) []uuid.UUID {
		out := []uuid.UUID{}
		for _, r := range runs {
			if keep(r) {
				out = append(out, r.ID)
			}
		}
		return out
	}
	for _, c := range []struct {
		name string
		read func(context.Context) ([]types.AgentRun, error)
		keep func(types.AgentRun) bool
	}{
		{"undispatched", pg.ListUndispatchedRuns, func(r types.AgentRun) bool {
			return slices.Contains(types.SlotHoldingRunStates, r.State) && r.SandboxRef == ""
		}},
		{"terminal with a sandbox", pg.ListTerminalRunsWithSandbox, func(r types.AgentRun) bool {
			return r.State.IsTerminal() && r.SandboxRef != ""
		}},
		{"live", pg.ListLiveRuns, func(r types.AgentRun) bool { return !r.State.IsTerminal() }},
	} {
		got, err := c.read(ctx)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		want := idsOf(all, c.keep)
		if len(want) == 0 {
			t.Fatalf("%s: the seed leaves nothing to select, so the check proves nothing", c.name)
		}
		if gotIDs := idsOf(got, func(types.AgentRun) bool { return true }); !slices.Equal(gotIDs, want) {
			t.Errorf("%s: got %v, want %v (ListRuns filtered in Go, same order)", c.name, gotIDs, want)
		}
	}

	pick := []uuid.UUID{ids[0], ids[7], ids[len(ids)-1], uuid.New()} // the last has no row
	got, err := pg.GetRunsByID(ctx, pick)
	if err != nil {
		t.Fatalf("GetRunsByID: %v", err)
	}
	want := idsOf(all, func(r types.AgentRun) bool { return slices.Contains(pick, r.ID) })
	if gotIDs := idsOf(got, func(types.AgentRun) bool { return true }); !slices.Equal(gotIDs, want) || len(want) != 3 {
		t.Errorf("GetRunsByID = %v, want the three that exist, newest first: %v", gotIDs, want)
	}
	if empty, err := pg.GetRunsByID(ctx, nil); err != nil || len(empty) != 0 {
		t.Errorf("GetRunsByID(nil) = %d runs, %v, want none", len(empty), err)
	}
}
