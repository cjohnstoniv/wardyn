// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A person's own user pools against a real Postgres (H14-api): only the owner creates, changes,
// deletes or uses one; another person, and an administrator writing, meet the answer an unknown id gets;
// the organisation keeps no user pool as its default; administrators oversee every pool read-only; erasure
// removes a person's pools; and a person reads which of their pools each runner is in.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func myPoolPath(id uuid.UUID) string { return "/api/v1/me/runner-pools/" + id.String() }

func myRemotePath(id uuid.UUID, rid string) string { return myPoolPath(id) + "/remote-runners/" + rid }

// A person creates, edits and deletes their own user pools; names are unique per owner only.
func TestPG_MyRunnerPoolsCreateEditDelete(t *testing.T) {
	e := newPoolEnv(t, false)
	laptop := e.createOwnPool(e.person, "Laptop")
	if laptop.Scope != types.RunnerPoolUser || laptop.Owner != e.personP || laptop.Revision != 1 {
		t.Fatalf("created %+v, want a user pool owned by %s at revision 1", laptop, e.personP)
	}
	// Another person may use the same name: one person's names tell nobody else anything.
	e.createOwnPool(e.other, "Laptop")
	if w := e.call(e.person, http.MethodPost, "/api/v1/me/runner-pools", `{"name":"laptop"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a duplicate of one's own name = %d %s, want 400", w.Code, w.Body)
	}
	if w := e.call(e.person, http.MethodPost, "/api/v1/me/runner-pools", `{"name":"x","scope":"organization"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a scope on a person's create = %d %s, want 400: the route makes user pools only", w.Code, w.Body)
	}
	if w := e.admin(http.MethodPost, "/api/v1/me/runner-pools", `{"name":"Bearer"}`); w.Code != http.StatusForbidden {
		t.Errorf("the admin token creating a personal pool = %d %s, want 403", w.Code, w.Body)
	}

	w := e.call(e.person, http.MethodPut, myPoolPath(laptop.ID), `{"revision":1,"name":"Work laptop","state":"disabled"}`)
	if got := decodeJSON[types.RunnerPool](t, w); w.Code != http.StatusOK || got.Name != "Work laptop" || got.State != types.RunnerPoolDisabled || got.Revision != 2 {
		t.Fatalf("own update = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.person, http.MethodPut, myPoolPath(laptop.ID), `{"revision":1,"name":"Again"}`); w.Code != http.StatusConflict {
		t.Errorf("a stale own update = %d %s, want 409", w.Code, w.Body)
	}
	if w := e.call(e.person, http.MethodDelete, myPoolPath(laptop.ID), ""); w.Code != http.StatusNoContent {
		t.Fatalf("own delete = %d %s", w.Code, w.Body)
	}
	if p, _ := e.pg.GetRunnerPool(context.Background(), laptop.ID); p.State != types.RunnerPoolDeleted {
		t.Errorf("the deleted pool reads %q", p.State)
	}
	for action, n := range map[string]int{auditPoolCreate: 2, auditPoolUpdate: 1, auditPoolDelete: 1} {
		rows := e.audits(action)
		if len(rows) != n {
			t.Fatalf("%s rows = %d, want %d", action, len(rows), n)
		}
		if d := auditData(t, rows[0]); d["owner"] != e.personP || d["scope"] != string(types.RunnerPoolUser) {
			t.Errorf("%s row = %v, want the owner and the scope", action, d)
		}
	}
}

// Each person may keep at most RunnerPoolsPerOwnerMax pools; the next is a 422 in plain words, and the
// cap is the person's own.
func TestPG_MyRunnerPoolsPerPersonCap(t *testing.T) {
	e := newPoolEnv(t, false)
	for i := range 10 {
		e.createOwnPool(e.person, fmt.Sprintf("Pool %d", i))
	}
	w := e.call(e.person, http.MethodPost, "/api/v1/me/runner-pools", `{"name":"One more"}`)
	if w.Code != http.StatusUnprocessableEntity || !strings.Contains(w.Body.String(), "10 user pools") {
		t.Errorf("an eleventh pool = %d %s, want 422 in plain words", w.Code, w.Body)
	}
	e.createOwnPool(e.other, "Theirs")
	e.createPool("Provided", types.RunnerPoolOrganization)
}

// Another person's pool is a 404 on every verb, byte for byte the answer for a random id, and nothing
// moves; an administrator has no write path on it either.
func TestPG_AnotherPersonsPoolIsAnUnknownID(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createOwnPool(e.person, "Laptop")
	mine := e.claimedRunner(e.personP)
	if w := e.call(e.person, http.MethodPut, addRunnerPath(pool.ID, mine), ""); w.Code != http.StatusCreated {
		t.Fatalf("own add = %d %s", w.Code, w.Body)
	}
	theirs := e.claimedRunner(e.otherP)
	random := uuid.New()
	// The remote-hosted runner is available to everyone's pools, so only the pool's owner stands between
	// another person and its member routes.
	if w := e.call(e.alice, http.MethodPut, "/api/v1/remote-runners/fake/people-policy", `{"people_pools":"everyone"}`); w.Code != http.StatusOK {
		t.Fatalf("people policy = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.person, http.MethodPut, myRemotePath(pool.ID, "fake"), ""); w.Code != http.StatusCreated {
		t.Fatalf("own remote add = %d %s", w.Code, w.Body)
	}
	type verb struct{ method, path, body string }
	verbs := func(id uuid.UUID) []verb {
		return []verb{
			{http.MethodGet, "/api/v1/runner-pools/" + id.String(), ""},
			{http.MethodPut, myPoolPath(id), `{"revision":1,"name":"Mine now"}`},
			{http.MethodDelete, myPoolPath(id), ""},
			{http.MethodPut, addRunnerPath(id, theirs), ""},
			{http.MethodDelete, addRunnerPath(id, mine), ""},
			{http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, id)},
			{http.MethodPut, myRemotePath(id, "fake"), ""},
			{http.MethodDelete, myRemotePath(id, "fake"), ""},
			{http.MethodGet, "/api/v1/runner-pools/" + id.String() + "/members", ""},
		}
	}
	for i, v := range verbs(pool.ID) {
		got := e.call(e.other, v.method, v.path, v.body)
		r := verbs(random)[i]
		want := e.call(e.other, r.method, r.path, r.body)
		if got.Code != http.StatusNotFound || got.Code != want.Code || got.Body.String() != want.Body.String() {
			t.Errorf("%s %s: another person's pool = %d %s, a random id = %d %s", v.method, v.path, got.Code, got.Body, want.Code, want.Body)
		}
	}
	for _, v := range []verb{
		{http.MethodPut, poolPath(pool.ID), `{"revision":1,"state":"disabled"}`},
		{http.MethodDelete, poolPath(pool.ID), ""},
		{http.MethodPut, poolPath(pool.ID) + "/remote-runners/fake", ""},
		{http.MethodDelete, poolPath(pool.ID) + "/remote-runners/fake", ""},
		{http.MethodPut, myPoolPath(pool.ID), `{"revision":1,"state":"disabled"}`},
		{http.MethodDelete, myPoolPath(pool.ID), ""},
		{http.MethodPut, myRemotePath(pool.ID, "fake"), ""},
		{http.MethodDelete, myRemotePath(pool.ID, "fake"), ""},
	} {
		got := e.call(e.super, v.method, v.path, v.body)
		want := e.call(e.super, v.method, strings.Replace(v.path, pool.ID.String(), random.String(), 1), v.body)
		if got.Code != http.StatusNotFound || got.Body.String() != want.Body.String() {
			t.Errorf("admin %s %s on a person's pool = %d %s, a random id = %d %s", v.method, v.path, got.Code, got.Body, want.Code, want.Body)
		}
	}
	if p, _ := e.pg.GetRunnerPool(context.Background(), pool.ID); p.Revision != 3 || p.State != types.RunnerPoolActive || p.Name != "Laptop" || e.members(pool.ID) != 2 {
		t.Errorf("the pool moved: %+v, %d members", p, e.members(pool.ID))
	}
	if n := e.count(`SELECT count(*) FROM principal_prefs WHERE principal = $1`, e.otherP); n != 0 {
		t.Errorf("another person's default naming the pool was stored (%d rows)", n)
	}
}

// An administrator cannot create a user pool: the organisation's route makes organization pools only,
// a scope in the body is refused as the old hosting type is, and nothing is stored.
func TestPG_OperatorCreateOfAUserPoolIsRefused(t *testing.T) {
	e := newPoolEnv(t, false)
	for _, body := range []string{`{"name":"Laptops","scope":"user"}`, `{"name":"Laptops","hosting_type":"self_hosted"}`} {
		w := e.admin(http.MethodPost, "/api/v1/runner-pools", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("admin create %s = %d %s, want 400", body, w.Code, w.Body)
		}
	}
	if n := e.count(`SELECT count(*) FROM runner_pools`); n != 0 {
		t.Errorf("a refused create stored %d pools", n)
	}
	if p := e.createPool("Builds", types.RunnerPoolOrganization); p.Scope != types.RunnerPoolOrganization || p.Owner != "" {
		t.Errorf("the organisation's create made %+v", p)
	}
}

// The catalogue: a person sees the organization pools they may use and only their own user
// pools; an administrator sees every pool, with its owner, and uses none of another person's. G5: each
// own pool lists the person's runners in it, whether or not they could take a run now.
func TestPG_CatalogueShowsOnlyOwnUserPoolsAndTheirRunners(t *testing.T) {
	e := newPoolEnv(t, false)
	remote := e.createPool("Provided", types.RunnerPoolOrganization)
	laptop := e.createOwnPool(e.person, "Laptop")
	home := e.createOwnPool(e.person, "Home server")
	theirs := e.createOwnPool(e.other, "Theirs")
	a, b := e.claimedRunner(e.personP), e.claimedRunner(e.personP)
	for _, add := range [][2]uuid.UUID{{laptop.ID, a}, {home.ID, a}, {home.ID, b}} {
		if w := e.call(e.person, http.MethodPut, addRunnerPath(add[0], add[1]), ""); w.Code != http.StatusCreated {
			t.Fatalf("add = %d %s", w.Code, w.Body)
		}
	}

	ids := func(m map[uuid.UUID]client.RunnerPoolCatalogueEntry) []uuid.UUID {
		var out []uuid.UUID
		for id := range m {
			out = append(out, id)
		}
		slices.SortFunc(out, func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) })
		return out
	}
	sorted := func(in ...uuid.UUID) []uuid.UUID {
		slices.SortFunc(in, func(x, y uuid.UUID) int { return strings.Compare(x.String(), y.String()) })
		return in
	}
	person := e.listed(e.person)
	if got, want := ids(person), sorted(remote.ID, laptop.ID, home.ID); !slices.Equal(got, want) {
		t.Errorf("the person lists %v, want %v", got, want)
	}
	if got, want := ids(e.listed(e.other)), sorted(remote.ID, theirs.ID); !slices.Equal(got, want) {
		t.Errorf("the other person lists %v, want %v", got, want)
	}
	runnerIDs := func(entry client.RunnerPoolCatalogueEntry) []uuid.UUID {
		var out []uuid.UUID
		for _, r := range entry.MyRunners {
			out = append(out, r.ID)
		}
		return sorted(out...)
	}
	if got := runnerIDs(person[laptop.ID]); !slices.Equal(got, []uuid.UUID{a}) {
		t.Errorf("Laptop's my_runners = %v, want [%s]", got, a)
	}
	if got, want := runnerIDs(person[home.ID]), sorted(a, b); !slices.Equal(got, want) {
		t.Errorf("Home server's my_runners = %v, want %v", got, want)
	}
	if person[laptop.ID].Owner != e.personP || person[remote.ID].Owner != "" {
		t.Errorf("owners: own %q, remote %q", person[laptop.ID].Owner, person[remote.ID].Owner)
	}

	oversight := e.listed(e.alice)
	if got, want := ids(oversight), sorted(remote.ID, laptop.ID, home.ID, theirs.ID); !slices.Equal(got, want) {
		t.Fatalf("security oversight lists %v, want every pool %v", got, want)
	}
	if got := oversight[theirs.ID]; got.Owner != e.otherP || got.Reason != runnerpool.ReasonNotFound || got.MyRunners != nil {
		t.Errorf("an oversight row = %+v, want the owner, manage-only and no runners", got)
	}
	if w := e.call(e.alice, http.MethodGet, "/api/v1/runner-pools/"+theirs.ID.String(), ""); w.Code != http.StatusOK || decodeJSON[types.RunnerPool](t, w).Owner != e.otherP {
		t.Errorf("an oversight read = %d %s", w.Code, w.Body)
	}
}

