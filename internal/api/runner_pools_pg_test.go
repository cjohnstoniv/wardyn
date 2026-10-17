// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The runner pool catalogue against a real Postgres (the H10 proofs): who may write what, what a
// person can and cannot see, and that no route lets one person act on another's runner or defaults.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const poolTestOrgURL = "https://org.example.com"

// poolEnv is a deployment with pool storage, one remote-hosted runner ("fake"), the four-eyes switch
// off, and three people: alice (security admin), super (admin) and two members.
type poolEnv struct {
	*govEnv
	// superTwo is a second super admin: a remote-hosted runner add or removal is decided on the super-admin tier.
	superTwo *http.Cookie
	person   *http.Cookie
	other    *http.Cookie
	personP  string
	otherP   string
}

func newPoolEnv(t *testing.T, secondHuman bool) *poolEnv {
	t.Helper()
	mutate := func(c *Config) { c.Runner = &fakeRunner{}; c.RunnerOrgURL = poolTestOrgURL }
	if secondHuman {
		t.Setenv(envGovernanceSecondHuman, "true")
	}
	g := newGovEnvOn(t, throwawayBarePGPool(t), mutate)
	e := &poolEnv{
		govEnv:   g,
		superTwo: ssoSession(t, poolSubSuperTwo, "super-two@corp.example", oidc.RoleAdmin),
		person:   ssoSession(t, "sub-person", "person@corp.example", "user"), personP: "sub-person",
		other: ssoSession(t, "sub-other", "other@corp.example", "user"), otherP: "sub-other",
	}
	e.ensureRemoteRunners()
	return e
}

// ensureRemoteRunners gives each remote-hosted runner the server is set up with its row, as every boot
// does, without the boot's pool bootstrap, with roomy limits where it declares none.
func (e *poolEnv) ensureRemoteRunners() {
	e.t.Helper()
	proposals := map[string]types.RunnerLimits{}
	for _, id := range e.srv.configuredExecutors() {
		proposals[id] = roomyRemoteLimits()
	}
	if _, _, err := e.pg.EnsureRemoteRunners(context.Background(), e.srv.configuredExecutors(), proposals); err != nil {
		e.t.Fatal(err)
	}
}

func decodeJSON[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
	return v
}

// createPool creates an organization pool as the admin, or a user pool as the first person's own.
func (e *poolEnv) createPool(name string, scope types.RunnerPoolScope) types.RunnerPool {
	e.t.Helper()
	if scope == types.RunnerPoolUser {
		return e.createOwnPool(e.person, name)
	}
	w := e.admin(http.MethodPost, "/api/v1/runner-pools", fmt.Sprintf(`{"name":%q}`, name))
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create pool %q = %d %s", name, w.Code, w.Body)
	}
	return decodeJSON[types.RunnerPool](e.t, w)
}

// createOwnPool creates a user pool of the signed-in person c's own.
func (e *poolEnv) createOwnPool(c *http.Cookie, name string) types.RunnerPool {
	e.t.Helper()
	w := e.call(c, http.MethodPost, "/api/v1/me/runner-pools", fmt.Sprintf(`{"name":%q}`, name))
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create own pool %q = %d %s", name, w.Code, w.Body)
	}
	return decodeJSON[types.RunnerPool](e.t, w)
}

// claimedRunner registers a runner for owner and claims it, bound to this organisation.
func (e *poolEnv) claimedRunner(owner string) uuid.UUID {
	e.t.Helper()
	ctx := context.Background()
	id := uuid.New()
	fp := "fp-" + id.String()
	if _, err := e.pg.CreateRunner(ctx, types.Runner{ID: id, Owner: owner, Name: "laptop", PublicKey: append(id[:], id[:]...), KeyFingerprint: fp,
		OrgURLSHA256: federation.OrgURLSHA256(poolTestOrgURL), Limits: roomyRunnerLimits()}); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.pg.ClaimRunner(ctx, id, owner, fp, time.Now()); err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *poolEnv) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *poolEnv) members(pool uuid.UUID) int {
	return e.count(`SELECT count(*) FROM runner_pool_members WHERE pool_id = $1`, pool)
}

func (e *poolEnv) revision(pool uuid.UUID) int64 {
	e.t.Helper()
	p, err := e.pg.GetRunnerPool(context.Background(), pool)
	if err != nil {
		e.t.Fatal(err)
	}
	return p.Revision
}

