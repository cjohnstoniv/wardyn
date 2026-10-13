// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A runner at its runs at once takes no further run: in a pool with no cap the launch and the dry doors
// refuse runner_at_capacity, and a run waiting in a pool's queue is held with its own wait reason.
// Raising the limit lets the next run in.
func TestPG_RunnerAtItsMaxRunsRefusesOrWaits(t *testing.T) {
	e := newPlacementEnv(t)
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	awake := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	e.addToPool(e.person, laptops.ID, awake)
	e.stream(awake, types.CC1, types.CC2)
	e.personDefaults(e.person, fmt.Sprintf(`{"pool_id":%q}`, laptops.ID))
	if _, err := e.pool.Exec(context.Background(), `UPDATE runners SET max_runs = 1 WHERE id = $1`, awake); err != nil {
		t.Fatal(err)
	}

	first := e.localCreate(runBody(""))
	if first.RunnerID == nil || *first.RunnerID != awake {
		t.Fatalf("first run bound to %v, want %s", first.RunnerID, awake)
	}
	for _, door := range []string{"/api/v1/runs", "/api/v1/runs/preflight"} {
		w := e.call(e.person, http.MethodPost, door, runBody(""))
		if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), `"reason":"`+string(placement.ReasonRunnerAtCapacity)+`"`) ||
			!strings.Contains(w.Body.String(), runnerpool.RunnerAtCapacityMsg("Laptops")) {
			t.Fatalf("%s on a full runner = %d %s, want 422 runner_at_capacity", door, w.Code, w.Body)
		}
	}

	room, err := e.srv.queueRunnerFree(withOIDCHuman(context.Background(), e.personP), laptops, queuedLaunch{Request: []byte(runBody(""))})
	if err != nil || room.wait != runnerpool.WaitRunnersAtCapacity || len(room.runners) != 0 {
		t.Fatalf("the queue's verdict on a full runner = %+v, %v; want it held for runners_at_capacity", room, err)
	}

	if w := e.call(e.person, http.MethodPut, "/api/v1/me/runners/"+awake.String(), roomyLimitsBody(2)); w.Code != http.StatusOK {
		t.Fatalf("owner raise = %d %s", w.Code, w.Body)
	}
	if second := e.localCreate(runBody("")); second.RunnerID == nil || *second.RunnerID != awake {
		t.Fatalf("after the raise the run went to %v", second.RunnerID)
	}
}

