// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Remote-hosted runners against a real Postgres (RP-1): who may add one to their own user pool, the
// second human on its people policy, and the leaver purge. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func wireError(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var b errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &b)
	return b.Error
}

const peoplePath = "/api/v1/remote-runners/fake/people-policy"

func (e *poolEnv) remoteRunner(id string) types.RemoteRunner {
	e.t.Helper()
	row, err := e.pg.GetRemoteRunner(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return row.Runner
}

// DONE WHEN (RP-1 3): a remote-hosted runner joins a person's user pool only while its "Available to
// people's pools" policy admits that person: never when it is off, always when it is everyone, and when
// it is listed only for a listed person. A runner the person may not add answers exactly as one that
// does not exist, and nothing is stored. A later narrowing leaves the member row (RP-2 drops it from the
// candidates at launch), and only the admitted runners are listed to the person.
func TestPG_UserPoolRemoteRunnerNeedsPeoplePolicy(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createOwnPool(e.person, "Laptop")
	add := func() *httptest.ResponseRecorder {
		return e.call(e.person, http.MethodPut, myRemotePath(pool.ID, "fake"), "")
	}
	unknown := e.call(e.person, http.MethodPut, myRemotePath(pool.ID, "nomad"), "")
	refusedAlike := func(name string) {
		t.Helper()
		w := add()
		if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(runnerpool.ReasonMemberMismatch) ||
			wireError(t, w) != runnerpool.RemoteNotAvailableMsg("fake") ||
			strings.ReplaceAll(w.Body.String(), "fake", "X") != strings.ReplaceAll(unknown.Body.String(), "nomad", "X") {
			t.Errorf("%s: add = %d %s, an unknown runner = %d %s", name, w.Code, w.Body, unknown.Code, unknown.Body)
		}
		if n := e.members(pool.ID); n != 0 || e.revision(pool.ID) != 1 {
			t.Errorf("%s: %d members, revision %d", name, n, e.revision(pool.ID))
		}
		if got := decodeJSON[[]client.RemoteRunnerView](t, e.call(e.person, http.MethodGet, "/api/v1/me/remote-runners", "")); len(got) != 0 {
			t.Errorf("%s: the person is offered %+v", name, got)
		}
	}
	setPeople := func(body string) {
		t.Helper()
		if w := e.call(e.alice, http.MethodPut, peoplePath, body); w.Code != http.StatusOK {
			t.Fatalf("people policy %s = %d %s", body, w.Code, w.Body)
		}
	}

	refusedAlike("off")
	setPeople(`{"people_pools":"listed","subjects":[{"subject_type":"user","subject":"sub-someone-else"}]}`)
	refusedAlike("listed without the person")

	setPeople(`{"people_pools":"listed","subjects":[{"subject_type":"user","subject":"sub-person"}]}`)
	offered := decodeJSON[[]client.RemoteRunnerView](t, e.call(e.person, http.MethodGet, "/api/v1/me/remote-runners", ""))
	if len(offered) != 1 || offered[0].ID != "fake" || offered[0].PeopleSubjects != nil || len(offered[0].Pools) != 0 {
		t.Errorf("the person is offered %+v, want the runner without its people list", offered)
	}
	w := add()
	if w.Code != http.StatusCreated {
		t.Fatalf("listed add = %d %s", w.Code, w.Body)
	}
	if m := decodeJSON[types.RunnerPoolMember](t, w); m.Kind != types.RunnerKindRemoteHosted || m.RemoteRunnerID != "fake" || m.RunnerID != nil {
		t.Errorf("member = %+v", m)
	}
	rows := e.audits(auditPoolRunnerAdd)
	if len(rows) != 1 {
		t.Fatalf("%d runner.attach rows", len(rows))
	}
	if d := auditData(t, rows[0]); d["runner_kind"] != "remote_hosted" || d["remote_runner_id"] != "fake" || d["scope"] != "user" || d["owner"] != e.personP {
		t.Errorf("attach audit = %v", d)
	}
	if offered := decodeJSON[[]client.RemoteRunnerView](t, e.call(e.person, http.MethodGet, "/api/v1/me/remote-runners", "")); len(offered) != 1 || len(offered[0].Pools) != 1 || offered[0].Pools[0].ID != pool.ID || offered[0].Pools[0].Owner != "" {
		t.Errorf("after the add the person is offered %+v, want their own pool named and no owner", offered)
	}

	setPeople(`{"people_pools":"everyone"}`)
	theirs := e.createOwnPool(e.other, "Theirs")
	if w := e.call(e.other, http.MethodPut, myRemotePath(theirs.ID, "fake"), ""); w.Code != http.StatusCreated {
		t.Errorf("everyone: another person's add = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodDelete, peoplePath, ""); w.Code != http.StatusNoContent {
		t.Fatalf("clear = %d %s", w.Code, w.Body)
	}
	if e.members(pool.ID) != 1 || e.members(theirs.ID) != 1 {
		t.Error("turning the policy off removed a member row")
	}
	if w := e.call(e.person, http.MethodDelete, myRemotePath(pool.ID, "fake"), ""); w.Code != http.StatusNoContent || e.members(pool.ID) != 0 {
		t.Errorf("a person removes a runner the policy no longer admits = %d, %d members left", w.Code, e.members(pool.ID))
	}
}

// DONE WHEN (RP-1 4): with WARDYN_GOVERNANCE_SECOND_HUMAN on, a people-policy set or clear is held and
// nothing moves until a second human approves; a held change goes stale when the policy moved under it.
func TestPeoplePolicyHeldForSecondHuman(t *testing.T) {
	e := newPoolEnv(t, true)
	held := e.pending(e.call(e.alice, http.MethodPut, peoplePath, `{"people_pools":"everyone"}`))
	if held.TargetKind != govKindRemoteRunnerPeople || held.TargetKey != "fake" || held.Op != "set" {
		t.Errorf("held = %+v", held)
	}
	if rr := e.remoteRunner("fake"); rr.PeoplePools != types.PeoplePoolsOff || rr.Revision != 1 {
		t.Fatalf("a held set moved the runner: %+v", rr)
	}
	if n := len(e.audits(auditRemoteRunnerPeopleSet)); n != 0 {
		t.Errorf("a held set wrote %d audit rows", n)
	}
	e.approve(held.ID)
	if rr := e.remoteRunner("fake"); rr.PeoplePools != types.PeoplePoolsEveryone || rr.Revision != 2 {
		t.Fatalf("the approved set = %+v", rr)
	}
	if rows := e.audits(auditRemoteRunnerPeopleSet); len(rows) != 1 {
		t.Errorf("%d people_policy.set rows after the approval", len(rows))
	}

	clear := e.pending(e.call(e.carol, http.MethodDelete, peoplePath, ""))
	if rr := e.remoteRunner("fake"); rr.PeoplePools != types.PeoplePoolsEveryone {
		t.Fatalf("a held clear moved the runner: %+v", rr)
	}
	// The admin token writes past the hold (break glass), so the clear reviewed a policy that moved.
	if w := e.admin(http.MethodPut, peoplePath, `{"people_pools":"listed","subjects":[{"subject_type":"group","subject":"eng"}]}`); w.Code != http.StatusOK {
		t.Fatalf("break-glass write = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(clear.ID), ""); w.Code == http.StatusOK {
		t.Fatalf("approving a clear gone stale = %d %s", w.Code, w.Body)
	}
	if rr := e.remoteRunner("fake"); rr.PeoplePools != types.PeoplePoolsListed {
		t.Errorf("a stale approval applied: %+v", rr)
	}
}

// DONE WHEN (RP-1 6): the leaver purge strips the person from every remote-hosted runner's people list,
// and a list it empties admits nobody: it stays listed, never everyone.
func TestPG_LeaverPurgeStripsPeoplePolicyAndNeverWidens(t *testing.T) {
	e := newPoolEnv(t, false)
	ctx := context.Background()
	if w := e.call(e.alice, http.MethodPut, peoplePath, `{"people_pools":"listed","subjects":[{"subject_type":"user","subject":"Sub-Person"}]}`); w.Code != http.StatusOK {
		t.Fatalf("people policy = %d %s", w.Code, w.Body)
	}
	before := e.remoteRunner("fake")
	if _, _, _, err := e.pg.DeleteUserSubjectRows(ctx, uuid.New(), []string{"sub-person"}, nil); err != nil {
		t.Fatal(err)
	}
	rr := e.remoteRunner("fake")
	if rr.PeoplePools != types.PeoplePoolsListed || rr.PeopleSubjects == nil || len(rr.PeopleSubjects) != 0 || rr.Revision != before.Revision+1 {
		t.Fatalf("after the purge = %+v, want listed with nobody, revision raised", rr)
	}
	for name, c := range map[string]*http.Cookie{"the purged person": e.person, "another person": e.other} {
		if got := decodeJSON[[]client.RemoteRunnerView](t, e.call(c, http.MethodGet, "/api/v1/me/remote-runners", "")); len(got) != 0 {
			t.Errorf("%s is offered %+v by an emptied list", name, got)
		}
		pool := e.createOwnPool(c, "Pool "+name)
		if w := e.call(c, http.MethodPut, myRemotePath(pool.ID, "fake"), ""); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s adds through an emptied list = %d %s", name, w.Code, w.Body)
		}
	}
}

// The operators read every remote-hosted runner with every pool it is in; a member is refused the
// operators' reads and the rename; a rename is audited and needs the revision read.
func TestPG_RemoteRunnerReadsAndRename(t *testing.T) {
	e := newPoolEnv(t, false)
	org := e.createPool("Builds", types.RunnerPoolOrganization)
	e.admin(http.MethodPut, poolPath(org.ID)+"/remote-runners/fake", "")
	if w := e.call(e.alice, http.MethodPut, peoplePath, `{"people_pools":"everyone"}`); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	mine := e.createOwnPool(e.person, "Laptop")
	e.call(e.person, http.MethodPut, myRemotePath(mine.ID, "fake"), "")

	list := decodeJSON[[]client.RemoteRunnerView](t, e.call(e.alice, http.MethodGet, "/api/v1/remote-runners", ""))
	if len(list) != 1 || list[0].ID != "fake" || list[0].Kind != types.RunnerKindRemoteHosted || !list[0].Configured || len(list[0].Pools) != 2 {
		t.Fatalf("operators' list = %+v", list)
	}
	for _, p := range list[0].Pools {
		if (p.Scope == types.RunnerPoolUser) != (p.Owner == e.personP) {
			t.Errorf("pool ref %+v: a user pool names its owner, an organization pool none", p)
		}
	}
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/remote-runners", ""},
		{http.MethodGet, "/api/v1/remote-runners/fake", ""},
		{http.MethodPut, "/api/v1/remote-runners/fake", `{"revision":2,"name":"Mine"}`},
		{http.MethodPut, peoplePath, `{"people_pools":"everyone"}`},
	} {
		if w := e.call(e.person, c.method, c.path, c.body); w.Code != http.StatusForbidden {
			t.Errorf("a member's %s %s = %d", c.method, c.path, w.Code)
		}
	}
	if w := e.call(e.alice, http.MethodPut, "/api/v1/remote-runners/fake", `{"revision":2,"name":"Build host"}`); w.Code != http.StatusForbidden {
		t.Errorf("a security admin's rename = %d, want 403: the rename is the admin's", w.Code)
	}
	if w := e.call(e.super, http.MethodPut, "/api/v1/remote-runners/fake", `{"revision":1,"name":"Build host"}`); w.Code != http.StatusConflict {
		t.Errorf("a rename at a stale revision = %d %s", w.Code, w.Body)
	}
	w := e.call(e.super, http.MethodPut, "/api/v1/remote-runners/fake", `{"revision":2,"name":"Build host"}`)
	if got := decodeJSON[client.RemoteRunnerView](t, w); w.Code != http.StatusOK || got.Name != "Build host" || got.Revision != 3 {
		t.Fatalf("rename = %d %s", w.Code, w.Body)
	}
	if rows := e.audits(auditRemoteRunnerUpdate); len(rows) != 1 || auditData(t, rows[0])["name_before"] != types.RemoteRunnerDefaultName("fake") {
		t.Errorf("rename audit rows = %+v", rows)
	}
	if w := e.call(e.alice, http.MethodGet, "/api/v1/remote-runners/nomad", ""); w.Code != http.StatusNotFound {
		t.Errorf("an unknown runner = %d", w.Code)
	}
}
