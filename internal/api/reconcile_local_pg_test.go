// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run bound to a person's own runner that the runner never received (wardynd stopped between the
// create and the dispatch) is finalized by the undispatched reaper like any other run never dispatched,
// freeing its runner's place. One the runner did receive is left to the runner's own exit receipt.
func TestPG_UndispatchedLocalRunIsFinalizedOnlyWhenItsRunnerNeverHadIt(t *testing.T) {
	e := newPlacementEnv(t)
	ctx := context.Background()
	laptop := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	stranded := func() uuid.UUID {
		t.Helper()
		id, at := uuid.New(), time.Now().UTC().Add(-undispatchedGrace-time.Minute)
		if _, err := e.pg.CreateRun(ctx, types.AgentRun{ID: id, CreatedAt: at, UpdatedAt: at, CreatedBy: e.personP, Agent: "claude-code",
			ConfinementClass: types.CC2, State: types.RunPending, SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker",
			Placement: types.PlacementLocal, EvidenceSource: types.RunEvidenceRunnerAsserted, RunnerID: &laptop}); err != nil {
			t.Fatal(err)
		}
		// No live process holds it: the one that created it is gone.
		if _, err := e.pool.Exec(ctx, `UPDATE agent_runs SET watcher_heartbeat = $2 WHERE id = $1`, id, at); err != nil {
			t.Fatal(err)
		}
		return id
	}
	neverHad, received := stranded(), stranded()
	if _, err := e.pool.Exec(ctx, `INSERT INTO runner_executions(run_id, runner_id, generation, operation) VALUES ($1, $2, $3, 'exec')`,
		received, laptop, uuid.New()); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.finalizeUndispatchedRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.pg.GetRun(ctx, neverHad); got.State != types.RunFailed {
		t.Errorf("a local run its runner never received is %s past the dispatch grace, want FAILED", got.State)
	}
	if got, _ := e.pg.GetRun(ctx, received); got.State != types.RunPending {
		t.Errorf("a local run its runner received is %s, want it left PENDING for the runner's exit receipt", got.State)
	}
}

// offlineRunner answers every agent probe as a runner that is offline does.
type offlineRunner struct{ runner.Runner }

func (offlineRunner) AgentStatus(context.Context, string, string) (runner.Status, error) {
	return runner.Status{}, runner.ErrRunnerOffline
}

// An interactive run on a person's own runner has no task, so no execution row, only its sandbox on
// the runner. With the runner offline past the watcher's probe-error ceiling (a closed laptop) the
// watcher gives up and finalizes, and the run must be left for its runner all the same.
func TestPG_InteractiveLocalRunSurvivesItsRunnerBeingOffline(t *testing.T) {
	e := newPlacementEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	laptop := e.laptop(e.personP, 4000, 8192, types.CC1, types.CC2)
	id, at := uuid.New(), time.Now().UTC()
	ref := "runner:" + laptop.String() + ":sandbox"
	if _, err := e.pg.CreateRun(ctx, types.AgentRun{ID: id, CreatedAt: at, UpdatedAt: at, CreatedBy: e.personP, Agent: "claude-code",
		ConfinementClass: types.CC2, State: types.RunRunning, SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker",
		Placement: types.PlacementLocal, EvidenceSource: types.RunEvidenceRunnerAsserted, RunnerID: &laptop, Interactive: true, SandboxRef: ref}); err != nil {
		t.Fatal(err)
	}
	e.srv.cfg.Runner = offlineRunner{Runner: e.srv.cfg.Runner}
	// Each look at the clock is 16 minutes on: the second failed probe is past the 30-minute ceiling.
	var looks atomic.Int64
	e.srv.cfg.Now = func() time.Time { return at.Add(time.Duration(looks.Add(1)) * 16 * time.Minute) }
	prev := reconcileWatchIntervalNS.Swap(int64(time.Millisecond))
	defer reconcileWatchIntervalNS.Store(prev)
	if e.srv.reconcileWatch(ctx, id, ref, ""); ctx.Err() != nil {
		t.Fatal("the watcher never reached its probe-error ceiling")
	}
	got, err := e.pg.GetRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != types.RunRunning {
		t.Errorf("an interactive run on the person's own runner is %s (%q) after its runner was offline, want it left RUNNING", got.State, got.FailureHint)
	}
}
