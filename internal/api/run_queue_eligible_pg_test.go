// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// settles sweeps the queue and returns the run once it is no longer mid-start: still waiting, or past
// PENDING.
func (e *queueEnv) settles(id uuid.UUID) types.AgentRun {
	e.t.Helper()
	e.sweep()
	var got types.AgentRun
	e.eventually("run "+id.String()+" to settle", func() bool {
		got = e.run(id)
		return got.State != types.RunPending && got.State != types.RunStarting
	})
	return got
}

// endOccupant ends the run a test inserted RUNNING on runner (a self-hosted runner's id, or a
// remote-hosted runner's), as the server ends any run, so the freed place kicks the queue. The occupant
// is inserted, never launched: no dispatch moves its state under the swap.
func (e *queueEnv) endOccupant(runner string) {
	e.t.Helper()
	ctx := context.Background()
	var id uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT id FROM agent_runs WHERE state = 'RUNNING' AND $1 IN (runner_id::text, runner_pool_executor)`, runner).Scan(&id); err != nil {
		e.t.Fatalf("the occupant of %s: %v", runner, err)
	}
	if ok, err := e.srv.casRunState(ctx, id, types.RunRunning, types.RunCompleted); err != nil || !ok {
		e.t.Fatalf("end the occupant of %s: %v %v", runner, ok, err)
	}
}

// waitingLaunch is the run_queue launch stored for a waiting run.
func (e *queueEnv) waitingLaunch(id uuid.UUID) queuedLaunch {
	e.t.Helper()
	var raw json.RawMessage
	var launch queuedLaunch
	if err := e.pool.QueryRow(context.Background(), `SELECT launch FROM run_queue WHERE run_id = $1`, id).Scan(&raw); err != nil || json.Unmarshal(raw, &launch) != nil {
		e.t.Fatalf("the queued launch of %s: %v %s", id, err, raw)
	}
	return launch
}

// waitsFor launches body, which must wait, and returns the run still QUEUED for its runners after a sweep.
func (e *queueEnv) waitsFor(body, why string) types.AgentRun {
	e.t.Helper()
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", body)
	if w.Code != http.StatusCreated {
		e.t.Fatalf("launch = %d %s", w.Code, w.Body)
	}
	got := e.settles(decodeJSON[types.AgentRun](e.t, w).ID)
	if got.State != types.RunQueued || got.StatusDetail != statusDetailQueuePrefix+string(runnerpool.WaitRunnersAtCapacity) {
		e.t.Fatalf("the launch is %s (%q) on runner %v executor %q; want it QUEUED for its runners: %s", got.State, got.StatusDetail, got.RunnerID, got.RunnerPoolExecutor, why)
	}
	return got
}

// wallAndFence is a pool of two of the person's runners: a enforces Wall, takes one run and is full with
// an inserted run; b enforces only Fence and is idle.
func wallAndFence(e *queueEnv) (types.RunnerPool, uuid.UUID, uuid.UUID) {
	e.t.Helper()
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	a := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	b := e.laptop(e.personP, 4000, 8192, types.CC1)
	for _, id := range []uuid.UUID{a, b} {
		e.addToPool(e.person, laptops.ID, id)
	}
	e.stream(a, types.CC1, types.CC2)
	e.stream(b, types.CC1)
	e.maxRuns(a, e.personP, 1)
	laptops = e.reread(laptops)
	occupyRunner(e, laptops, a)
	return laptops, a, b
}

// The queue offers a waiting run only the runners its start would keep. A launch that asks for Wall waits
// for a, the one runner that enforces it; it is never promoted onto the idle b and ended there, and it
// starts on a once a has room.
func TestPG_WaitingRunIsNotEndedForARunnerThatCannotEnforceItsBarrier(t *testing.T) {
	e := newQueueEnv(t)
	laptops, a, _ := wallAndFence(e)
	waiting := e.waitsFor(runBody(fmt.Sprintf(`"confinement_class":"CC2","runner_pool_id":%q`, laptops.ID)), "a is the only runner that enforces Wall")
	e.endOccupant(a.String())
	if got := e.settles(waiting.ID); got.State != types.RunRunning || got.RunnerID == nil || *got.RunnerID != a || got.ConfinementClass != types.CC2 {
		t.Fatalf("once a has room the run is %s on %v at %s (%s); want it RUNNING on a at CC2", got.State, got.RunnerID, got.ConfinementClass, got.StatusDetail)
	}
}

// The barrier the queue holds a runner to is the one the run was queued at, not the one its request
// names: a launch that names no barrier under a policy whose floor is Wall records Wall, waits for a, and
// is never promoted onto b.
func TestPG_WaitingRunThatNamesNoBarrierWaitsForItsPolicyFloor(t *testing.T) {
	e := newQueueEnv(t)
	laptops, a, _ := wallAndFence(e)
	waiting := e.waitsFor(runBody(fmt.Sprintf(`"inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"]},"runner_pool_id":%q`, laptops.ID)),
		"a is the only runner that enforces the policy's Wall floor")
	if launch := e.waitingLaunch(waiting.ID); launch.Barrier != types.CC2 || launch.ConfinementDefault == "" {
		t.Fatalf("the launch records barrier %q (default %q), want the Wall it was queued at, chosen for it", launch.Barrier, launch.ConfinementDefault)
	}
	e.endOccupant(a.String())
	if got := e.settles(waiting.ID); got.State != types.RunRunning || got.RunnerID == nil || *got.RunnerID != a || got.ConfinementClass != types.CC2 {
		t.Fatalf("once a has room the run is %s on %v at %s (%s); want it RUNNING on a at CC2", got.State, got.RunnerID, got.ConfinementClass, got.StatusDetail)
	}
}

// No drive mounts on a person's own runner, so a run with a drive goes to the pool's remote-hosted runner
// alone. With that runner full and the laptop idle it waits; it is never promoted onto the laptop and
// ended, and it starts on the remote-hosted runner once that has room.
func TestPG_WaitingRunWithADriveIsNotEndedForThePersonsOwnRunner(t *testing.T) {
	e := newQueueEnv(t)
	ctx := context.Background()
	pool, _ := e.mixedPool("Mixed")
	drive, err := e.pg.UpsertUserDrive(ctx, *driveFixture(nil), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pg.UpsertUserDriveGrant(ctx, *grantFixture(drive.ID, func(g *types.UserDriveGrant) { g.Subject = e.personP }), false); err != nil {
		t.Fatal(err)
	}
	occupyRemote(e.placementEnv, pool, "alpha")
	waiting := e.waitsFor(runBody(fmt.Sprintf(`"drive":{"enabled":true},"runner_pool_id":%q`, pool.ID)), "no drive mounts on the idle laptop")
	e.endOccupant("alpha")
	if got := e.settles(waiting.ID); got.State != types.RunRunning || got.RunnerID != nil || got.RunnerPoolExecutor != "alpha" {
		t.Fatalf("once alpha has room the run is %s on runner %v executor %q (%s); want it RUNNING on alpha", got.State, got.RunnerID, got.RunnerPoolExecutor, got.StatusDetail)
	}
}

// bothFull is a mixed pool whose laptop (one run at once) and remote-hosted runner alpha are each full
// with an inserted run.
func bothFull(e *queueEnv) (types.RunnerPool, uuid.UUID) {
	e.t.Helper()
	pool, laptop := e.mixedPool("Mixed")
	e.maxRuns(laptop, e.personP, 1)
	e.stream(laptop, types.CC1, types.CC2, types.CC3)
	pool = e.reread(pool)
	e.occupyRunner(pool, laptop, e.personP)
	occupyRemote(e.placementEnv, pool, "alpha")
	return pool, laptop
}

// A run accepted to wait for the remote-hosted runner is never ended because a runner it cannot use
// became free. On a deployment whose upstream proxy credential cannot go to a person's runner, the
// local-eligibility step refuses every run on the laptop: the launch waits on alpha, keeps waiting when
// the laptop frees first, and starts on alpha once alpha has room though the laptop is idle.
func TestPG_WaitingRunIsNotEndedWhenARunnerItCannotUseFrees(t *testing.T) {
	e := newQueueEnv(t)
	if _, err := e.pg.PutSiteConfig(context.Background(), types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}, UpstreamProxySecretRef: "corporate-parent"}); err != nil {
		t.Fatal(err)
	}
	pool, laptop := bothFull(e)
	waiting := e.waitsFor(runBody(fmt.Sprintf(`"runner_pool_id":%q`, pool.ID)), "both runners are full")
	if launch := e.waitingLaunch(waiting.ID); !launch.NoLocal || waiting.Placement != types.PlacementRemote {
		t.Fatalf("the launch waits on %q and records no_local %v, want it waiting on the remote-hosted kind and kept off the person's own", waiting.Placement, launch.NoLocal)
	}
	e.endOccupant(laptop.String())
	if got := e.settles(waiting.ID); got.State != types.RunQueued {
		t.Fatalf("with the laptop freed and alpha full the run is %s (%s: %s); want it still QUEUED for alpha", got.State, got.StatusDetail, got.FailureHint)
	}
	e.endOccupant("alpha")
	if got := e.settles(waiting.ID); got.State != types.RunRunning || got.RunnerID != nil || got.RunnerPoolExecutor != "alpha" {
		t.Fatalf("once alpha has room the run is %s on runner %v executor %q (%s: %s); want it RUNNING on alpha", got.State, got.RunnerID, got.RunnerPoolExecutor, got.StatusDetail, got.FailureHint)
	}
}

// The mirror: a run that can run on the person's own runner is still promoted onto the laptop when it
// frees first.
func TestPG_WaitingRunThatCanRunLocallyTakesTheFreedLaptop(t *testing.T) {
	e := newQueueEnv(t)
	pool, laptop := bothFull(e)
	waiting := e.waitsFor(runBody(fmt.Sprintf(`"runner_pool_id":%q`, pool.ID)), "both runners are full")
	if launch := e.waitingLaunch(waiting.ID); launch.NoLocal {
		t.Fatal("a launch its own runner can take records no_local")
	}
	e.endOccupant(laptop.String())
	if got := e.settles(waiting.ID); got.State != types.RunRunning || got.RunnerID == nil || *got.RunnerID != laptop {
		t.Fatalf("with the laptop freed the run is %s on runner %v executor %q (%s); want it RUNNING on the laptop", got.State, got.RunnerID, got.RunnerPoolExecutor, got.FailureHint)
	}
}