// DONE WHEN (RP-1 7): the organisation's default is an organization pool, so a user pool (the
// administrator's own included) is refused by name and nothing is stored; a person's own default names a
// pool they may use, so another person's user pool and an organization pool a use policy withholds are
// the unknown-id 404, byte for byte, and only their own is kept.
func TestPG_NoOrganisationUserPoolDefault(t *testing.T) {
	e := newPoolEnv(t, false)
	own := e.createOwnPool(e.super, "Admin laptop")
	w := e.admin(http.MethodPut, "/api/v1/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, own.ID))
	if w.Code != http.StatusBadRequest || wireError(t, w) != noOrgUserPoolDefaultMsg {
		t.Errorf("an organisation default naming a user pool = %d %s, want 400 refused by name", w.Code, w.Body)
	}
	if n := e.count(`SELECT count(*) FROM runner_pool_org_defaults`); n != 0 {
		t.Errorf("the refused default stored %d rows", n)
	}
	if w := e.call(e.super, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, own.ID)); w.Code != http.StatusOK {
		t.Errorf("an owner's own default = %d %s", w.Code, w.Body)
	}
	withheld := e.createPool("Finance", types.RunnerPoolOrganization)
	if w := e.call(e.alice, http.MethodPut, usePolicyPath(withheld.ID), `{"subjects":[{"subject_type":"user","subject":"sub-finance"}]}`); w.Code != http.StatusOK {
		t.Fatalf("use policy = %d %s", w.Code, w.Body)
	}
	unknown := e.call(e.person, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, uuid.New()))
	for name, id := range map[string]uuid.UUID{"another person's user pool": own.ID, "a withheld organization pool": withheld.ID} {
		w := e.call(e.person, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, id))
		if w.Code != http.StatusNotFound || w.Body.String() != unknown.Body.String() {
			t.Errorf("a personal default naming %s = %d %s, want the unknown-id 404 %s", name, w.Code, w.Body, unknown.Body)
		}
	}
	if n := e.count(`SELECT count(*) FROM principal_prefs WHERE principal = $1`, e.personP); n != 0 {
		t.Errorf("a refused personal default stored %d rows", n)
	}
}

