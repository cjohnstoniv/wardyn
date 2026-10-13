// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

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
