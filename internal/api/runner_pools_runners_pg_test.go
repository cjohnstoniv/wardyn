// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A pool's runners and a runner's pools show membership both ways, and never across people: even a
// membership row that put another person's runner in a pool shows in neither read, and another person
// reads the pool's runners as an unknown id.
func TestPG_PoolRunnersAndRunnerPoolsNeverCrossPeople(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createOwnPool(e.person, "Laptop")
	mine := e.claimedRunner(e.personP)
	if w := e.call(e.person, http.MethodPut, addRunnerPath(pool.ID, mine), ""); w.Code != http.StatusCreated {
		t.Fatalf("own add = %d %s", w.Code, w.Body)
	}
	theirs := e.claimedRunner(e.otherP)
	// The doors refuse this; the reads must not rely on that.
	if _, err := e.pool.Exec(context.Background(), `INSERT INTO runner_pool_members (pool_id, runner_id) VALUES ($1, $2)`, pool.ID, theirs); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/runner-pools/" + pool.ID.String() + "/runners"

	own := decodeJSON[[]types.RunnerView](t, e.call(e.person, http.MethodGet, path, ""))
	if len(own) != 1 || own[0].ID != mine || own[0].Owner != "" || own[0].MaxRuns != types.RunnerMaxRunsDefault {
		t.Fatalf("the owner's read of their pool = %+v, want only their own runner", own)
	}
	if len(own[0].Pools) != 1 || own[0].Pools[0].ID != pool.ID || own[0].Pools[0].Name != "Laptop" {
		t.Fatalf("the runner's pools = %+v, want [Laptop]", own[0].Pools)
	}
	got, random := e.call(e.other, http.MethodGet, path, ""), e.call(e.other, http.MethodGet, "/api/v1/runner-pools/"+uuid.NewString()+"/runners", "")
	if got.Code != http.StatusNotFound || got.Body.String() != random.Body.String() {
		t.Fatalf("another person's read = %d %s, a random id = %d %s", got.Code, got.Body, random.Code, random.Body)
	}
	oversight := decodeJSON[[]types.RunnerView](t, e.admin(http.MethodGet, path, ""))
	if len(oversight) != 1 || oversight[0].ID != mine || oversight[0].Owner != e.personP {
		t.Fatalf("an operator's read = %+v, want the owner's runner, owner named", oversight)
	}
	all := viewIDs(decodeJSON[[]types.RunnerView](t, e.admin(http.MethodGet, "/api/v1/runners", "")))
	if p := all[theirs].Pools; p == nil || len(p) != 0 {
		t.Errorf("the other person's runner lists pools %+v, want [] (the pool is not theirs)", p)
	}
	if p := all[mine].Pools; len(p) != 1 || p[0].ID != pool.ID {
		t.Errorf("the owner's runner lists pools %+v on the admin route", p)
	}
	remote := e.createPool("Provided", types.RunnerPoolRemoteProvided)
	// A member who may read the pool reads its runners too: a Remote Provided pool has none.
	if rows := decodeJSON[[]types.RunnerView](t, e.call(e.person, http.MethodGet, "/api/v1/runner-pools/"+remote.ID.String()+"/runners", "")); len(rows) != 0 {
		t.Errorf("a member's read of a Remote Provided pool's runners = %+v, want []", rows)
	}
	if rows := decodeJSON[[]types.RunnerView](t, e.admin(http.MethodGet, "/api/v1/runner-pools/"+remote.ID.String()+"/runners", "")); len(rows) != 0 {
		t.Errorf("a Remote Provided pool's runners = %+v, want []", rows)
	}
}
