// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// occupyRemote fills the remote-hosted runner id (Runs at once 1) with a RUNNING run of pool's.
func occupyRemote(e *placementEnv, pool types.RunnerPool, id string) {
	e.t.Helper()
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE remote_runners SET max_runs = 1 WHERE id = $1`, id); err != nil {
		e.t.Fatal(err)
	}
	run, now := uuid.New(), time.Now().UTC()
	if _, err := e.pg.CreateRun(ctx, types.AgentRun{ID: run, CreatedAt: now, UpdatedAt: now, CreatedBy: "occupant", Agent: "claude-code",
		ConfinementClass: types.CC2, State: types.RunRunning, SPIFFEID: "spiffe://wardyn.local/agent-run/" + run.String(), RunnerTarget: "docker",
		Placement: types.PlacementRemote, RunnerPoolID: &pool.ID, RunnerPoolRevision: e.revision(pool.ID), RunnerPoolSelection: types.RunnerPoolSelectedExplicit,
		RunnerPoolExecutor: id}); err != nil {
		e.t.Fatalf("occupy %s: %v", id, err)
	}
}

// A queued run promoted onto the one remote-hosted runner with room starts there: the start keeps the
// promotion's runner, and is never ended because the run's own pick is the full one.
func TestPG_QueuedRemoteRunStartsOnThePromotedRemote(t *testing.T) {
	e := newQueueEnv(t)
	p := e.cappedPool("Shared", 5)
	e.addExecutors(p.ID, "beta")
	p = e.reread(p)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE remote_runners SET max_runs = 1 WHERE id = 'beta'`); err != nil {
		t.Fatal(err)
	}
	occupyRemote(e.placementEnv, p, "alpha")
	sawAlpha := false
	for i := 0; i < 24 && !sawAlpha; i++ {
		out, w := e.launch(nil, p, "")
		if w.Code != http.StatusCreated {
			t.Fatalf("launch %d = %d %s", i, w.Code, w.Body)
		}
		pick := runnerpool.Select(p.ID, out.ID, []string{"alpha", "beta"})
		sawAlpha = pick == "alpha"
		e.sweep()
		var got types.AgentRun
		e.eventually("the run to leave the queue", func() bool {
			got = e.run(out.ID)
			return got.State == types.RunRunning || got.State == types.RunFailed || got.State == types.RunCompleted
		})
		if got.State == types.RunFailed || got.RunnerPoolExecutor != "beta" {
			t.Fatalf("launch %d (own pick %s) = %s on %q detail %q; want it started on beta, the remote-hosted runner with room", i, pick, got.State, got.RunnerPoolExecutor, got.StatusDetail)
		}
		if ok, err := e.srv.casRunState(ctx, got.ID, got.State, types.RunCompleted); err != nil || !ok {
			t.Fatalf("end: %v %v", ok, err)
		}
	}
	if !sawAlpha {
		t.Fatal("no launch's own pick was the full runner, so the path was not exercised")
	}
}