func addRunnerPath(pool, runner uuid.UUID) string {
	return "/api/v1/me/runner-pools/" + pool.String() + "/runners/" + runner.String()
}

func (e *poolEnv) listed(c *http.Cookie) map[uuid.UUID]client.RunnerPoolCatalogueEntry {
	e.t.Helper()
	w := e.call(c, http.MethodGet, "/api/v1/runner-pools", "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	out := map[uuid.UUID]client.RunnerPoolCatalogueEntry{}
	for _, p := range decodeJSON[client.RunnerPoolList](e.t, w).Pools {
		out[p.ID] = p
	}
	return out
}

// DONE WHEN (1): a cross-person runner add and a cross-person preference write cause zero rows.
func TestPG_RunnerPoolsCrossPersonWritesCauseNoRows(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createPool("Laptops", types.RunnerPoolUser)
	theirPool := e.createOwnPool(e.other, "Theirs")
	mine := e.claimedRunner(e.personP)
	rev := e.revision(theirPool.ID)

	// The other person names the first person's runner for their own pool: the answer is the one an
	// unknown runner gets, and nothing is written.
	foreign := e.call(e.other, http.MethodPut, addRunnerPath(theirPool.ID, mine), "")
	unknown := e.call(e.other, http.MethodPut, addRunnerPath(theirPool.ID, uuid.New()), "")
	if foreign.Code != http.StatusUnprocessableEntity || wireReason(t, foreign) != string(runnerpool.ReasonMemberMismatch) {
		t.Fatalf("foreign runner add = %d %s, want 422 runner_pool_member_mismatch", foreign.Code, foreign.Body)
	}
	if unknown.Code != foreign.Code || wireReason(t, unknown) != wireReason(t, foreign) {
		t.Errorf("an unknown runner answers %d %s, a foreign one %d %s: they must read alike", unknown.Code, unknown.Body, foreign.Code, foreign.Body)
	}
	if n := e.members(theirPool.ID); n != 0 || e.revision(theirPool.ID) != rev {
		t.Errorf("a cross-person add left %d member rows and revision %d", n, e.revision(theirPool.ID))
	}

	// The owner adds their own runner; the other person cannot take it out.
	if w := e.call(e.person, http.MethodPut, addRunnerPath(pool.ID, mine), ""); w.Code != http.StatusCreated {
		t.Fatalf("own add = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.other, http.MethodDelete, addRunnerPath(pool.ID, mine), ""); w.Code != http.StatusNotFound {
		t.Fatalf("foreign remove = %d %s, want 404: the pool is not theirs", w.Code, w.Body)
	}
	if n := e.members(pool.ID); n != 1 {
		t.Errorf("a cross-person remove took the runner out: %d members", n)
	}

	// A preference write lands on the signed-in person only, and no body can name another principal.
	if w := e.call(e.other, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"principal":%q,"pool_id":%q}`, e.personP, pool.ID)); w.Code != http.StatusBadRequest {
		t.Errorf("a body naming a principal = %d, want 400", w.Code)
	}
	if w := e.call(e.other, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, pool.ID)); w.Code != http.StatusNotFound {
		t.Fatalf("a default naming another person's pool = %d %s, want 404", w.Code, w.Body)
	}
	if w := e.call(e.other, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, theirPool.ID)); w.Code != http.StatusOK {
		t.Fatalf("own default = %d %s", w.Code, w.Body)
	}
	if n := e.count(`SELECT count(*) FROM principal_prefs WHERE key = $1 AND principal <> $2`, types.RunnerPoolDefaultPrefKey, e.otherP); n != 0 {
		t.Errorf("%d preference rows belong to someone other than the signed-in person", n)
	}
	if w := e.call(e.person, http.MethodGet, "/api/v1/me/runner-pool-defaults", ""); decodeJSON[types.RunnerPoolDefaults](t, w).PoolID != nil {
		t.Errorf("the first person reads the other person's default: %s", w.Body)
	}
}

// DONE WHEN (2): an administrative bearer has no personal subject, so it can neither add a runner nor
// set a preference, and a refused one writes nothing.
func TestPG_RunnerPoolsAdministrativeBearerHasNoPersonalSubject(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createPool("Laptops", types.RunnerPoolUser)
	mine := e.claimedRunner(e.personP)

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, addRunnerPath(pool.ID, mine), ""},
		{http.MethodDelete, addRunnerPath(pool.ID, mine), ""},
		{http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, pool.ID)},
		{http.MethodGet, "/api/v1/me/runner-pool-defaults", ""},
		{http.MethodDelete, "/api/v1/me/runner-pool-defaults", ""},
	} {
		w := e.admin(c.method, c.path, c.body)
		if w.Code != http.StatusForbidden || wireReason(t, w) != "runner_claim_mismatch" {
			t.Errorf("admin bearer %s %s = %d %s, want 403 runner_claim_mismatch", c.method, c.path, w.Code, w.Body)
		}
	}
	if n := e.members(pool.ID); n != 0 {
		t.Errorf("an administrative bearer added %d runners", n)
	}
	if n := e.count(`SELECT count(*) FROM principal_prefs WHERE key = $1`, types.RunnerPoolDefaultPrefKey); n != 0 {
		t.Errorf("an administrative bearer wrote %d preference rows", n)
	}
}

// DONE WHEN (3): a pool the caller may not use reads as one that does not exist, everywhere, and the
// organisation-defaults read omits it.
func TestPG_RunnerPoolsInaccessiblePoolIsIndistinguishableFromUnknown(t *testing.T) {
	e := newPoolEnv(t, false)
	open := e.createPool("Open", types.RunnerPoolOrganization)
	secret := e.createPool("Finance only", types.RunnerPoolOrganization)
	if w := e.call(e.alice, http.MethodPut, "/api/v1/runner-pools/"+secret.ID.String()+"/use-policy", `{"subjects":[{"subject_type":"user","subject":"sub-someone-else"}]}`); w.Code != http.StatusOK {
		t.Fatalf("use policy = %d %s", w.Code, w.Body)
	}
	if w := e.admin(http.MethodPut, "/api/v1/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, secret.ID)); w.Code != http.StatusOK {
		t.Fatalf("org default = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.person, http.MethodGet, "/api/v1/runner-pool-defaults", ""); decodeJSON[types.RunnerPoolDefaults](t, w).PoolID != nil {
		t.Errorf("the organisation defaults name a pool this person may not use: %s", w.Body)
	}
	if w := e.call(e.alice, http.MethodGet, "/api/v1/runner-pool-defaults", ""); decodeJSON[types.RunnerPoolDefaults](t, w).PoolID == nil {
		t.Errorf("a manager's read dropped the default: %s", w.Body)
	}

	listed := e.listed(e.person)
	if _, leaks := listed[secret.ID]; leaks || len(listed) != 1 {
		t.Errorf("the catalogue lists %v, want only the open pool", listed)
	}
	if got := e.listed(e.alice); len(got) != 2 {
		t.Errorf("a manager's catalogue lists %d pools, want both", len(got))
	}

	// Every door a person can reach a pool by answers the inaccessible one exactly as an unknown one.
	for _, c := range []struct {
		method string
		build  func(pool uuid.UUID) (path, body string)
	}{
		{http.MethodGet, func(p uuid.UUID) (string, string) { return "/api/v1/runner-pools/" + p.String(), "" }},
		{http.MethodPut, func(p uuid.UUID) (string, string) {
			return "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, p)
		}},
		{http.MethodPut, func(p uuid.UUID) (string, string) { return addRunnerPath(p, uuid.New()), "" }},
		{http.MethodDelete, func(p uuid.UUID) (string, string) { return addRunnerPath(p, uuid.New()), "" }},
	} {
		answer := func(pool uuid.UUID) (int, string) {
			path, body := c.build(pool)
			w := e.call(e.person, c.method, path, body)
			return w.Code, w.Body.String()
		}
		sc, sb := answer(secret.ID)
		gc, gb := answer(uuid.New())
		if sc != http.StatusNotFound || sc != gc || sb != gb {
			t.Errorf("%s: inaccessible = %d %s, unknown = %d %s", c.method, sc, sb, gc, gb)
		}
	}
	if w := e.call(e.person, http.MethodGet, "/api/v1/runner-pools/"+open.ID.String(), ""); w.Code != http.StatusOK {
		t.Errorf("the open pool answers %d", w.Code)
	}
	// An administrator's role grants no pool a use policy withholds from a personal default.
	if w := e.call(e.super, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, secret.ID)); w.Code != http.StatusNotFound {
		t.Errorf("an admin's personal default for a pool they may not use = %d %s, want 404", w.Code, w.Body)
	}
}

// DONE WHEN (4): a runner of the wrong kind for the pool's scope, or one this server is not set up with,
// is refused.
func TestPG_RunnerPoolsRefuseWrongKindMembers(t *testing.T) {
	e := newPoolEnv(t, false)
	remote := e.createPool("Provided", types.RunnerPoolOrganization)
	self := e.createPool("Laptops", types.RunnerPoolUser)
	mine := e.claimedRunner(e.personP)

	if w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+self.ID.String()+"/remote-runners/fake", ""); w.Code != http.StatusNotFound || wireReason(t, w) != string(runnerpool.ReasonNotFound) {
		t.Errorf("an administrator's add to a person's user pool = %d %s, want 404 runner_pool_not_found: no administrator writes one", w.Code, w.Body)
	}
	if w := e.call(e.person, http.MethodPut, addRunnerPath(remote.ID, mine), ""); w.Code != http.StatusUnprocessableEntity ||
		wireReason(t, w) != string(runnerpool.ReasonMemberMismatch) || wireError(t, w) != runnerpool.OrgPoolSelfHostedMsg() {
		t.Errorf("a self-hosted runner in an organization pool = %d %s, want 422 with the ruling's sentence", w.Code, w.Body)
	}
	for _, id := range []string{"not-configured", "Not%20An%20Id", uuid.NewString()} {
		if w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+remote.ID.String()+"/remote-runners/"+id, ""); w.Code != http.StatusUnprocessableEntity ||
			wireReason(t, w) != string(runnerpool.ReasonMemberMismatch) {
			t.Errorf("remote-hosted runner %s that is not set up here = %d %s, want 422", id, w.Code, w.Body)
		}
	}
	if n := e.members(remote.ID) + e.members(self.ID); n != 0 {
		t.Errorf("%d refused members were stored", n)
	}
	if w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+remote.ID.String()+"/remote-runners/fake", ""); w.Code != http.StatusOK {
		t.Fatalf("the configured remote-hosted runner = %d %s", w.Code, w.Body)
	}
	// An unclaimed or revoked runner of the owner is no member either.
	revoked := e.claimedRunner(e.personP)
	if _, err := e.pg.RevokeRunner(context.Background(), revoked, time.Now(), "", ""); err != nil {
		t.Fatal(err)
	}
	if w := e.call(e.person, http.MethodPut, addRunnerPath(self.ID, revoked), ""); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("a revoked runner = %d %s, want 422", w.Code, w.Body)
	}
}

// DONE WHEN (5): a stale revision is runner_pool_stale and changes nothing; a delete stays DELETE.
func TestPG_RunnerPoolsStaleRevisionIsRefused(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createPool("Provided", types.RunnerPoolOrganization)
	path := "/api/v1/runner-pools/" + pool.ID.String()

	if w := e.admin(http.MethodPut, path, `{"revision":1,"name":"Renamed"}`); w.Code != http.StatusOK || decodeJSON[types.RunnerPool](t, w).Revision != 2 {
		t.Fatalf("rename = %d %s", w.Code, w.Body)
	}
	w := e.admin(http.MethodPut, path, `{"revision":1,"name":"Again"}`)
	if w.Code != http.StatusConflict || wireReason(t, w) != string(runnerpool.ReasonStale) {
		t.Errorf("stale rename = %d %s, want 409 runner_pool_stale", w.Code, w.Body)
	}
	if p, _ := e.pg.GetRunnerPool(context.Background(), pool.ID); p.Name != "Renamed" || p.Revision != 2 {
		t.Errorf("a stale update changed the pool: %+v", p)
	}
	// A request that moves nothing keeps the revision and writes no audit row.
	if w := e.admin(http.MethodPut, path, `{"revision":2,"name":"Renamed"}`); w.Code != http.StatusOK || decodeJSON[types.RunnerPool](t, w).Revision != 2 {
		t.Errorf("no-op update = %d %s", w.Code, w.Body)
	}
	if n := len(e.audits(auditPoolUpdate)); n != 1 {
		t.Errorf("%d update audit rows, want 1", n)
	}
	if w := e.admin(http.MethodPut, path, `{"revision":2,"state":"disabled"}`); w.Code != http.StatusOK {
		t.Errorf("disable = %d %s", w.Code, w.Body)
	}
	if w := e.admin(http.MethodPut, path, `{"revision":3,"state":"deleted"}`); w.Code != http.StatusBadRequest || wireReason(t, w) != string(runnerpool.ReasonInvalid) {
		t.Errorf("state deleted = %d %s, want 400 runner_pool_invalid", w.Code, w.Body)
	}
	if p, _ := e.pg.GetRunnerPool(context.Background(), pool.ID); p.State != types.RunnerPoolDisabled {
		t.Errorf("an update reached state %q", p.State)
	}
	// A pool name is unique among the organisation's live pools.
	if w := e.admin(http.MethodPost, "/api/v1/runner-pools", `{"name":"renamed"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a duplicate name = %d %s, want 400", w.Code, w.Body)
	}
}

// DONE WHEN (7): revoking a runner and tidying it away never depend on its pool; a revoked runner stops
// counting as the owner's.
func TestPG_RunnerPoolsRevokeIsIndependentOfMembership(t *testing.T) {
	e := newPoolEnv(t, false)
	if _, err := e.pg.PutSiteConfig(context.Background(), types.SiteConfig{Runners: &types.RunnerSettings{Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	pool := e.createPool("Laptops", types.RunnerPoolUser)
	mine := e.claimedRunner(e.personP)
	e.srv.cfg.RunnerOnline = func(id uuid.UUID) bool { return id == mine }
	if w := e.call(e.person, http.MethodPut, addRunnerPath(pool.ID, mine), ""); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	if got := e.listed(e.person)[pool.ID]; got.Availability != client.RunnerPoolAvailable {
		t.Fatalf("with an own runner the pool reads %+v", got)
	}
	if got, listed := e.listed(e.other)[pool.ID]; listed {
		t.Errorf("another person's list shows the person's own pool: %+v", got)
	}
	if _, err := e.pg.RevokeRunner(context.Background(), mine, time.Now(), "", ""); err != nil {
		t.Fatalf("revoking a pool member: %v", err)
	}
	if got := e.listed(e.person)[pool.ID]; got.Availability != client.RunnerPoolUnavailable {
		t.Errorf("a revoked runner still makes the pool available: %+v", got)
	}
	if w := e.call(e.person, http.MethodDelete, addRunnerPath(pool.ID, mine), ""); w.Code != http.StatusNoContent || e.members(pool.ID) != 0 {
		t.Errorf("tidying a revoked runner away = %d, %d members left", w.Code, e.members(pool.ID))
	}
	// Deleting a pool never blocks revoking what was in it.
	other := e.claimedRunner(e.personP)
	e.call(e.person, http.MethodPut, addRunnerPath(pool.ID, other), "")
	if w := e.call(e.person, http.MethodDelete, "/api/v1/me/runner-pools/"+pool.ID.String(), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete pool = %d %s", w.Code, w.Body)
	}
	if _, err := e.pg.RevokeRunner(context.Background(), other, time.Now(), "", ""); err != nil {
		t.Errorf("revoking a runner of a deleted pool: %v", err)
	}
	if e.members(pool.ID) != 0 || e.call(e.person, http.MethodGet, "/api/v1/runner-pools/"+pool.ID.String(), "").Code != http.StatusNotFound {
		t.Errorf("a deleted pool keeps members or stays readable")
	}
}

// DONE WHEN (8): the legacy bootstrap makes one organization pool of the configured remote-hosted
// runners, as the organisation's default pool, with no use policy and no user pool, once; it leaves runs
// without a pool.
func TestPG_RunnerPoolsLegacyBootstrapIsOneOrganizationPoolOnce(t *testing.T) {
	e := newPoolEnv(t, false)
	ctx := context.Background()
	run := e.count(`SELECT count(*) FROM agent_runs`)

	if err := e.srv.BootstrapRunnerPools(ctx); err != nil {
		t.Fatal(err)
	}
	pools := e.listed(e.person)
	if len(pools) != 1 {
		t.Fatalf("bootstrap made %d pools visible to a member, want 1", len(pools))
	}
	var bootstrapped client.RunnerPoolCatalogueEntry
	for _, p := range pools {
		bootstrapped = p
	}
	if bootstrapped.Scope != types.RunnerPoolOrganization || bootstrapped.Availability != client.RunnerPoolAvailable || bootstrapped.Name != "Organization" {
		t.Errorf("bootstrapped pool = %+v", bootstrapped)
	}
	d, _ := e.pg.GetRunnerPoolOrgDefaults(ctx)
	if d.PoolID == nil || *d.PoolID != bootstrapped.ID {
		t.Errorf("organisation defaults = %+v", d)
	}
	if n := e.count(`SELECT count(*) FROM runner_pool_use_policies`) + e.count(`SELECT count(*) FROM runner_pools WHERE scope = 'user'`) +
		e.count(`SELECT count(*) FROM remote_runners WHERE people_pools <> 'off'`); n != 0 {
		t.Errorf("bootstrap widened or narrowed something: %d policy, user pool or people-policy rows", n)
	}
	if e.count(`SELECT count(*) FROM runner_pool_members WHERE remote_runner_id = 'fake'`) != 1 {
		t.Errorf("the pool does not hold the configured remote-hosted runner")
	}
	if n := e.count(`SELECT count(*) FROM agent_runs WHERE runner_pool_id IS NOT NULL OR runner_pool_selection <> ''`); n != 0 || e.count(`SELECT count(*) FROM agent_runs`) != run {
		t.Errorf("bootstrap bound %d runs to a pool, %d runs (was %d)", n, e.count(`SELECT count(*) FROM agent_runs`), run)
	}
	if len(e.audits(auditPoolCreate)) != 1 || len(e.audits(auditPoolDefaultSet)) != 1 {
		t.Errorf("bootstrap audit rows: %d create, %d default", len(e.audits(auditPoolCreate)), len(e.audits(auditPoolDefaultSet)))
	}

	// Once: a pool an administrator deletes is not made again, and a second boot adds nothing.
	if err := e.srv.BootstrapRunnerPools(ctx); err != nil {
		t.Fatal(err)
	}
	if w := e.admin(http.MethodDelete, "/api/v1/runner-pools/"+bootstrapped.ID.String(), ""); w.Code != http.StatusNoContent {
		t.Fatal(w.Body)
	}
	if err := e.srv.BootstrapRunnerPools(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.listed(e.person); len(got) != 0 {
		t.Errorf("a deleted bootstrap pool came back: %v", got)
	}
}

// A deployment whose administrators already made pools keeps exactly those; one with no configured
// remote-hosted runner records nothing and so bootstraps once one is configured.
func TestPG_RunnerPoolsBootstrapLeavesExistingPoolsAndNeedsExecutors(t *testing.T) {
	bare := newPoolEnvWith(t, func(c *Config) { c.Runner = nil })
	if err := bare.srv.BootstrapRunnerPools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if bare.count(`SELECT count(*) FROM runner_pools`)+bare.count(`SELECT count(*) FROM runner_pool_org_defaults`) != 0 {
		t.Errorf("a deployment with no remote-hosted runner recorded a bootstrap")
	}
	e := newPoolEnv(t, false)
	e.createPool("Ours", types.RunnerPoolOrganization)
	if err := e.srv.BootstrapRunnerPools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.listed(e.person); len(got) != 1 {
		t.Errorf("bootstrap added to an administrator's catalogue: %v", got)
	}
	if d, _ := e.pg.GetRunnerPoolOrgDefaults(context.Background()); d.PoolID != nil {
		t.Errorf("bootstrap set a default over an administrator's catalogue: %+v", d)
	}
}

func newPoolEnvWith(t *testing.T, mutate func(*Config)) *poolEnv {
	t.Helper()
	return &poolEnv{govEnv: newGovEnvOn(t, throwawayBarePGPool(t), mutate)}
}

// Defaults: revalidated on write for id, scope (the organisation's names an organization pool) and
// state; a person's own record
// only; cleared on DELETE; and no generic config door writes them.
func TestPG_RunnerPoolsDefaultsAreRevalidatedAndPersonal(t *testing.T) {
	e := newPoolEnv(t, false)
	remote := e.createPool("Provided", types.RunnerPoolOrganization)
	self := e.createPool("Laptops", types.RunnerPoolUser)
	e.admin(http.MethodPut, "/api/v1/runner-pools/"+remote.ID.String()+"/remote-runners/fake", "")

	put := func(c *http.Cookie, path, body string) *httptest.ResponseRecorder {
		if c == nil {
			return e.admin(http.MethodPut, path, body)
		}
		return e.call(c, http.MethodPut, path, body)
	}
	for _, c := range []*http.Cookie{nil, e.person} {
		path := "/api/v1/runner-pool-defaults"
		if c != nil {
			path = "/api/v1/me/runner-pool-defaults"
		}
		if w := put(c, path, fmt.Sprintf(`{"pool_id":%q}`, uuid.New())); w.Code != http.StatusNotFound {
			t.Errorf("PUT %s with an unknown pool = %d %s", path, w.Code, w.Body)
		}
		for _, old := range []string{`{"preferred_hosting":"remote_provided"}`, fmt.Sprintf(`{"remote_provided":%q}`, remote.ID)} {
			if w := put(c, path, old); w.Code != http.StatusBadRequest {
				t.Errorf("PUT %s with the old shape %s = %d %s, want 400", path, old, w.Code, w.Body)
			}
		}
	}
	if w := e.call(e.person, http.MethodPut, "/api/v1/runner-pool-defaults", `{}`); w.Code != http.StatusForbidden {
		t.Errorf("a member writing the organisation defaults = %d, want 403", w.Code)
	}

	// The organisation's default is an organization pool: a person's user pool is refused by name, and
	// nothing is stored.
	if w := put(nil, "/api/v1/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, self.ID)); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), noOrgUserPoolDefaultMsg) {
		t.Errorf("an organisation default naming a user pool = %d %s, want 400 refused by name", w.Code, w.Body)
	}
	if n := e.count(`SELECT count(*) FROM runner_pool_org_defaults`); n != 0 {
		t.Errorf("a refused organisation default stored %d rows", n)
	}
	if w := put(nil, "/api/v1/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, remote.ID)); w.Code != http.StatusOK {
		t.Fatalf("org defaults = %d %s", w.Code, w.Body)
	}
	// A person's own default may be an organization pool or one of their own user pools.
	for _, p := range []types.RunnerPool{remote, self} {
		if w := put(e.person, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, p.ID)); w.Code != http.StatusOK {
			t.Fatalf("personal default %s = %d %s", p.Name, w.Code, w.Body)
		}
	}
	// A pool that is switched off is not a default to set.
	if w := e.call(e.person, http.MethodPut, "/api/v1/me/runner-pools/"+self.ID.String(), `{"revision":1,"state":"disabled"}`); w.Code != http.StatusOK {
		t.Fatalf("switch off own pool = %d %s", w.Code, w.Body)
	}
	if w := put(e.person, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, self.ID)); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(runnerpool.ReasonUnavailable) {
		t.Errorf("a disabled pool as a default = %d %s, want 422 runner_pool_unavailable", w.Code, w.Body)
	}
	if w := e.call(e.person, http.MethodDelete, "/api/v1/me/runner-pool-defaults", ""); w.Code != http.StatusNoContent {
		t.Fatalf("clear = %d", w.Code)
	}
	if w := e.call(e.person, http.MethodGet, "/api/v1/me/runner-pool-defaults", ""); w.Body.String() != "{}\n" {
		t.Errorf("a cleared record reads %q, want {}", w.Body)
	}
	if len(e.audits(auditPersonDefaultSet)) != 2 || len(e.audits(auditPersonDefaultClear)) != 1 || len(e.audits(auditPoolDefaultSet)) != 1 {
		t.Errorf("default audit rows: person set %d, clear %d, org %d", len(e.audits(auditPersonDefaultSet)), len(e.audits(auditPersonDefaultClear)), len(e.audits(auditPoolDefaultSet)))
	}

	// The generic site-config document is never a second writer of pools or defaults.
	for _, key := range []string{"runner_pools", "runner_pool_defaults"} {
		if w := e.admin(http.MethodPut, "/api/v1/site-config", fmt.Sprintf(`{%q:{"pool_id":%q}}`, key, remote.ID)); w.Code != http.StatusBadRequest {
			t.Errorf("PUT /site-config carrying %s = %d %s, want 400", key, w.Code, w.Body)
		}
	}
}

// The catalogue reads and writes leave an audit row each, with no secret, and the routes a stable pool
// lifecycle: create, remote-hosted runner membership, update, delete.
func TestPG_RunnerPoolsManagementIsAudited(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createPool("Provided", types.RunnerPoolOrganization)
	path := "/api/v1/runner-pools/" + pool.ID.String()
	e.admin(http.MethodPut, path+"/remote-runners/fake", "")
	e.admin(http.MethodPut, path+"/remote-runners/fake", "") // idempotent: no second row
	e.admin(http.MethodPut, path, `{"revision":2,"name":"Renamed"}`)
	e.admin(http.MethodDelete, path+"/remote-runners/fake", "")
	e.admin(http.MethodDelete, path, "")
	for action, want := range map[string]int{auditPoolCreate: 1, auditPoolRunnerAdd: 1, auditPoolUpdate: 1, auditPoolRunnerRemove: 1, auditPoolDelete: 1} {
		if got := len(e.audits(action)); got != want {
			t.Errorf("%s rows = %d, want %d", action, got, want)
		}
	}
	rows := e.audits(auditPoolUpdate)
	if len(rows) == 1 {
		if d := auditData(t, rows[0]); d["name"] != "Renamed" || d["name_before"] != "Provided" || d["revision"] != float64(3) {
			t.Errorf("update audit data = %v", d)
		}
	}
}

// racingPoolRead runs after the pool read of an update, the way another admin's write lands between
// that read and the store call.
type racingPoolRead struct {
	store.PG
	after func()
}

func (r *racingPoolRead) GetRunnerPool(ctx context.Context, id uuid.UUID) (types.RunnerPool, error) {
	p, err := r.PG.GetRunnerPool(ctx, id)
	if r.after != nil {
		r.after()
		r.after = nil
	}
	return p, err
}

// An update naming the revision another admin's write just made is refused, not answered as a store
// no-op that audits a change against the pool as it stood before.
func TestPG_RunnerPoolUpdateNamingTheNextRevisionWritesNoFalseAuditRow(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createPool("Provided", types.RunnerPoolOrganization)
	renamed := "Renamed"
	e.srv.cfg.Store = &racingPoolRead{PG: e.pg, after: func() {
		if _, err := e.pg.UpdateRunnerPool(context.Background(), pool.ID, 1, &renamed, nil, nil); err != nil {
			t.Error(err)
		}
	}}
	w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+pool.ID.String(), `{"revision":2,"name":"Renamed"}`)
	if w.Code != http.StatusConflict || wireReason(t, w) != string(runnerpool.ReasonStale) {
		t.Errorf("update naming the next revision = %d %s, want 409 runner_pool_stale", w.Code, w.Body)
	}
	if n := len(e.audits(auditPoolUpdate)); n != 0 {
		t.Errorf("%d update audit rows for a request that wrote nothing", n)
	}
}

// A write landing between the pool read and the store call: the request names the revision it read,
// the pre-check passes, and the store's own revision check refuses it, with no update audit row.
func TestPG_RunnerPoolUpdateLosingTheRaceToTheStoreIsStale(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createPool("Provided", types.RunnerPoolOrganization)
	other := "Other"
	e.srv.cfg.Store = &racingPoolRead{PG: e.pg, after: func() {
		if _, err := e.pg.UpdateRunnerPool(context.Background(), pool.ID, 1, &other, nil, nil); err != nil {
			t.Error(err)
		}
	}}
	w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+pool.ID.String(), `{"revision":1,"name":"Renamed"}`)
	if w.Code != http.StatusConflict || wireReason(t, w) != string(runnerpool.ReasonStale) {
		t.Errorf("update that lost the race = %d %s, want 409 runner_pool_stale", w.Code, w.Body)
	}
	if n := len(e.audits(auditPoolUpdate)); n != 0 {
		t.Errorf("%d update audit rows for a request that wrote nothing", n)
	}
	if got, err := e.pg.GetRunnerPool(context.Background(), pool.ID); err != nil || got.Name != "Other" {
		t.Errorf("pool = %+v, %v; want the other admin's rename kept", got, err)
	}
}
