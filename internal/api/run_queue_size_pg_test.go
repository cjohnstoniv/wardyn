// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reservedOn is what run reserves on its runner, as stored.
func (e *queueEnv) reservedOn(run uuid.UUID) types.RunnerAmounts {
	e.t.Helper()
	var a types.RunnerAmounts
	if err := e.pool.QueryRow(context.Background(), `SELECT reserved_cpu_millis, reserved_memory_mib, reserved_disk_mib FROM agent_runs WHERE id = $1`, run).
		Scan(&a.CPUMillis, &a.MemoryMiB, &a.DiskMiB); err != nil {
		e.t.Fatal(err)
	}
	return a
}

// A waiting run is judged, promoted and started at the size it would START with on each runner, never the
// size it joined the queue with: bound to its laptop when it joined, it is promoted onto the idle
// remote-hosted runner whose per-run disk is under that size, and starts there holding that runner's
// size. While that runner has runs left but no disk left for the run, the queue says its runners are at
// capacity instead of admitting a run the promotion would pass over.
func TestPG_QueuedRunIsSizedForTheRunnerItIsPromotedOnto(t *testing.T) {
	e := newQueueEnv(t)
	pool, laptop := e.mixedPool("Mixed")
	e.maxRuns(laptop, e.personP, 1)
	ctx := context.Background()
	limits := types.RunnerPoolLimits{
		Background: &types.RunnerPoolBackgroundLimits{CPUMillis: &types.RunnerPoolAmount{Default: 500, Cap: 4000}, MemoryMiB: &types.RunnerPoolAmount{Default: 512, Cap: 8192},
			Lifetime: &types.RunnerPoolDuration{DefaultSec: 3600, MaxSec: 7200}},
		Barriers: []types.ConfinementClass{types.CC1, types.CC2, types.CC3},
	}
	poolCap := func(n int) {
		limits.MaxConcurrentRuns = intp(n)
		if _, err := e.pg.UpdateRunnerPool(ctx, pool.ID, e.revision(pool.ID), nil, nil, &limits, nil); err != nil {
			t.Fatal(err)
		}
	}
	poolCap(1)
	occupant := e.occupy(pool)
	queued := e.queuedLaunch(e.person, e.reread(pool), "")

	var alpha types.RunnerLimits
	if err := e.pool.QueryRow(ctx, `SELECT run_cpu_millis, run_memory_mib, run_disk_mib FROM remote_runners WHERE id = 'alpha'`).
		Scan(&alpha.PerRun.CPUMillis, &alpha.PerRun.MemoryMiB, &alpha.PerRun.DiskMiB); err != nil {
		t.Fatal(err)
	}
	joined := e.reservedOn(queued.ID)
	if joined.DiskMiB <= alpha.PerRun.DiskMiB {
		t.Fatalf("the run joined reserving %d MiB of disk, alpha takes %d a run: the case needs a join-time size alpha does not take", joined.DiskMiB, alpha.PerRun.DiskMiB)
	}
	var raw json.RawMessage
	var launch queuedLaunch
	if err := e.pool.QueryRow(ctx, `SELECT launch FROM run_queue WHERE run_id = $1`, queued.ID).Scan(&raw); err != nil || json.Unmarshal(raw, &launch) != nil || launch.Unbound == nil {
		t.Fatalf("the queued launch %s (%v) records no unbound size", raw, err)
	}
	want := startSizeOn(*launch.Unbound, &alpha)
	if want != (types.RunnerAmounts{CPUMillis: 500, MemoryMiB: 512, DiskMiB: alpha.PerRun.DiskMiB}) {
		t.Fatalf("the run's size on alpha = %+v, want the pool's defaults and alpha's per-run disk %d", want, alpha.PerRun.DiskMiB)
	}

	// The laptop fills while the run waits, and the pool's cap is raised so only runner room decides. Alpha
	// has runs left, but its disk total is one run's and another run holds a part of it.
	e.occupyRunner(e.reread(pool), laptop, e.personP)
	if _, err := e.pool.Exec(ctx, `UPDATE remote_runners SET total_disk_mib = run_disk_mib WHERE id = 'alpha'`); err != nil {
		t.Fatal(err)
	}
	holder, now := uuid.New(), time.Now().UTC()
	if _, err := e.pg.CreateRun(ctx, types.AgentRun{ID: holder, CreatedAt: now, UpdatedAt: now, CreatedBy: "occupant", Agent: "claude-code",
		ConfinementClass: types.CC2, State: types.RunRunning, SPIFFEID: "spiffe://wardyn.local/agent-run/" + holder.String(), RunnerTarget: "docker",
		Placement: types.PlacementRemote, RunnerPoolExecutor: "alpha", Reserved: types.RunnerAmounts{DiskMiB: 1024}}); err != nil {
		t.Fatal(err)
	}
	poolCap(5)
	room, err := e.srv.queueRunnerFree(withOIDCHuman(ctx, e.personP), e.reread(pool), launch)
	if err != nil || room.wait != runnerpool.WaitRunnersAtCapacity || len(room.runners)+len(room.remotes) != 0 {
		t.Fatalf("the queue's verdict with no disk left on alpha = %+v, %v; want it held for runners_at_capacity", room, err)
	}
	e.free(occupant)
	e.sweep()
	if r := e.run(queued.ID); r.State != types.RunQueued || r.StatusDetail != statusDetailQueuePrefix+string(runnerpool.WaitRunnersAtCapacity) {
		t.Fatalf("the run is %s (%q) with no room for it on any runner, want it still waiting for its runners", r.State, r.StatusDetail)
	}

	if ok, err := e.srv.casRunState(ctx, holder, types.RunRunning, types.RunCompleted); err != nil || !ok {
		t.Fatalf("end alpha's run: %v %v", ok, err)
	}
	e.sweep()
	var got types.AgentRun
	e.eventually("the run to leave the queue", func() bool {
		got = e.run(queued.ID)
		return got.State == types.RunRunning || got.State == types.RunFailed || got.State == types.RunCompleted
	})
	if got.State != types.RunRunning || got.RunnerID != nil || got.RunnerPoolExecutor != "alpha" || got.Placement != types.PlacementRemote {
		t.Fatalf("the run is %s on runner %v executor %q (%s), want it RUNNING on alpha alone", got.State, got.RunnerID, got.RunnerPoolExecutor, got.StatusDetail)
	}
	if held := e.reservedOn(queued.ID); held != want {
		t.Errorf("the started run reserves %+v, want the size it was promoted at %+v", held, want)
	}
	used, err := e.pg.RunnerUsage(ctx, []uuid.UUID{laptop}, []string{"alpha"}, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if used[laptop.String()].Runs != 1 || used["alpha"] != (types.RunnerTally{Runs: 1, CPUMillis: want.CPUMillis, MemoryMiB: want.MemoryMiB, DiskMiB: want.DiskMiB}) {
		t.Errorf("in use: laptop %+v, alpha %+v; want the laptop's one run and alpha holding the run at %+v", used[laptop.String()], used["alpha"], want)
	}
}

// The start sizes a promoted run by the runner its promotion chose. With the roomy runner full, a waiting
// run is promoted onto the runner with the smaller per-run disk and starts there at that size: it is
// neither left waiting for the larger runner nor ended because a start sized for the larger one fits
// nowhere.
func TestPG_QueuedRunStartsAtTheSizeOfTheRunnerItsPromotionChose(t *testing.T) {
	e := newQueueEnv(t)
	p, a, b := cappedLaptops(e, 1)
	ctx := context.Background()
	const small = 2048
	if _, err := e.pool.Exec(ctx, `UPDATE runners SET run_disk_mib = $2 WHERE id = $1`, b, small); err != nil {
		t.Fatal(err)
	}
	occupyRunner(e, p, a)
	_, got := launchThroughQueue(e, p, a, b)
	if got.State == types.RunFailed || got.RunnerID == nil || *got.RunnerID != b {
		t.Fatalf("the run ended %s on %v (%s), want it started on the runner with room %s", got.State, got.RunnerID, got.StatusDetail, b)
	}
	if held := e.reservedOn(got.ID); held.DiskMiB != small {
		t.Errorf("the run reserves %d MiB of disk, want the %d its runner takes a run", held.DiskMiB, small)
	}
}