// cappedLaptops is a capped self-hosted pool of the person's with two online runners a and b, each
// allowed maxRuns at once, set as their default.
func cappedLaptops(e *queueEnv, maxRuns int) (types.RunnerPool, uuid.UUID, uuid.UUID) {
	e.t.Helper()
	laptops := e.createPool("Laptops", types.RunnerPoolUser)
	a := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	b := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	for _, id := range []uuid.UUID{a, b} {
		e.addToPool(e.person, laptops.ID, id)
		e.stream(id, types.CC1, types.CC2)
	}
	e.personDefaults(e.person, `{"pool_id":"`+laptops.ID.String()+`"}`)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `UPDATE runners SET max_runs = $2 WHERE id = ANY($1)`, []uuid.UUID{a, b}, maxRuns); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE runner_pools SET limits = '{"background":{"cpu_millis":{"default":500,"cap":4000},"memory_mib":{"default":512,"cap":8192},"lifetime":{"default_sec":3600,"max_sec":7200}},"barriers":["CC1","CC2"],"max_concurrent_runs":5}'::jsonb, revision = revision + 1 WHERE id = $1`, laptops.ID); err != nil {
		e.t.Fatal(err)
	}
	return e.reread(laptops), a, b
}

// occupyRunner puts a RUNNING run on runner in p, holding one of its places.
func occupyRunner(e *queueEnv, p types.RunnerPool, runner uuid.UUID) {
	e.t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	if _, err := e.pg.CreateRun(context.Background(), types.AgentRun{ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: e.personP, Agent: "claude-code",
		ConfinementClass: types.CC2, State: types.RunRunning, SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker",
		RunnerID: &runner, RunnerPoolID: &p.ID, RunnerPoolRevision: p.Revision, RunnerPoolSelection: types.RunnerPoolSelectedExplicit}); err != nil {
		e.t.Fatalf("occupy runner: %v", err)
	}
}

// launchThroughQueue launches a run into the capped pool, sweeps the queue, and returns the runner the
// run's own pick lands on (runnerpool.Select over the run id and runners, what bind chooses when it
// prefers none) and the run once it left the queue. The run is then ended, so it frees its place.
func launchThroughQueue(e *queueEnv, p types.RunnerPool, runners ...uuid.UUID) (*uuid.UUID, types.AgentRun) {
	e.t.Helper()
	w := e.call(e.person, http.MethodPost, "/api/v1/runs", endBody("background", `"runner_pool_id":"`+p.ID.String()+`"`))
	if w.Code != http.StatusCreated {
		e.t.Fatalf("launch = %d %s", w.Code, w.Body)
	}
	created := decodeJSON[types.AgentRun](e.t, w)
	ids := make([]string, len(runners))
	for i, id := range runners {
		ids[i] = id.String()
	}
	pick := uuid.MustParse(runnerpool.Select(p.ID, created.ID, runnerpool.SortedUnique(ids)))
	atCreate := &pick
	e.sweep()
	var got types.AgentRun
	e.eventually("the run to leave the queue", func() bool {
		got = e.run(created.ID)
		return got.State == types.RunRunning || got.State == types.RunFailed || got.State == types.RunCompleted
	})
	if got.State != types.RunCompleted {
		if ok, err := e.srv.casRunState(context.Background(), got.ID, got.State, types.RunCompleted); err != nil || !ok {
			e.t.Fatalf("end %s from %s: %v %v", got.ID, got.State, ok, err)
		}
	}
	return atCreate, got
}

// A queued run starts on the runner its promotion found room on and is never ended for one full runner
// while another of its runners has room (review r1 H1). With runner a full, every run starts on b; with
// both having room, the start keeps the promotion's choice (the roomier b) even where the run's own pick
// is a.
func TestPG_QueuedRunStartsOnTheRunnerItsPromotionChose(t *testing.T) {
	e := newQueueEnv(t)
	p, a, b := cappedLaptops(e, 1)
	occupyRunner(e, p, a)
	sawA := false
	for i := 0; i < 16 && !sawA; i++ {
		atCreate, got := launchThroughQueue(e, p, a, b)
		sawA = atCreate != nil && *atCreate == a
		if got.State == types.RunFailed || got.RunnerID == nil || *got.RunnerID != b {
			t.Fatalf("launch %d (own pick %v) = %s on %v detail %q; want it started on the runner with room %s", i, atCreate, got.State, got.RunnerID, got.StatusDetail, b)
		}
	}
	if !sawA {
		t.Fatal("no launch's own pick was the full runner, so the path was not exercised")
	}

	if _, err := e.pool.Exec(context.Background(), `UPDATE runners SET max_runs = 2 WHERE id = ANY($1)`, []uuid.UUID{a, b}); err != nil {
		t.Fatal(err)
	}
	sawA = false
	for i := 0; i < 16 && !sawA; i++ {
		atCreate, got := launchThroughQueue(e, p, a, b)
		if atCreate == nil || *atCreate != a {
			continue
		}
		sawA = true
		if got.RunnerID == nil || *got.RunnerID != b {
			t.Fatalf("a run whose own pick is a, promoted to the roomier b, started on %v; want the promotion's b", got.RunnerID)
		}
	}
	if !sawA {
		t.Fatal("no launch's own pick was a, so the kept binding was not exercised")
	}
}

// roomyLimitsBody is an owner's runner update declaring roomy limits with runs at once.
func roomyLimitsBody(runs int) string {
	l := roomyRunnerLimits()
	l.RunsAtOnce = runs
	b, _ := json.Marshal(types.RunnerUpdateRequest{Limits: l})
	return string(b)
}