// Erasure removes a person's pools: deleted, members dropped, and the person's name gone from every
// pool row of theirs; another person's pools are untouched. A SCIM purge asks for the same scope.
func TestPG_ErasureRemovesAPersonsPools(t *testing.T) {
	e := newPoolEnv(t, false)
	gone := e.createOwnPool(e.person, "Laptop")
	old := e.createOwnPool(e.person, "Old")
	if w := e.call(e.person, http.MethodDelete, myPoolPath(old.ID), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", w.Code)
	}
	kept := e.createOwnPool(e.other, "Theirs")
	runnerID := e.claimedRunner(e.personP)
	e.call(e.person, http.MethodPut, addRunnerPath(gone.ID, runnerID), "")

	detail, err := e.srv.erasureOrchestrator(nil).Steps[erasure.RunnerPools](context.Background(), e.personP)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := detail.(map[string]any); d["runner_pools"] != 1 {
		t.Errorf("erasure detail = %v, want one pool", detail)
	}
	if p, _ := e.pg.GetRunnerPool(context.Background(), gone.ID); p.State != types.RunnerPoolDeleted || p.Owner != "" || e.members(gone.ID) != 0 {
		t.Errorf("the erased pool = %+v with %d members, want deleted, ownerless, empty", p, e.members(gone.ID))
	}
	if n := e.count(`SELECT count(*) FROM runner_pools WHERE owner = $1 OR created_by = $1`, e.personP); n != 0 {
		t.Errorf("%d pool rows still name the erased person", n)
	}
	if p, _ := e.pg.GetRunnerPool(context.Background(), kept.ID); p.State != types.RunnerPoolActive || p.Owner != e.otherP {
		t.Errorf("another person's pool = %+v", p)
	}
	if !slices.Contains(scimPurgeScopes, erasure.RunnerPools) {
		t.Error("a SCIM purge does not remove the person's pools")
	}
}

