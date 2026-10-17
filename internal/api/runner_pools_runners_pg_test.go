// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
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
	path := "/api/v1/runner-pools/" + pool.ID.String() + "/members"
	runnersOf := func(w *httptest.ResponseRecorder) []types.RunnerView {
		t.Helper()
		var out []types.RunnerView
		for _, m := range decodeJSON[[]client.RunnerPoolMemberView](t, w) {
			if m.Kind != types.RunnerKindSelfHosted || m.Runner == nil || m.ID != m.Runner.ID.String() || m.Name != m.Runner.Name {
				t.Fatalf("a self-hosted member = %+v", m)
			}
			out = append(out, *m.Runner)
		}
		return out
	}

	own := runnersOf(e.call(e.person, http.MethodGet, path, ""))
	if len(own) != 1 || own[0].ID != mine || own[0].Owner != "" {
		t.Fatalf("the owner's read of their pool = %+v, want only their own runner", own)
	}
	if len(own[0].Pools) != 1 || own[0].Pools[0].ID != pool.ID || own[0].Pools[0].Name != "Laptop" {
		t.Fatalf("the runner's pools = %+v, want [Laptop]", own[0].Pools)
	}
	got, random := e.call(e.other, http.MethodGet, path, ""), e.call(e.other, http.MethodGet, "/api/v1/runner-pools/"+uuid.NewString()+"/members", "")
	if got.Code != http.StatusNotFound || got.Body.String() != random.Body.String() {
		t.Fatalf("another person's read = %d %s, a random id = %d %s", got.Code, got.Body, random.Code, random.Body)
	}
	oversight := runnersOf(e.admin(http.MethodGet, path, ""))
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
	remote := e.createPool("Provided", types.RunnerPoolOrganization)
	// A member who may read the pool reads its members too: an empty organization pool has none.
	if rows := decodeJSON[[]client.RunnerPoolMemberView](t, e.call(e.person, http.MethodGet, "/api/v1/runner-pools/"+remote.ID.String()+"/members", "")); len(rows) != 0 {
		t.Errorf("a member's read of an empty organization pool's members = %+v, want []", rows)
	}
	if rows := decodeJSON[[]client.RunnerPoolMemberView](t, e.admin(http.MethodGet, "/api/v1/runner-pools/"+remote.ID.String()+"/members", "")); len(rows) != 0 {
		t.Errorf("an empty organization pool's members = %+v, want []", rows)
	}
	e.admin(http.MethodPut, "/api/v1/runner-pools/"+remote.ID.String()+"/remote-runners/fake", "")
	rows := decodeJSON[[]client.RunnerPoolMemberView](t, e.admin(http.MethodGet, "/api/v1/runner-pools/"+remote.ID.String()+"/members", ""))
	if len(rows) != 1 || rows[0].Kind != types.RunnerKindRemoteHosted || rows[0].ID != "fake" || rows[0].Name != types.RemoteRunnerDefaultName("fake") || !rows[0].Configured || rows[0].Runner != nil {
		t.Errorf("an organization pool's members = %+v, want the remote-hosted runner", rows)
	}
	if mine := decodeJSON[[]client.RunnerPoolMemberView](t, e.call(e.person, http.MethodGet, "/api/v1/runner-pools/"+remote.ID.String()+"/members", "")); len(mine) != 1 || mine[0].ID != "fake" {
		t.Errorf("a member who may use the organization pool reads %+v, want its remote-hosted runner", mine)
	}
}
