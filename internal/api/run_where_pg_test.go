// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// GET /runs/{id} names where the run ran, against a real Postgres. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (e *poolEnv) boundRun(bind func(*types.AgentRun)) uuid.UUID {
	e.t.Helper()
	id, now := uuid.New(), time.Now().UTC()
	run := types.AgentRun{
		ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: e.personP, Agent: "claude-code", ConfinementClass: types.CC2,
		State: types.RunCompleted, SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker",
	}
	bind(&run)
	if _, err := e.pg.CreateRun(context.Background(), run); err != nil {
		e.t.Fatalf("create run: %v", err)
	}
	return id
}

func (e *poolEnv) runDetail(id uuid.UUID) map[string]any {
	e.t.Helper()
	w := e.admin(http.MethodGet, "/api/v1/runs/"+id.String(), "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("GET run = %d %s", w.Code, w.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		e.t.Fatal(err)
	}
	return m
}

// The run page names the pool and the runner a run was bound to, read at request time: a self-hosted runner,
// a remote-hosted runner, and a pool deleted since (its row is a tombstone, so it still resolves). A run bound
// to neither says nothing, and the ids stay on the wire.
func TestPG_RunDetailNamesThePoolAndTheRunner(t *testing.T) {
	e := newPoolEnv(t, false)
	ctx := context.Background()
	farm := e.createPool("Build farm", types.RunnerPoolOrganization)
	old := e.createPool("Old farm", types.RunnerPoolOrganization)
	mine := e.createOwnPool(e.person, "Laptops")
	laptop := e.claimedRunner(e.personP)
	name := "Cluster A"
	if _, _, err := e.pg.UpdateRemoteRunner(ctx, "fake", e.remoteRunner("fake").Revision, &name, nil, "admin"); err != nil {
		t.Fatal(err)
	}

	local := e.boundRun(func(r *types.AgentRun) {
		r.Placement, r.RunnerID, r.RunnerPoolID, r.RunnerPoolRevision, r.RunnerPoolSelection = types.PlacementLocal, &laptop, &mine.ID, 1, types.RunnerPoolSelectedExplicit
	})
	remote := e.boundRun(func(r *types.AgentRun) {
		r.Placement, r.RunnerPoolExecutor, r.RunnerPoolID, r.RunnerPoolRevision, r.RunnerPoolSelection = types.PlacementRemote, "fake", &farm.ID, 1, types.RunnerPoolSelectedExplicit
	})
	gone := e.boundRun(func(r *types.AgentRun) {
		r.Placement, r.RunnerPoolExecutor, r.RunnerPoolID, r.RunnerPoolRevision, r.RunnerPoolSelection = types.PlacementRemote, "fake", &old.ID, 1, types.RunnerPoolSelectedExplicit
	})
	if w := e.admin(http.MethodDelete, "/api/v1/runner-pools/"+old.ID.String(), ""); w.Code >= 300 {
		t.Fatalf("delete pool = %d %s", w.Code, w.Body)
	}
	unbound := e.boundRun(func(r *types.AgentRun) { r.Placement = types.PlacementRemote })

	for _, c := range []struct {
		what       string
		id         uuid.UUID
		pool, name string
	}{
		{"a self-hosted runner in a user pool", local, "Laptops", "laptop"},
		{"a remote-hosted runner in an organization pool", remote, "Build farm", "Cluster A"},
		{"a pool deleted since", gone, "Old farm", "Cluster A"},
		{"a run bound to neither", unbound, "", ""},
	} {
		got := e.runDetail(c.id)
		pool, hasPool := got["runner_pool_name"].(string)
		runnerName, hasName := got["runner_name"].(string)
		if pool != c.pool || hasPool != (c.pool != "") || runnerName != c.name || hasName != (c.name != "") {
			t.Errorf("%s: runner_pool_name %q (%v), runner_name %q (%v); want %q and %q, omitted when empty", c.what, pool, hasPool, runnerName, hasName, c.pool, c.name)
		}
	}
	if got := e.runDetail(remote); got["remote_runner_id"] != "fake" || got["runner_pool_id"] != farm.ID.String() {
		t.Errorf("the ids left the wire: %v", got)
	}
}
