// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// capacityRunner registers a runner of alice's that may hold maxRuns runs at once.
func capacityRunner(t *testing.T, pg store.PG, maxRuns int) types.Runner {
	t.Helper()
	r := newRunner("alice")
	r.MaxRuns = maxRuns
	got, err := pg.CreateRunner(context.Background(), r)
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}
	return got
}

// onRunner is a run of alice's in p bound to runner, in state.
func onRunner(p types.RunnerPool, runner types.Runner, state types.RunState) types.AgentRun {
	r := poolRun(p, state, "alice")
	r.RunnerID = &runner.ID
	return r
}

// A runner that holds two runs at once never gets a third, however many launches race for it, and takes
// one again once a run ends.
func TestPG_RunnerMaxRunsHoldsUnderConcurrentCreates(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := queuePool(t, pg)
	runner := capacityRunner(t, pg, 2)
	const racers = 8
	var wg sync.WaitGroup
	created := make(chan types.AgentRun, racers)
	errs := make(chan error, racers)
	start := make(chan struct{})
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, err := pg.CreateRun(ctx, onRunner(p, runner, types.RunPending))
			if err != nil {
				errs <- err
				return
			}
			created <- r
		}()
	}
	close(start)
	wg.Wait()
	close(created)
	close(errs)
	var ids []uuid.UUID
	for r := range created {
		ids = append(ids, r.ID)
	}
	if len(ids) != 2 {
		t.Fatalf("%d of %d racing creates landed on a runner with max_runs 2, want exactly 2", len(ids), racers)
	}
	for err := range errs {
		var full *store.RunnerAtCapacityError
		if !errors.As(err, &full) || full.Max != 2 || full.Runner != runner.Name {
			t.Fatalf("a refused create = %v, want a RunnerAtCapacityError naming the runner and its 2", err)
		}
	}
	if n, err := pg.RunnerSlotHolders(ctx, []uuid.UUID{runner.ID}); err != nil || n[runner.ID] != 2 {
		t.Fatalf("slot holders = %v, %v; want 2", n, err)
	}
	// A QUEUED run holds no place on its runner.
	if _, err := enqueueOnRunner(t, pg, p, runner); err != nil {
		t.Fatalf("a waiting run was refused for the runner's limit: %v", err)
	}
	if ok, err := pg.UpdateRunStateIf(ctx, ids[0], types.RunPending, types.RunCompleted); err != nil || !ok {
		t.Fatal(err)
	}
	if _, err := pg.CreateRun(ctx, onRunner(p, runner, types.RunPending)); err != nil {
		t.Fatalf("a create after a run ended = %v, want admitted", err)
	}
}

func enqueueOnRunner(t *testing.T, pg store.PG, p types.RunnerPool, runner types.Runner) (types.AgentRun, error) {
	t.Helper()
	now := time.Now().UTC()
	return pg.CreateQueuedRun(context.Background(), onRunner(p, runner, types.RunQueued), store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)}, 0)
}

// Promotion binds a waiting run to one of its runners that still has room under the runner's lock, and
// leaves the rest waiting when none has, though the pool itself has room.
func TestPG_PromotionHoldsEachRunnerToItsMaxRuns(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 10)
	busy := capacityRunner(t, pg, 1)
	free := capacityRunner(t, pg, 1)
	if _, err := pg.CreateRun(ctx, onRunner(p, busy, types.RunRunning)); err != nil {
		t.Fatal(err)
	}
	var queued []uuid.UUID
	for range 3 {
		r, err := enqueueOnRunner(t, pg, p, busy)
		if err != nil {
			t.Fatal(err)
		}
		queued = append(queued, r.ID)
	}
	// The verdict, read without a lock, offers both runners for every run.
	both := func(context.Context, types.RunnerPool, store.RunQueueEntry) (store.QueueVerdict, error) {
		return store.QueueVerdict{Admit: true, Runners: []uuid.UUID{busy.ID, free.ID}}, nil
	}
	got, _, err := pg.PromoteRunQueue(ctx, pass(p), both)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Run.RunnerID == nil || *got[0].Run.RunnerID != free.ID {
		t.Fatalf("promoted %+v, want exactly one run, bound to the runner with room", got)
	}
	for _, id := range queued[1:] {
		if r := runState(t, pg, id); r.State != types.RunQueued {
			t.Errorf("run %s is %s, want it still waiting: both runners are at their limit", id, r.State)
		}
	}
	if again, _, err := pg.PromoteRunQueue(ctx, pass(p), both); err != nil || len(again) != 0 {
		t.Fatalf("a second pass promoted %d (%v), want none while both runners are full", len(again), err)
	}

	// The start keeps the runner the promotion found room on; moving to a full one is refused.
	if _, _, err := pg.StartQueuedRun(ctx, got[0].Run.ID, store.QueueStart{RunnerID: &busy.ID, Placement: types.PlacementLocal, ConfinementClass: types.CC2}); !errors.Is(err, store.ErrRunnerAtCapacity) {
		t.Fatalf("a start onto the full runner = %v, want ErrRunnerAtCapacity", err)
	}
	if _, ok, err := pg.StartQueuedRun(ctx, got[0].Run.ID, store.QueueStart{RunnerID: &free.ID, Placement: types.PlacementLocal, ConfinementClass: types.CC2}); err != nil || !ok {
		t.Fatalf("a start on the runner it was promoted to = %v %v, want admitted (it does not count itself)", ok, err)
	}
}

// Only the owner changes a runner's max_runs, never past the bounds; a revoked runner is gone.
func TestPG_UpdateRunnerMaxRunsIsTheOwners(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	r := newRunner("alice")
	got, err := pg.CreateRunner(ctx, r)
	if err != nil || got.MaxRuns != types.RunnerMaxRunsDefault {
		t.Fatalf("a runner registered without max_runs = %d, %v; want the default", got.MaxRuns, err)
	}
	before, after, err := pg.UpdateRunnerMaxRuns(ctx, r.ID, "alice", 6)
	if err != nil || before != types.RunnerMaxRunsDefault || after.MaxRuns != 6 {
		t.Fatalf("owner update = %d -> %+v, %v", before, after.MaxRuns, err)
	}
	if _, _, err := pg.UpdateRunnerMaxRuns(ctx, r.ID, "bob", 1); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another person's update = %v, want ErrNotFound", err)
	}
	if _, _, err := pg.UpdateRunnerMaxRuns(ctx, r.ID, "alice", types.RunnerMaxRunsCeiling+1); err == nil {
		t.Fatal("a max_runs past the ceiling was stored")
	}
	if _, err := pg.RevokeRunner(ctx, r.ID, time.Now().UTC(), "alice", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pg.UpdateRunnerMaxRuns(ctx, r.ID, "alice", 3); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a revoked runner's update = %v, want ErrNotFound", err)
	}
}
