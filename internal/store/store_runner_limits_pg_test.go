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

	"github.com/cjohnstoniv/wardyn/internal/db"
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

	// A start onto a full runner with nowhere else to go is refused; with the run's other runner offered it
	// lands there (the promoted run does not count against itself), never ending for one full runner.
	start := func(runner uuid.UUID, fallbacks ...uuid.UUID) (types.AgentRun, bool, error) {
		return pg.StartQueuedRun(ctx, got[0].Run.ID, store.QueueStart{RunnerID: &runner, RunnerFallbacks: fallbacks,
			Placement: types.PlacementLocal, ConfinementClass: types.CC2})
	}
	if _, _, err := start(busy.ID); !errors.Is(err, store.ErrRunnerAtCapacity) {
		t.Fatalf("a start onto the full runner alone = %v, want ErrRunnerAtCapacity", err)
	}
	started, ok, err := start(busy.ID, free.ID)
	if err != nil || !ok || started.RunnerID == nil || *started.RunnerID != free.ID {
		t.Fatalf("a start offered the free runner too = %v %v on %v, want it on %s", ok, err, started.RunnerID, free.ID)
	}
}

// A pass spreads its runs: a run whose runners all have room goes to the one with the most left.
func TestPG_PromotionBindsTheRoomiestRunner(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := cappedPool(t, pg, 10)
	loaded := capacityRunner(t, pg, 3)
	idle := capacityRunner(t, pg, 3)
	if _, err := pg.CreateRun(ctx, onRunner(p, loaded, types.RunRunning)); err != nil {
		t.Fatal(err)
	}
	if _, err := enqueueOnRunner(t, pg, p, loaded); err != nil {
		t.Fatal(err)
	}
	got, _, err := pg.PromoteRunQueue(ctx, pass(p), func(context.Context, types.RunnerPool, store.RunQueueEntry) (store.QueueVerdict, error) {
		return store.QueueVerdict{Admit: true, Runners: []uuid.UUID{loaded.ID, idle.ID}}, nil
	})
	if err != nil || len(got) != 1 || *got[0].Run.RunnerID != idle.ID {
		t.Fatalf("promoted %+v (%v), want the run on the idle runner", got, err)
	}
}

// A revive that starts an agent takes a place on its runner: on a runner at its max_runs it is refused,
// naming the runner and its limit, and claimed once a run there ends.
func TestPG_ReviveOntoAFullRunnerIsRefused(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := queuePool(t, pg)
	runner := capacityRunner(t, pg, 1)
	kept, err := pg.CreateRun(ctx, onRunner(p, runner, types.RunRunning))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at=now(), lost_reason='reboot' WHERE id=$1`, kept.ID); err != nil {
		t.Fatal(err)
	}
	busy, err := pg.CreateRun(ctx, onRunner(p, runner, types.RunRunning))
	if err != nil {
		t.Fatalf("a kept run holds no place, so another run takes the runner's one: %v", err)
	}
	_, err = pg.MarkRunRevived(ctx, kept.ID, types.LostReboot, nil, 0, true)
	var full *store.RunnerAtCapacityError
	if !errors.As(err, &full) || full.Max != 1 || full.Runner != runner.Name {
		t.Fatalf("revive onto a full runner = %v, want a RunnerAtCapacityError naming it and 1", err)
	}
	if ok, err := pg.UpdateRunStateIf(ctx, busy.ID, types.RunRunning, types.RunCompleted); err != nil || !ok {
		t.Fatal(err)
	}
	if claimed, err := pg.MarkRunRevived(ctx, kept.ID, types.LostReboot, nil, 0, true); err != nil || !claimed {
		t.Fatalf("revive once the runner has room = %v, %v; want claimed", claimed, err)
	}
}

// A create into a pool takes the pool row before the deployment cap's advisory lock, the order a
// promotion and a revive use: while another writer holds the pool row, a waiting create holds no cap
// lock that writer could next wait on.
func TestPG_CreateTakesThePoolBeforeTheRunCap(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := queuePool(t, pg)
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if _, err := holder.Exec(ctx, `SELECT 1 FROM runner_pools WHERE id = $1 FOR NO KEY UPDATE`, p.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := pg.CreateRunUnderCap(ctx, poolRun(p, types.RunPending, "alice"), 100)
		done <- err
	}()
	waiting := false
	for deadline := time.Now().Add(5 * time.Second); !waiting && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		// pg_stat_activity is a snapshot per transaction; holder's has to be dropped for each look.
		if _, err := holder.Exec(ctx, `SELECT pg_stat_clear_snapshot()`); err != nil {
			t.Fatal(err)
		}
		if err := holder.QueryRow(ctx, `SELECT count(*) > 0 FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
	}
	if !waiting {
		t.Fatal("the create never waited on the held pool row")
	}
	var free bool
	// Probed through the held connection: a second acquire from this goroutine trips WARDYN_TEST_POOL_GUARD.
	if err := holder.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1, 0)`, db.RunCapLockClass).Scan(&free); err != nil {
		t.Fatal(err)
	}
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the create after the pool was released = %v", err)
	}
	if !free {
		t.Fatal("a create waiting on the pool row already held the deployment cap's lock: pool before cap is broken")
	}
}

// Only the owner changes a runner's max_runs, never past the bounds; a revoked runner is gone.
func TestPG_UpdateRunnerMaxRunsIsTheOwners(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	r := newRunner("alice")
	r.MaxRuns = 0
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
