// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (p *poolRunner) executorCalls() []string {
	p.fakeRunner.mu.Lock()
	defer p.fakeRunner.mu.Unlock()
	return append([]string(nil), p.onExecutor...)
}

// DONE WHEN 9: a pooled remote-provided run is created on the executor it was admitted against, once
// and on every retry, whatever happens to the pool, its members, the defaults or the deployment's other
// executors afterwards; it never takes the first-substrate path.
func TestDispatch_ExactTargetFence(t *testing.T) {
	e := newPlacementEnv(t)
	ctx := context.Background()
	pool := e.createPool("Mixed", types.RunnerPoolOrganization)
	e.addExecutors(pool.ID, "alpha", "beta")
	other := e.createPool("Elsewhere", types.RunnerPoolOrganization)
	e.addExecutors(other.ID, "alpha", "beta")
	e.orgDefaults(fmt.Sprintf(`{"pool_id":%q}`, pool.ID))

	w := e.call(e.person, http.MethodPost, "/api/v1/runs", runBody(""))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var created types.AgentRun
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	run, err := e.pg.GetRun(ctx, created.ID)
	if err != nil || run.RunnerPoolExecutor == "" {
		t.Fatalf("persisted run = %+v (%v)", run, err)
	}
	winner := run.RunnerPoolExecutor
	loser := map[string]string{"alpha": "beta", "beta": "alpha"}[winner]
	for deadline := time.Now().Add(5 * time.Second); len(e.runner.executorCalls()) == 0 && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	if got := e.runner.executorCalls(); len(got) != 1 || got[0] != winner {
		t.Fatalf("dispatch created on %v, the run was admitted against %q", got, winner)
	}
	if e.runner.fakeRunner.createCalls != 0 {
		t.Fatalf("a pooled run took the first-substrate path %d times", e.runner.fakeRunner.createCalls)
	}

	// Everything the pool's label could change after admission, changed.
	if w := e.admin(http.MethodDelete, "/api/v1/runner-pools/"+pool.ID.String()+"/remote-runners/"+winner, ""); w.Code >= 300 {
		t.Fatalf("remove the winner from the pool = %d %s", w.Code, w.Body)
	}
	e.orgDefaults(fmt.Sprintf(`{"pool_id":%q}`, other.ID))
	e.personDefaults(e.person, fmt.Sprintf(`{"pool_id":%q}`, other.ID))
	if w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+pool.ID.String(), fmt.Sprintf(`{"revision":%d,"state":"disabled"}`, e.revision(pool.ID))); w.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", w.Code, w.Body)
	}
	delete(e.runner.execFacts, loser)

	stored, err := e.pg.GetRun(ctx, run.ID)
	if err != nil || stored.RunnerPoolExecutor != winner || stored.RunnerPoolID == nil || *stored.RunnerPoolID != pool.ID {
		t.Fatalf("the binding moved: %+v (%v)", stored, err)
	}
	for i := range 3 {
		if _, err := e.srv.createSandboxAtTarget(ctx, stored, runner.SandboxSpec{RunID: run.ID, ConfinementClass: stored.ConfinementClass}); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
	}
	got := e.runner.executorCalls()
	if len(got) != 4 {
		t.Fatalf("calls = %v", got)
	}
	for _, g := range got {
		if g != winner {
			t.Fatalf("a retry went to %q after admission to %q: %v", g, winner, got)
		}
	}

	// A pooled run whose row names no target fails; it is never handed to a substrate to choose.
	broken := stored
	broken.RunnerPoolExecutor = ""
	if _, err := e.srv.createSandboxAtTarget(ctx, broken, runner.SandboxSpec{RunID: run.ID}); !errors.Is(err, runner.ErrExecutorUnavailable) {
		t.Fatalf("a pooled run naming no executor = %v, want ErrExecutorUnavailable", err)
	}
	if e.runner.fakeRunner.createCalls != 0 {
		t.Fatalf("the first-substrate path served a pooled run %d times", e.runner.fakeRunner.createCalls)
	}
}