// A user pool's limits are its owner's, and every run in it is still bounded by the owner's governance
// profile: a pool cap above the profile's cap is held to the profile, at the list and at placement.
func TestPG_MyPoolLimitsAreBoundedByGovernance(t *testing.T) {
	e := newPlacementEnv(t)
	body := `{"name":"Big laptop","limits":{"background":{"cpu_millis":{"default":1000,"cap":16000},` +
		`"memory_mib":{"default":1024,"cap":16384},"lifetime":{"default_sec":3600,"max_sec":7200}},"barriers":["CC1","CC2","CC3"]}}`
	w := e.call(e.person, http.MethodPost, "/api/v1/me/runner-pools", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	pool := decodeJSON[types.RunnerPool](t, w)
	big := e.laptop(e.personP, 16000, 16384, types.CC1, types.CC2, types.CC3)
	e.addToPool(e.person, pool.ID, big)
	e.assignSizedProfile("six", &types.ResourceLimits{CPUMillis: 6000, MemoryMiB: 8192}, types.GovernanceLimits{})

	entry := e.listed(e.person)[pool.ID]
	if len(entry.Limits) != 1 || entry.Limits[0].CPUMillis.Cap.Value != 6000 || entry.Limits[0].CPUMillis.CapSource != types.LimitSourceGovernance {
		t.Errorf("the list's limits = %+v, want the profile's 6000 cap", entry.Limits)
	}
	ctx := operatorCtx(e.personP, "person@corp.example", oidc.RoleUser)
	ceiling, err := e.srv.effectiveCeiling(ctx)
	if err != nil {
		t.Fatal(err)
	}
	res := &client.RunResources{CPUMillis: 12000}
	req := createRunRequest{Agent: "claude-code", Placement: types.PlacementLocal, RunnerID: big.String(), RunnerPoolID: pool.ID.String(), Resources: res}
	req.Experience = client.ExperienceBackground
	plan, refusal := e.srv.resolvePlacement(ctx, placementInput{req: &req, reqCCOK: true, ceiling: ceiling, ask: resourceAsk(res, types.ResourceLimits{})})
	if refusal != nil {
		t.Fatalf("placement refused: %d %+v", refusal.status, refusal.body)
	}
	if got := plan.applied.CPUMillis; got.Applied != 6000 || !got.Capped || got.Source != types.LimitSourceGovernance {
		t.Errorf("applied cpu = %+v, want 6000 capped by governance", got)
	}
}

// A user pool has no use policy. Its owner, on every use-policy verb, is told so in plain words;
// anyone else, an administrator included, gets the unknown-id 404 byte for byte, and nothing is stored.
func TestPG_UsePolicyRoutesOnAUserPool(t *testing.T) {
	e := newPoolEnv(t, false)
	own := e.createOwnPool(e.alice, "Alice laptop")
	random := uuid.New()
	body := `{"subjects":[{"subject_type":"user","subject":"sub-other"}]}`
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		w := e.call(e.alice, m, usePolicyPath(own.ID), body)
		if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(runnerpool.ReasonMemberMismatch) ||
			!strings.Contains(w.Body.String(), "Alice laptop is your own user pool: only you use it, so it has no use policy.") {
			t.Errorf("owner %s = %d %s, want 422 saying a user pool has no use policy", m, w.Code, w.Body)
		}
		got := e.call(e.super, m, usePolicyPath(own.ID), body)
		want := e.call(e.super, m, usePolicyPath(random), body)
		if got.Code != http.StatusNotFound || got.Body.String() != want.Body.String() {
			t.Errorf("another admin %s = %d %s, a random id = %d %s", m, got.Code, got.Body, want.Code, want.Body)
		}
	}
	if n := e.count(`SELECT count(*) FROM runner_pool_use_policies`); n != 0 || e.revision(own.ID) != 1 {
		t.Errorf("%d use policies stored, revision %d", n, e.revision(own.ID))
	}
}