// What a queued run reserves on its runner is the size it starts with, not the size it joined the queue
// with: the pool's default CPU is raised while it waits, and its reservation is what its sandbox gets.
func TestPG_QueuedRunReservesTheSizeItStartsWith(t *testing.T) {
	e := newQueueEnv(t)
	p, _, _ := cappedLaptops(e, 4)
	ctx := context.Background()
	hold := e.occupy(p)
	if _, err := e.pool.Exec(ctx, `UPDATE runner_pools SET limits = jsonb_set(limits, '{max_concurrent_runs}', '1'), revision = revision + 1 WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	p = e.reread(p)
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", endBody("background", `"runner_pool_id":"`+p.ID.String()+`"`))
	if w.Code != http.StatusCreated {
		t.Fatalf("launch = %d %s", w.Code, w.Body)
	}
	created := decodeJSON[types.AgentRun](t, w)
	if created.State != types.RunQueued {
		t.Fatalf("created %s, want QUEUED", created.State)
	}
	var atQueue int64
	if err := e.pool.QueryRow(ctx, `SELECT reserved_cpu_millis FROM agent_runs WHERE id = $1`, created.ID).Scan(&atQueue); err != nil || atQueue != 500 {
		t.Fatalf("reserved at the queue = %d (%v), want the pool's default 500", atQueue, err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE runner_pools SET limits = jsonb_set(limits, '{background,cpu_millis,default}', '1500'), revision = revision + 1 WHERE id = $1`, p.ID); err != nil {
		t.Fatal(err)
	}
	e.free(hold)
	e.sweep()
	var got types.AgentRun
	e.eventually("the run to leave the queue", func() bool {
		got = e.run(created.ID)
		return got.State == types.RunRunning || got.State == types.RunFailed || got.State == types.RunCompleted
	})
	e.runner.fakeRunner.mu.Lock()
	spec := e.runner.fakeRunner.lastSpec
	e.runner.fakeRunner.mu.Unlock()
	if got.State == types.RunFailed || spec.RunID != created.ID {
		t.Fatalf("the run did not start: %s %s %s", got.State, got.StatusDetail, got.FailureHint)
	}
	var reserved int64
	if err := e.pool.QueryRow(ctx, `SELECT reserved_cpu_millis FROM agent_runs WHERE id = $1`, created.ID).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if spec.Resources.CPUMillis != 1500 || reserved != spec.Resources.CPUMillis {
		t.Fatalf("the run reserves %d milli-CPU and its sandbox was given %d, want both the 1500 it started with", reserved, spec.Resources.CPUMillis)
	}
}

// A run that asks for no disk is sized by one real runner of the pool and lands on a runner that takes
// that size: two idle runners whose per-run disk differs never refuse it.
func TestPG_DefaultRunLandsOnARunnerThatTakesItsDisk(t *testing.T) {
	e := newPlacementEnv(t)
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	a := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	b := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	for _, id := range []uuid.UUID{a, b} {
		e.addToPool(e.person, laptops.ID, id)
		e.stream(id, types.CC1, types.CC2)
	}
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE runners SET run_disk_mib = CASE WHEN id = $1 THEN 10240 ELSE 20480 END WHERE id = ANY($2)`, a, []uuid.UUID{a, b}); err != nil {
		t.Fatal(err)
	}
	limits := map[uuid.UUID]int64{a: 10240, b: 20480}
	for i := range 16 {
		run, code, refused := e.launchStored(runBody(`"runner_pool_id":"` + laptops.ID.String() + `"`))
		if code != http.StatusCreated || run.RunnerID == nil {
			t.Fatalf("default launch %d = %d %s, want it placed: both runners are idle", i, code, refused)
		}
		var disk int64
		if err := e.pool.QueryRow(ctx, `SELECT reserved_disk_mib FROM agent_runs WHERE id = $1`, run.ID).Scan(&disk); err != nil {
			t.Fatal(err)
		}
		if disk < 1 || disk > limits[*run.RunnerID] {
			t.Fatalf("launch %d on %s reserves %d MiB disk, past that runner's per-run disk %d", i, run.RunnerID, disk, limits[*run.RunnerID])
		}
	}
}

// The size a run is cut to is one a real runner takes whole. With a 2-CPU/1-GiB runner and a
// 1-CPU/4-GiB runner, an ask of 2 CPUs and 4 GiB is reduced to one of them and placed there, never
// refused for a size built from one runner's CPU and the other's memory.
func TestPG_OverAskIsCutToASizeSomeRunnerTakes(t *testing.T) {
	e := newPlacementEnv(t)
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	a := e.laptop(e.personP, 2000, 1024, types.CC1, types.CC2)
	b := e.laptop(e.personP, 1000, 4096, types.CC1, types.CC2)
	for _, id := range []uuid.UUID{a, b} {
		e.addToPool(e.person, laptops.ID, id)
		e.stream(id, types.CC1, types.CC2)
	}
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE runners SET run_disk_mib = 10240 WHERE id = ANY($1)`, []uuid.UUID{a, b}); err != nil {
		t.Fatal(err)
	}
	e.assignSizedProfile("eight", &types.ResourceLimits{CPUMillis: 8000, MemoryMiB: 16384}, types.GovernanceLimits{})
	body := runBody(fmt.Sprintf(`"runner_pool_id":%q,"resources":{"cpu_millis":2000,"memory_mib":4096}`, laptops.ID))
	if pf := e.call(e.person, http.MethodPost, "/api/v1/runs/preflight", body); pf.Code != http.StatusOK {
		t.Fatalf("preflight = %d %s, want the ask reduced, never refused", pf.Code, pf.Body)
	}
	run, code, refused := e.launchStored(body)
	if code != http.StatusCreated || run.RunnerID == nil {
		t.Fatalf("an ask no one runner takes whole = %d %s, want it cut to a size one of them takes and placed", code, refused)
	}
	var cpu, mem int64
	if err := e.pool.QueryRow(ctx, `SELECT reserved_cpu_millis, reserved_memory_mib FROM agent_runs WHERE id = $1`, run.ID).Scan(&cpu, &mem); err != nil {
		t.Fatal(err)
	}
	want := map[uuid.UUID][2]int64{a: {2000, 1024}, b: {1000, 4096}}[*run.RunnerID]
	if cpu > want[0] || mem > want[1] {
		t.Fatalf("placed on %s reserving %d m / %d MiB, past its per-run %d m / %d MiB", run.RunnerID, cpu, mem, want[0], want[1])
	}
	// An ask one runner takes whole is never cut for another's sake.
	whole, code, refused := e.launchStored(runBody(fmt.Sprintf(`"runner_pool_id":%q,"resources":{"cpu_millis":1000,"memory_mib":4096}`, laptops.ID)))
	if code != http.StatusCreated || whole.RunnerID == nil || *whole.RunnerID != b {
		t.Fatalf("an ask runner b takes whole = %d %s on %v, want it on b uncut", code, refused, whole.RunnerID)
	}
	if err := e.pool.QueryRow(ctx, `SELECT reserved_cpu_millis, reserved_memory_mib FROM agent_runs WHERE id = $1`, whole.ID).Scan(&cpu, &mem); err != nil || cpu != 1000 || mem != 4096 {
		t.Fatalf("an ask runner b takes whole reserves %d m / %d MiB (%v), want 1000 / 4096", cpu, mem, err)
	}
}