// A self-hosted run is created on the persisted runner and no other: the pool is not consulted, so another
// member of the pool, or a runner the person adds later, cannot take it.
func TestDispatch_SelfHostedRunGoesToItsPersistedRunner(t *testing.T) {
	e := newPlacementEnv(t)
	ctx := context.Background()
	bound, later := e.claimedRunner(e.personP), uuid.New()
	pool := e.createPool("Laptops", types.RunnerPoolUser)
	poolID := pool.ID
	run := types.AgentRun{ID: uuid.New(), Placement: types.PlacementLocal, RunnerID: &bound, RunnerPoolID: &poolID}
	for range 3 {
		if _, err := e.srv.createSandboxAtTarget(ctx, run, runner.SandboxSpec{RunID: run.ID}); err != nil {
			t.Fatal(err)
		}
		if got := e.runner.fakeRunner.lastSpec.RunnerID; got != bound.String() || got == later.String() {
			t.Fatalf("created for runner %q, the run is bound to %q", got, bound)
		}
		// The create carries the runner's stored runs at once, the runner's own backstop.
		if got := e.runner.fakeRunner.lastSpec.RunnerMaxRuns; got != types.RunnerMaxRunsCeiling {
			t.Fatalf("created with RunnerMaxRuns %d, want the runner's stored %d", got, types.RunnerMaxRunsCeiling)
		}
	}
	// A spec that arrives naming another runner is overwritten by the persisted one.
	if _, err := e.srv.createSandboxAtTarget(ctx, run, runner.SandboxSpec{RunID: run.ID, RunnerID: later.String()}); err != nil || e.runner.fakeRunner.lastSpec.RunnerID != bound.String() {
		t.Fatalf("a spec naming %s was honoured over the binding: %q (%v)", later, e.runner.fakeRunner.lastSpec.RunnerID, err)
	}
	// An unpooled run, as every run was before pools, still goes through the plain path.
	before := e.runner.fakeRunner.createCalls
	if _, err := e.srv.createSandboxAtTarget(ctx, types.AgentRun{ID: uuid.New()}, runner.SandboxSpec{}); err != nil || e.runner.fakeRunner.createCalls != before+1 {
		t.Fatalf("an unpooled run: %v", err)
	}
}

// A run Wardyn launches itself (a sign-in, a probe, a source scan) is placed through no pool, yet it counts
// on the remote-hosted runner it is dispatched to, with the size it is given, so people's runs there are
// never admitted past that runner's limits beside it.
func TestServerRunsCountOnTheRemoteRunner(t *testing.T) {
	fr := &fakeRunner{}
	srv, pool := pgHarnessWithRunner(t, fr)
	ctx := context.Background()
	usage := func() types.RunnerTally {
		used, err := store.NewPG(pool).RunnerUsage(ctx, nil, []string{"fake"})
		if err != nil {
			t.Fatal(err)
		}
		return used["fake"]
	}
	before := usage()
	now := time.Now().UTC()
	run, err := srv.cfg.Store.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: "wardynd", Agent: "claude-code",
		ConfinementClass: types.CC1, State: types.RunPending, SPIFFEID: "spiffe://wardyn.local/agent-run/" + uuid.NewString(), RunnerTarget: "docker"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.createSandboxAtTarget(ctx, run, runner.SandboxSpec{RunID: run.ID, Resources: runner.Resources{CPUMillis: 1500, MemoryMiB: 3072, DiskMiB: 2048}}); err != nil {
		t.Fatal(err)
	}
	after := usage()
	if got, want := after, (types.RunnerTally{Runs: before.Runs + 1, CPUMillis: before.CPUMillis + 1500, MemoryMiB: before.MemoryMiB + 3072, DiskMiB: before.DiskMiB + 2048}); got != want {
		t.Fatalf("the remote-hosted runner's in use = %+v, want %+v: a server run must count", got, want)
	}
	stored, err := srv.cfg.Store.GetRun(ctx, run.ID)
	if err != nil || stored.RunnerPoolExecutor != "fake" || stored.RunnerPoolID != nil {
		t.Fatalf("stored = %+v, %v; want it on fake and in no pool", stored, err)
	}
}
