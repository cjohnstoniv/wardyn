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

// capacityRunner registers a runner of alice's that may hold maxRuns runs at once, with room for any size.
func capacityRunner(t *testing.T, pg store.PG, maxRuns int) types.Runner {
	t.Helper()
	r := newRunner("alice")
	r.Limits = roomyLimits(maxRuns)
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
	// A first candidate deleted since it was read does not blank the refusal: it names the first one left.
	var full *store.RunnerAtCapacityError
	if _, _, err := start(uuid.New(), busy.ID); !errors.As(err, &full) || full.Runner != busy.Name || full.Max != 1 {
		t.Fatalf("a start whose first runner is gone = %+v, want a refusal naming %q and 1", full, busy.Name)
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

// Only the owner declares a runner's limits, never past the bounds; a revoked runner is gone.
func TestPG_UpdateRunnerLimitsIsTheOwners(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	r := newRunner("alice")
	r.Limits = nil
	got, err := pg.CreateRunner(ctx, r)
	if err != nil || got.Limits != nil || got.MaxRuns != types.RunnerMaxRunsDefault {
		t.Fatalf("a runner registered without limits = %+v, %d, %v; want none and the default runs at once", got.Limits, got.MaxRuns, err)
	}
	want := *sizedLimits(3, 4)
	before, after, err := pg.UpdateRunnerLimits(ctx, r.ID, "alice", want)
	if err != nil || before != nil || after.Limits == nil || *after.Limits != want || after.MaxRuns != 3 {
		t.Fatalf("owner update = %+v -> %+v, %v", before, after.Limits, err)
	}
	if _, _, err := pg.UpdateRunnerLimits(ctx, r.ID, "bob", *sizedLimits(1, 1)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("another person's update = %v, want ErrNotFound", err)
	}
	wider := want
	wider.PerRun.MemoryMiB = wider.Total.MemoryMiB + 1
	if _, _, err := pg.UpdateRunnerLimits(ctx, r.ID, "alice", wider); err == nil {
		t.Fatal("a per-run limit past its total was stored")
	}
	if _, err := pg.RevokeRunner(ctx, r.ID, time.Now().UTC(), "alice", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pg.UpdateRunnerLimits(ctx, r.ID, "alice", want); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a revoked runner's update = %v, want ErrNotFound", err)
	}
}

// sizedLimits is runs at once of a runner whose totals take exactly fit runs of oneRun's size, and
// whose per-run limit is that size.
func sizedLimits(runs, fit int) *types.RunnerLimits {
	one := oneRun()
	return &types.RunnerLimits{RunsAtOnce: runs, PerRun: one, Total: types.RunnerAmounts{
		CPUMillis: one.CPUMillis * int64(fit), MemoryMiB: one.MemoryMiB * int64(fit), DiskMiB: one.DiskMiB * int64(fit)}}
}

// oneRun is the size every run of the limits tests reserves.
func oneRun() types.RunnerAmounts {
	return types.RunnerAmounts{CPUMillis: 1000, MemoryMiB: 2048, DiskMiB: 1024}
}

// remoteRunner makes the remote-hosted runner id with limits.
func remoteRunner(t *testing.T, pg store.PG, id string, limits *types.RunnerLimits) {
	t.Helper()
	if _, _, err := pg.EnsureRemoteRunners(context.Background(), []string{id}, map[string]types.RunnerLimits{id: *limits}); err != nil {
		t.Fatal(err)
	}
	row, err := pg.GetRemoteRunner(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := pg.UpdateRemoteRunner(context.Background(), id, row.Runner.Revision, nil, limits, "test"); err != nil {
		t.Fatal(err)
	}
}

// race creates n runs from make at once and returns how many were admitted, failing on any refusal
// that is not a RunnerAtCapacityError.
func race(t *testing.T, n int, mk func() types.AgentRun, create func(types.AgentRun) error) int {
	t.Helper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	start := make(chan struct{})
	for range n {
		r := mk()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			err := create(r)
			var full *store.RunnerAtCapacityError
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				admitted++
			case !errors.As(err, &full):
				t.Errorf("a racing create = %v, want admitted or a RunnerAtCapacityError", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	return admitted
}

// Racing creates never put more on a runner, either kind, than its totals hold, though its Runs at once
// would take them all: each run's reservation is summed under the runner row's lock.
func TestPG_RunnerTotalsHoldUnderRace(t *testing.T) {
	ctx := context.Background()
	pg := store.NewPG(runsPGPoolIsolated(t))
	p := queuePool(t, pg)
	runner := newRunner("alice")
	runner.Limits = sizedLimits(types.RunnerMaxRunsCeiling, 3)
	if _, err := pg.CreateRunner(ctx, runner); err != nil {
		t.Fatal(err)
	}
	remote := "rp3-race"
	remoteRunner(t, pg, remote, sizedLimits(types.RemoteRunnerMaxRunsCeiling, 2))
	sized := func(r types.AgentRun) types.AgentRun { r.Reserved = oneRun(); return r }
	create := func(r types.AgentRun) error { _, err := pg.CreateRunUnderCap(ctx, r, 0); return err }
	if got := race(t, 8, func() types.AgentRun { return sized(onRunner(p, runner, types.RunPending)) }, create); got != 3 {
		t.Fatalf("%d of 8 racing creates landed on a self-hosted runner whose totals hold 3, want exactly 3", got)
	}
	onRemote := func() types.AgentRun {
		r := sized(poolRun(p, types.RunPending, "alice"))
		r.RunnerPoolExecutor = remote
		return r
	}
	if got := race(t, 8, onRemote, create); got != 2 {
		t.Fatalf("%d of 8 racing creates landed on a remote-hosted runner whose totals hold 2, want exactly 2", got)
	}
	used, err := pg.RunnerUsage(ctx, []uuid.UUID{runner.ID}, []string{remote}, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := used[runner.ID.String()], (types.RunnerTally{Runs: 3, CPUMillis: 3000, MemoryMiB: 6144, DiskMiB: 3072}); got != want {
		t.Fatalf("self-hosted in use = %+v, want %+v", got, want)
	}
	if got := used[remote].Runs; got != 2 {
		t.Fatalf("remote-hosted runs in use = %d, want 2", got)
	}
	// A run larger than the runner's per-run limit never lands there, room or not.
	big := sized(onRunner(p, runner, types.RunPending))
	big.Reserved.MemoryMiB++
	var never *store.RunnerAtCapacityError
	if _, err := pg.CreateRun(ctx, big); !errors.As(err, &never) || !never.NeverFits {
		t.Fatalf("a run past the per-run limit = %v, want refused as one that never fits there", err)
	}
	// A run within the per-run limit refused for room alone is not one that never fits.
	var full *store.RunnerAtCapacityError
	if _, err := pg.CreateRun(ctx, sized(onRunner(p, runner, types.RunPending))); !errors.As(err, &full) || full.NeverFits {
		t.Fatalf("a run with no room = %v, want refused for room, not for size", err)
	}
}

// A run the server launched itself is counted on its remote-hosted runner and never held to its limits:
// its revive is claimed on a runner that is full, where a pooled run's revive is refused.
func TestPG_ServerRunReviveIsExemptFromRunnerLimits(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := queuePool(t, pg)
	remote := "rp3-server"
	remoteRunner(t, pg, remote, sizedLimits(1, 1))
	server, err := pg.CreateRun(ctx, newRun(types.RunRunning))
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.StampServerRun(ctx, server.ID, remote, oneRun()); err != nil {
		t.Fatal(err)
	}
	pooled := poolRun(p, types.RunRunning, "alice")
	pooled.RunnerPoolExecutor, pooled.Reserved = remote, oneRun()
	if _, err := pg.CreateRun(ctx, pooled); !errors.Is(err, store.ErrRunnerAtCapacity) {
		t.Fatalf("a pooled run beside the counted server run = %v, want refused: the runner takes one", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at=now(), lost_reason='reboot' WHERE id=$1`, server.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.CreateRun(ctx, pooled); err != nil {
		t.Fatalf("a pooled run once the server run is kept = %v, want admitted", err)
	}
	if claimed, err := pg.MarkRunRevived(ctx, server.ID, types.LostReboot, nil, 0, true); err != nil || !claimed {
		t.Fatalf("the server run's revive onto the full runner = %v, %v; want claimed", claimed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at=now(), lost_reason='reboot' WHERE id=$1`, pooled.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.MarkRunRevived(ctx, pooled.ID, types.LostReboot, nil, 0, true); !errors.Is(err, store.ErrRunnerAtCapacity) {
		t.Fatalf("a pooled run's revive onto the full runner = %v, want refused", err)
	}
}

// In use as every read sees it is what admission counts, for both kinds, on the same rows: a run whose
// agent no longer runs (a lost run) counts in neither, so a runner with room for k runs at once and j
// live runs reads j in use and admits exactly k - j more.
func TestRunnerUsageMatchesAdmission(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := queuePool(t, pg)
	runner := capacityRunner(t, pg, 4)
	remote := "rp3-usage"
	remoteRunner(t, pg, remote, sizedLimits(4, 4))
	onRemote := func(state types.RunState) types.AgentRun {
		r := poolRun(p, state, "alice")
		r.RunnerPoolExecutor, r.Reserved = remote, oneRun()
		return r
	}
	for _, mk := range []func(types.RunState) types.AgentRun{
		func(s types.RunState) types.AgentRun { r := onRunner(p, runner, s); r.Reserved = oneRun(); return r },
		onRemote,
	} {
		if _, err := pg.CreateRun(ctx, mk(types.RunRunning)); err != nil {
			t.Fatal(err)
		}
		// A run never counts against itself: excluded, its own place and reservation are not in use.
		self := mk(types.RunRunning)
		if _, err := pg.CreateRun(ctx, self); err != nil {
			t.Fatal(err)
		}
		key := runner.ID.String()
		if self.RunnerID == nil {
			key = remote
		}
		with, err := pg.RunnerUsage(ctx, []uuid.UUID{runner.ID}, []string{remote}, uuid.Nil)
		if err != nil {
			t.Fatal(err)
		}
		without, err := pg.RunnerUsage(ctx, []uuid.UUID{runner.ID}, []string{remote}, self.ID)
		if err != nil {
			t.Fatal(err)
		}
		if with[key].Runs-without[key].Runs != 1 || with[key].MemoryMiB-without[key].MemoryMiB != oneRun().MemoryMiB {
			t.Fatalf("%s: excluding a run left %+v of %+v, want its one place and its reservation gone", key, without[key], with[key])
		}
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state = 'COMPLETED' WHERE id = $1`, self.ID); err != nil {
			t.Fatal(err)
		}
		lost, err := pg.CreateRun(ctx, mk(types.RunRunning))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = now(), lost_reason = 'reboot' WHERE id = $1`, lost.ID); err != nil {
			t.Fatal(err)
		}
	}
	used, err := pg.RunnerUsage(ctx, []uuid.UUID{runner.ID}, []string{remote}, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{runner.ID.String(), remote} {
		if got, want := used[key], (types.RunnerTally{Runs: 1, CPUMillis: 1000, MemoryMiB: 2048, DiskMiB: 1024}); got != want {
			t.Errorf("%s: read in use = %+v, want %+v (the lost run holds no sandbox)", key, got, want)
		}
	}
	admitted := 0
	for range 6 {
		if _, err := pg.CreateRun(ctx, func() types.AgentRun { r := onRunner(p, runner, types.RunPending); r.Reserved = oneRun(); return r }()); err == nil {
			admitted++
		}
		if _, err := pg.CreateRun(ctx, onRemote(types.RunPending)); err == nil {
			admitted++
		}
	}
	if admitted != 6 {
		t.Fatalf("admitted %d more runs across both runners, want 3 each: admission and the read disagree about in use", admitted)
	}
}

// A promotion binds a waiting run to a remote-hosted runner with room, and the start keeps that runner:
// the promoted run does not count against itself there (its own place and what it reserves), it starts
// with the size the start names and reserves exactly that, and when the runner it names is full it
// lands on the other remote-hosted runner it is offered.
func TestPG_RemoteStartKeepsItsOwnPlaceAndReservesItsStartSize(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	p := cappedPool(t, pg, 10)
	full, roomy := "rp3-full", "rp3-roomy"
	remoteRunner(t, pg, full, sizedLimits(1, 1))
	size := types.RunnerAmounts{CPUMillis: 1000, MemoryMiB: 4096, DiskMiB: 2048}
	remoteRunner(t, pg, roomy, &types.RunnerLimits{RunsAtOnce: 1, Total: size, PerRun: size})
	occupant := poolRun(p, types.RunRunning, "alice")
	occupant.RunnerPoolExecutor, occupant.Reserved = full, oneRun()
	if _, err := pg.CreateRun(ctx, occupant); err != nil {
		t.Fatal(err)
	}
	waiting := poolRun(p, types.RunQueued, "alice")
	waiting.Reserved = oneRun()
	now := time.Now().UTC()
	queued, err := pg.CreateQueuedRun(ctx, waiting, store.RunQueueEntry{QueuedAt: now, DeadlineAt: now.Add(time.Hour)}, 0)
	if err != nil {
		t.Fatal(err)
	}
	both := func(context.Context, types.RunnerPool, store.RunQueueEntry) (store.QueueVerdict, error) {
		return store.QueueVerdict{Admit: true, Remotes: []string{full, roomy}}, nil
	}
	got, _, err := pg.PromoteRunQueue(ctx, pass(p), both)
	if err != nil || len(got) != 1 || got[0].Run.RunnerPoolExecutor != roomy || got[0].Run.RunnerID != nil {
		t.Fatalf("promoted %+v (%v), want the run bound to the remote-hosted runner with room", got, err)
	}
	// The start names a size larger than the run joined the queue with. The roomy runner takes one run of
	// exactly that size, so it fits only because the promoted run's own place there is not counted.
	start := func(remote string, fallbacks ...string) (types.AgentRun, bool, error) {
		return pg.StartQueuedRun(ctx, queued.ID, store.QueueStart{RunnerPoolExecutor: remote, RemoteFallbacks: fallbacks, Reserved: size,
			Placement: types.PlacementRemote, ConfinementClass: types.CC2})
	}
	if _, _, err := start(full); !errors.Is(err, store.ErrRunnerAtCapacity) {
		t.Fatalf("a start onto the full remote-hosted runner alone = %v, want ErrRunnerAtCapacity", err)
	}
	started, ok, err := start(full, roomy)
	if err != nil || !ok || started.RunnerPoolExecutor != roomy {
		t.Fatalf("a start offered the roomy runner too = %v %v on %q, want it on %s: its own place there is its own", ok, err, started.RunnerPoolExecutor, roomy)
	}
	used, err := pg.RunnerUsage(ctx, nil, []string{roomy}, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := used[roomy], (types.RunnerTally{Runs: 1, CPUMillis: size.CPUMillis, MemoryMiB: size.MemoryMiB, DiskMiB: size.DiskMiB}); got != want {
		t.Fatalf("in use after the start = %+v, want the size the run started with %+v", got, want)
	}
}

// A runner's architecture is stored at registration, kept per connect (the latest winning), left alone
// by a revoked runner, and the database refuses one the runner images are not built for.
func TestPG_RunnerArchIsStoredAndUpdated(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	r := newRunner("alice")
	r.Arch = "amd64"
	got, err := pg.CreateRunner(ctx, r)
	if err != nil || got.Arch != "amd64" {
		t.Fatalf("a runner registered with arch amd64 = %q, %v", got.Arch, err)
	}
	if err := pg.SetRunnerArch(ctx, r.ID, "arm64"); err != nil {
		t.Fatal(err)
	}
	if got, err = pg.GetRunner(ctx, r.ID); err != nil || got.Arch != "arm64" {
		t.Fatalf("after a connect reporting arm64 = %q, %v", got.Arch, err)
	}
	older := newRunner("alice")
	if got, err = pg.CreateRunner(ctx, older); err != nil || got.Arch != "" {
		t.Fatalf("a runner registered without arch = %q, %v; want none", got.Arch, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runners SET arch = 'riscv64' WHERE id = $1`, older.ID); err == nil {
		t.Fatal("the database stored an unknown architecture")
	}
	if _, err := pg.RevokeRunner(ctx, r.ID, time.Now().UTC(), "alice", ""); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetRunnerArch(ctx, r.ID, "amd64"); err != nil {
		t.Fatal(err)
	}
	if got, err = pg.GetRunner(ctx, r.ID); err != nil || got.Arch != "arm64" {
		t.Fatalf("a revoked runner's arch = %q, %v; want the one it had", got.Arch, err)
	}
}