// Only a claimed runner of the caller's, bound to this organisation, joins a pool: a revoked or an
// unclaimed one of their own answers exactly as an unknown runner, and nothing is stored.
func TestPG_RevokedAndUnclaimedRunnersNeverJoinAPool(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createOwnPool(e.person, "Laptop")
	revoked := e.claimedRunner(e.personP)
	if _, err := e.pg.RevokeRunner(context.Background(), revoked, time.Now(), "", ""); err != nil {
		t.Fatal(err)
	}
	unclaimed := e.rawRunner(e.personP, poolTestOrgURL, false)
	unknown := uuid.New()
	answer := func(id uuid.UUID) string {
		t.Helper()
		w := e.call(e.person, http.MethodPut, addRunnerPath(pool.ID, id), "")
		if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(runnerpool.ReasonMemberMismatch) {
			t.Errorf("adding %s = %d %s, want 422 runner_pool_member_mismatch", id, w.Code, w.Body)
		}
		return strings.ReplaceAll(w.Body.String(), id.String(), "X")
	}
	want := answer(unknown)
	for name, id := range map[string]uuid.UUID{"revoked": revoked, "unclaimed": unclaimed} {
		if got := answer(id); got != want {
			t.Errorf("a %s runner answered %s, an unknown one %s", name, got, want)
		}
	}
	if n := e.members(pool.ID); n != 0 || e.revision(pool.ID) != 1 {
		t.Errorf("%d members, revision %d", n, e.revision(pool.ID))
	}
}

// With no organisation default and none of the person's own, a launch that names no pool is refused
// runner_pool_required at every door: no pool is picked for them, not even the only one. Once the person
// sets their own default, the same launch resolves.
func TestPG_NoDefaultAsksForAChoice(t *testing.T) {
	e := newPlacementEnv(t)
	remote := e.createPool("Provided", types.RunnerPoolOrganization)
	e.addExecutors(remote.ID, "alpha")
	sameEverywhere(t, "a launch naming nothing", e.answers(e.person, runBody("")), http.StatusUnprocessableEntity, string(runnerpool.ReasonRequired))
	if w := e.call(e.person, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, remote.ID)); w.Code != http.StatusOK {
		t.Fatalf("personal default = %d %s", w.Code, w.Body)
	}
	for door, w := range e.answers(e.person, runBody("")) {
		if w.Code >= 300 && wireReason(t, w) == string(runnerpool.ReasonRequired) {
			t.Errorf("%s after a personal default = %d %s", door, w.Code, w.Body)
		}
	}
}
