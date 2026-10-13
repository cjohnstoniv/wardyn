// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
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

// The queue offers a waiting run only the runners its start would keep. Two of a person's runners: a
// enforces Wall and is full, b enforces only Fence and is idle. A Wall launch waits for a; it is never
// promoted onto b and ended there, and it starts on a once a has room.
func TestPG_WaitingRunIsNotEndedForARunnerThatCannotEnforceItsBarrier(t *testing.T) {
	e := newQueueEnv(t)
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	a := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	b := e.laptop(e.personP, 4000, 8192, types.CC1)
	for _, id := range []uuid.UUID{a, b} {
		e.addToPool(e.person, laptops.ID, id)
	}
	e.stream(a, types.CC1, types.CC2)
	e.stream(b, types.CC1)
	e.maxRuns(a, e.personP, 1)
	body := runBody(fmt.Sprintf(`"confinement_class":"CC2","runner_pool_id":%q`, laptops.ID))
	launch := func() types.AgentRun {
		t.Helper()
		w := e.call(e.person, http.MethodPost, "/api/v1/runs", body)
		if w.Code != http.StatusCreated {
			t.Fatalf("launch = %d %s", w.Code, w.Body)
		}
		return decodeJSON[types.AgentRun](t, w)
	}
	first := launch()
	if first.RunnerID == nil || *first.RunnerID != a || first.ConfinementClass != types.CC2 {
		t.Fatalf("with room on a the launch is on %v at %s, want a at CC2", first.RunnerID, first.ConfinementClass)
	}
	waiting := launch()
	got := e.settles(waiting.ID)
	if got.State != types.RunQueued || got.StatusDetail != statusDetailQueuePrefix+string(runnerpool.WaitRunnersAtCapacity) {
		t.Fatalf("with a full the Wall launch is %s (%q) on %v; want it QUEUED for a, the only runner that enforces Wall", got.State, got.StatusDetail, got.RunnerID)
	}
	if ok, err := e.srv.casRunState(context.Background(), first.ID, e.run(first.ID).State, types.RunCompleted); err != nil || !ok {
		t.Fatalf("end the first run: %v %v", ok, err)
	}
	if got = e.settles(waiting.ID); got.State != types.RunRunning || got.RunnerID == nil || *got.RunnerID != a || got.ConfinementClass != types.CC2 {
		t.Fatalf("once a has room the run is %s on %v at %s (%s); want it RUNNING on a at CC2", got.State, got.RunnerID, got.ConfinementClass, got.StatusDetail)
	}
}

// No drive mounts on a person's own runner, so a run with a drive goes to the pool's remote-hosted runner
// alone. With that runner full and the laptop idle it waits; it is never promoted onto the laptop and
// ended, and it starts on the remote-hosted runner once that has room.
func TestPG_WaitingRunWithADriveIsNotEndedForThePersonsOwnRunner(t *testing.T) {
	e := newQueueEnv(t)
	ctx := context.Background()
	pool, laptop := e.mixedPool("Mixed")
	drive, err := e.pg.UpsertUserDrive(ctx, *driveFixture(nil), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pg.UpsertUserDriveGrant(ctx, *grantFixture(drive.ID, func(g *types.UserDriveGrant) { g.Subject = e.personP }), false); err != nil {
		t.Fatal(err)
	}
	occupyRemote(e.placementEnv, pool, "alpha")
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", runBody(fmt.Sprintf(`"drive":{"enabled":true},"runner_pool_id":%q`, pool.ID)))
	if w.Code != http.StatusCreated {
		t.Fatalf("launch with a drive = %d %s", w.Code, w.Body)
	}
	waiting := decodeJSON[types.AgentRun](t, w)
	got := e.settles(waiting.ID)
	if got.State != types.RunQueued || got.StatusDetail != statusDetailQueuePrefix+string(runnerpool.WaitRunnersAtCapacity) {
		t.Fatalf("with alpha full the run with a drive is %s (%q) on runner %v; want it QUEUED for alpha: no drive mounts on the laptop %s",
			got.State, got.StatusDetail, got.RunnerID, laptop)
	}
	var occupant uuid.UUID
	if err := e.pool.QueryRow(ctx, `SELECT id FROM agent_runs WHERE runner_pool_executor = 'alpha' AND state = 'RUNNING'`).Scan(&occupant); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.srv.casRunState(ctx, occupant, types.RunRunning, types.RunCompleted); err != nil || !ok {
		t.Fatalf("end alpha's run: %v %v", ok, err)
	}
	if got = e.settles(waiting.ID); got.State != types.RunRunning || got.RunnerID != nil || got.RunnerPoolExecutor != "alpha" {
		t.Fatalf("once alpha has room the run is %s on runner %v executor %q (%s); want it RUNNING on alpha", got.State, got.RunnerID, got.RunnerPoolExecutor, got.StatusDetail)
	}
}