// Every pool can queue: in a mixed pool whose remote-hosted runner is full, a launch waits for room
// whether the person's own runner is full too or simply offline. An offline laptop does not turn the
// wait into a refusal.
func TestPG_MixedPoolFullRemoteWaits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(e *placementEnv, pool types.RunnerPool, laptop uuid.UUID)
	}{
		{"laptop full", func(e *placementEnv, pool types.RunnerPool, laptop uuid.UUID) {
			e.maxRuns(laptop, e.personP, 1)
			e.occupyRunner(pool, laptop, e.personP)
		}},
		{"laptop offline", func(e *placementEnv, _ types.RunnerPool, laptop uuid.UUID) { e.online[laptop] = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newPlacementEnv(t)
			pool, laptop := e.mixedPool("Mixed")
			tc.prepare(e, pool, laptop)
			body := runBody(`"runner_pool_id":"` + pool.ID.String() + `"`)
			// With room on the remote-hosted runner the run is placed there.
			ctl, code, refused := e.launchStored(body)
			if code != http.StatusCreated || ctl.RunnerPoolExecutor != "alpha" {
				t.Fatalf("launch with room on alpha = %d %s on %q", code, refused, ctl.RunnerPoolExecutor)
			}
			if _, err := e.pool.Exec(context.Background(), `UPDATE agent_runs SET state = 'COMPLETED' WHERE id = $1`, ctl.ID); err != nil {
				t.Fatal(err)
			}
			occupyRemote(e, pool, "alpha")
			w := e.call(e.person, http.MethodPost, "/api/v1/runs", body)
			if w.Code != http.StatusCreated || decodeJSON[types.AgentRun](t, w).State != types.RunQueued {
				t.Fatalf("launch with the remote-hosted runner full = %d %s; want 201 QUEUED", w.Code, w.Body)
			}
		})
	}
}
