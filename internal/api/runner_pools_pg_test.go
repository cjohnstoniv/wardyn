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
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const poolTestOrgURL = "https://org.example.com"

// poolEnv is a deployment with pool storage, one configured executor ("fake"), the four-eyes switch
// off, and three people: alice (security admin), super (admin) and two members.
type poolEnv struct {
	*govEnv
	person  *http.Cookie
	other   *http.Cookie
	personP string
	otherP  string
}

func newPoolEnv(t *testing.T, secondHuman bool) *poolEnv {
	t.Helper()
	mutate := func(c *Config) { c.Runner = &fakeRunner{}; c.RunnerOrgURL = poolTestOrgURL }
	var g *govEnv
	if secondHuman {
		g = newGovEnv(t, mutate)
	} else {
		g = newGovEnvOn(t, throwawayPGPool(t), mutate)
	}
	return &poolEnv{
		govEnv: g,
		person: ssoSession(t, "sub-person", "person@corp.example", "user"), personP: "sub-person",
		other: ssoSession(t, "sub-other", "other@corp.example", "user"), otherP: "sub-other",
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

func (e *poolEnv) createPool(name string, hosting types.RunnerPoolHosting) types.RunnerPool {
	e.t.Helper()
	w := e.admin(http.MethodPost, "/api/v1/runner-pools", fmt.Sprintf(`{"name":%q,"hosting_type":%q}`, name, hosting))
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create pool %q = %d %s", name, w.Code, w.Body)
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
		OrgURLSHA256: federation.OrgURLSHA256(poolTestOrgURL)}); err != nil {
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

func (e *poolEnv) listed(c *http.Cookie) map[uuid.UUID]client.RunnerPoolChoice {
	e.t.Helper()
	w := e.call(c, http.MethodGet, "/api/v1/runner-pools", "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("list = %d %s", w.Code, w.Body)
	}
	out := map[uuid.UUID]client.RunnerPoolChoice{}
	for _, p := range decodeJSON[client.RunnerPoolList](e.t, w).Pools {
		out[p.ID] = p
	}
	return out
}

// DONE WHEN (1): a cross-person runner add and a cross-person preference write cause zero rows.
func TestPG_RunnerPoolsCrossPersonWritesCauseNoRows(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createPool("Laptops", types.RunnerPoolSelfHosted)
	mine := e.claimedRunner(e.personP)
	rev := e.revision(pool.ID)

	// The other person names the first person's runner: the answer is the one an unknown runner gets,
	// and nothing is written.
	foreign := e.call(e.other, http.MethodPut, addRunnerPath(pool.ID, mine), "")
	unknown := e.call(e.other, http.MethodPut, addRunnerPath(pool.ID, uuid.New()), "")
	if foreign.Code != http.StatusUnprocessableEntity || wireReason(t, foreign) != string(runnerpool.ReasonMemberMismatch) {
		t.Fatalf("foreign runner add = %d %s, want 422 runner_pool_member_mismatch", foreign.Code, foreign.Body)
	}
	if unknown.Code != foreign.Code || wireReason(t, unknown) != wireReason(t, foreign) {
		t.Errorf("an unknown runner answers %d %s, a foreign one %d %s: they must read alike", unknown.Code, unknown.Body, foreign.Code, foreign.Body)
	}
	if n := e.members(pool.ID); n != 0 || e.revision(pool.ID) != rev {
		t.Errorf("a cross-person add left %d member rows and revision %d", n, e.revision(pool.ID))
	}

	// The owner adds their own runner; the other person cannot take it out.
	if w := e.call(e.person, http.MethodPut, addRunnerPath(pool.ID, mine), ""); w.Code != http.StatusCreated {
		t.Fatalf("own add = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.other, http.MethodDelete, addRunnerPath(pool.ID, mine), ""); w.Code != http.StatusNoContent {
		t.Fatalf("foreign remove = %d %s", w.Code, w.Body)
	}
	if n := e.members(pool.ID); n != 1 {
		t.Errorf("a cross-person remove took the runner out: %d members", n)
	}

	// A preference write lands on the signed-in person only, and no body can name another principal.
	if w := e.call(e.other, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"principal":%q,"self_hosted":%q}`, e.personP, pool.ID)); w.Code != http.StatusBadRequest {
		t.Errorf("a body naming a principal = %d, want 400", w.Code)
	}
	if w := e.call(e.other, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"self_hosted":%q}`, pool.ID)); w.Code != http.StatusOK {
		t.Fatalf("own default = %d %s", w.Code, w.Body)
	}
	if n := e.count(`SELECT count(*) FROM principal_prefs WHERE key = $1 AND principal <> $2`, types.RunnerPoolDefaultsPrefKey, e.otherP); n != 0 {
		t.Errorf("%d preference rows belong to someone other than the signed-in person", n)
	}
	if w := e.call(e.person, http.MethodGet, "/api/v1/me/runner-pool-defaults", ""); decodeJSON[types.RunnerPoolDefaults](t, w).SelfHosted != nil {
		t.Errorf("the first person reads the other person's default: %s", w.Body)
	}
}

// DONE WHEN (2): an administrative bearer has no personal subject, so it can neither add a runner nor
// set a preference, and a refused one writes nothing.
func TestPG_RunnerPoolsAdministrativeBearerHasNoPersonalSubject(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createPool("Laptops", types.RunnerPoolSelfHosted)
	mine := e.claimedRunner(e.personP)

	for _, c := range []struct{ method, path, body string }{
		{http.MethodPut, addRunnerPath(pool.ID, mine), ""},
		{http.MethodDelete, addRunnerPath(pool.ID, mine), ""},
		{http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"self_hosted":%q}`, pool.ID)},
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
	if n := e.count(`SELECT count(*) FROM principal_prefs WHERE key = $1`, types.RunnerPoolDefaultsPrefKey); n != 0 {
		t.Errorf("an administrative bearer wrote %d preference rows", n)
	}
}

// DONE WHEN (3): a pool the caller may not use reads as one that does not exist, everywhere, and the
// organisation-defaults read omits it.
func TestPG_RunnerPoolsInaccessiblePoolIsIndistinguishableFromUnknown(t *testing.T) {
	e := newPoolEnv(t, false)
	open := e.createPool("Open", types.RunnerPoolRemoteProvided)
	secret := e.createPool("Finance only", types.RunnerPoolRemoteProvided)
	if w := e.call(e.alice, http.MethodPut, "/api/v1/runner-pools/"+secret.ID.String()+"/use-policy", `{"subjects":[{"subject_type":"user","subject":"sub-someone-else"}]}`); w.Code != http.StatusOK {
		t.Fatalf("use policy = %d %s", w.Code, w.Body)
	}
	if w := e.admin(http.MethodPut, "/api/v1/runner-pool-defaults", fmt.Sprintf(`{"remote_provided":%q}`, secret.ID)); w.Code != http.StatusOK {
		t.Fatalf("org default = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.person, http.MethodGet, "/api/v1/runner-pool-defaults", ""); decodeJSON[types.RunnerPoolDefaults](t, w).RemoteProvided != nil {
		t.Errorf("the organisation defaults name a pool this person may not use: %s", w.Body)
	}
	if w := e.call(e.alice, http.MethodGet, "/api/v1/runner-pool-defaults", ""); decodeJSON[types.RunnerPoolDefaults](t, w).RemoteProvided == nil {
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
			return "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"remote_provided":%q}`, p)
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
	if w := e.call(e.super, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"remote_provided":%q}`, secret.ID)); w.Code != http.StatusNotFound {
		t.Errorf("an admin's personal default for a pool they may not use = %d %s, want 404", w.Code, w.Body)
	}
}

// DONE WHEN (4): a member of the wrong kind or for the wrong hosting type is refused.
func TestPG_RunnerPoolsRefuseMixedAndWrongHostingMembers(t *testing.T) {
	e := newPoolEnv(t, false)
	remote := e.createPool("Provided", types.RunnerPoolRemoteProvided)
	self := e.createPool("Laptops", types.RunnerPoolSelfHosted)
	mine := e.claimedRunner(e.personP)

	if w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+self.ID.String()+"/executors/fake", ""); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(runnerpool.ReasonMemberMismatch) {
		t.Errorf("an executor in a self-hosted pool = %d %s, want 422 runner_pool_member_mismatch", w.Code, w.Body)
	}
	if w := e.call(e.person, http.MethodPut, addRunnerPath(remote.ID, mine), ""); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(runnerpool.ReasonMemberMismatch) {
		t.Errorf("a runner in a remote-provided pool = %d %s, want 422 runner_pool_member_mismatch", w.Code, w.Body)
	}
	if w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+remote.ID.String()+"/executors/not-configured", ""); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("an executor that is not configured = %d %s, want 422", w.Code, w.Body)
	}
	if w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+remote.ID.String()+"/executors/Not%20An%20Id", ""); w.Code != http.StatusBadRequest {
		t.Errorf("a malformed executor id = %d %s, want 400", w.Code, w.Body)
	}
	if n := e.members(remote.ID) + e.members(self.ID); n != 0 {
		t.Errorf("%d refused members were stored", n)
	}
	if w := e.admin(http.MethodPut, "/api/v1/runner-pools/"+remote.ID.String()+"/executors/fake", ""); w.Code != http.StatusOK {
		t.Fatalf("the configured executor = %d %s", w.Code, w.Body)
	}
	// An unclaimed or revoked runner of the owner is no member either.
	revoked := e.claimedRunner(e.personP)
	if _, err := e.pg.RevokeRunner(context.Background(), revoked, time.Now()); err != nil {
		t.Fatal(err)
	}
	if w := e.call(e.person, http.MethodPut, addRunnerPath(self.ID, revoked), ""); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("a revoked runner = %d %s, want 422", w.Code, w.Body)
	}
}

// DONE WHEN (5): a stale revision is runner_pool_stale and changes nothing; a delete stays DELETE.
func TestPG_RunnerPoolsStaleRevisionIsRefused(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createPool("Provided", types.RunnerPoolRemoteProvided)
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
	// A pool name is unique among live pools.
	if w := e.admin(http.MethodPost, "/api/v1/runner-pools", `{"name":"renamed","hosting_type":"self_hosted"}`); w.Code != http.StatusBadRequest {
		t.Errorf("a duplicate name = %d %s, want 400", w.Code, w.Body)
	}
}

// DONE WHEN (7): revoking a runner and tidying it away never depend on its pool; a revoked runner stops
// counting as the owner's.
func TestPG_RunnerPoolsRevokeIsIndependentOfMembership(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createPool("Laptops", types.RunnerPoolSelfHosted)
	mine := e.claimedRunner(e.personP)
	if w := e.call(e.person, http.MethodPut, addRunnerPath(pool.ID, mine), ""); w.Code != http.StatusCreated {
		t.Fatalf("add = %d %s", w.Code, w.Body)
	}
	if got := e.listed(e.person)[pool.ID]; got.Availability != client.RunnerPoolAvailable {
		t.Fatalf("with an own runner the pool reads %+v", got)
	}
	if got := e.listed(e.other)[pool.ID]; got.Availability != client.RunnerPoolUnavailable || got.Reason != runnerpool.ReasonNoEligibleMember {
		t.Errorf("another person's view of the pool reads %+v, want unavailable runner_pool_no_eligible_member", got)
	}
	if _, err := e.pg.RevokeRunner(context.Background(), mine, time.Now()); err != nil {
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
	if w := e.admin(http.MethodDelete, "/api/v1/runner-pools/"+pool.ID.String(), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete pool = %d %s", w.Code, w.Body)
	}
	if _, err := e.pg.RevokeRunner(context.Background(), other, time.Now()); err != nil {
		t.Errorf("revoking a runner of a deleted pool: %v", err)
	}
	if e.members(pool.ID) != 0 || e.call(e.person, http.MethodGet, "/api/v1/runner-pools/"+pool.ID.String(), "").Code != http.StatusNotFound {
		t.Errorf("a deleted pool keeps members or stays readable")
	}
}

// DONE WHEN (8): the legacy bootstrap makes one Remote Provided pool of the configured executors, as
// the organisation's remote default, with no use policy and no self-hosted pool, once; it leaves runs
// without a pool.
func TestPG_RunnerPoolsLegacyBootstrapIsOneRemoteProvidedPoolOnce(t *testing.T) {
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
	var bootstrapped client.RunnerPoolChoice
	for _, p := range pools {
		bootstrapped = p
	}
	if bootstrapped.HostingType != types.RunnerPoolRemoteProvided || bootstrapped.Availability != client.RunnerPoolAvailable || bootstrapped.Name != "Remote Provided" {
		t.Errorf("bootstrapped pool = %+v", bootstrapped)
	}
	d, _ := e.pg.GetRunnerPoolOrgDefaults(ctx)
	if d.RemoteProvided == nil || *d.RemoteProvided != bootstrapped.ID || d.SelfHosted != nil {
		t.Errorf("organisation defaults = %+v", d)
	}
	if n := e.count(`SELECT count(*) FROM runner_pool_use_policies`) + e.count(`SELECT count(*) FROM runner_pools WHERE hosting_type = 'self_hosted'`); n != 0 {
		t.Errorf("bootstrap widened or narrowed something: %d policy/self-hosted rows", n)
	}
	if e.count(`SELECT count(*) FROM runner_pool_members WHERE executor_id = 'fake'`) != 1 {
		t.Errorf("the pool does not hold the configured executor")
	}
	if n := e.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'agent_runs' AND column_name LIKE '%pool%'`); n != 0 || e.count(`SELECT count(*) FROM agent_runs`) != run {
		t.Errorf("bootstrap touched runs: %d pool columns, %d runs (was %d)", n, e.count(`SELECT count(*) FROM agent_runs`), run)
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
// executor records nothing and so bootstraps once one is configured.
func TestPG_RunnerPoolsBootstrapLeavesExistingPoolsAndNeedsExecutors(t *testing.T) {
	bare := newPoolEnvWith(t, func(c *Config) { c.Runner = nil })
	if err := bare.srv.BootstrapRunnerPools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if bare.count(`SELECT count(*) FROM runner_pools`)+bare.count(`SELECT count(*) FROM runner_pool_org_defaults`) != 0 {
		t.Errorf("a deployment with no executor recorded a bootstrap")
	}
	e := newPoolEnv(t, false)
	e.createPool("Mine", types.RunnerPoolSelfHosted)
	if err := e.srv.BootstrapRunnerPools(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.listed(e.person); len(got) != 1 {
		t.Errorf("bootstrap added to an administrator's catalogue: %v", got)
	}
	if d, _ := e.pg.GetRunnerPoolOrgDefaults(context.Background()); d.RemoteProvided != nil {
		t.Errorf("bootstrap set a default over an administrator's catalogue: %+v", d)
	}
}

func newPoolEnvWith(t *testing.T, mutate func(*Config)) *poolEnv {
	t.Helper()
	return &poolEnv{govEnv: newGovEnvOn(t, throwawayPGPool(t), mutate)}
}

// Defaults: revalidated on write for id, hosting type of the slot and state; a person's own record
// only; cleared on DELETE; and no generic config door writes them.
func TestPG_RunnerPoolsDefaultsAreRevalidatedAndPersonal(t *testing.T) {
	e := newPoolEnv(t, false)
	remote := e.createPool("Provided", types.RunnerPoolRemoteProvided)
	self := e.createPool("Laptops", types.RunnerPoolSelfHosted)
	e.admin(http.MethodPut, "/api/v1/runner-pools/"+remote.ID.String()+"/executors/fake", "")

	put := func(c *http.Cookie, path, body string) *httptest.ResponseRecorder {
		if c == nil {
			return e.admin(http.MethodPut, path, body)
		}
		return e.call(c, http.MethodPut, path, body)
	}
	wrongSlot := fmt.Sprintf(`{"remote_provided":%q}`, self.ID)
	for _, c := range []*http.Cookie{nil, e.person} {
		path := "/api/v1/runner-pool-defaults"
		if c != nil {
			path = "/api/v1/me/runner-pool-defaults"
		}
		if w := put(c, path, wrongSlot); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(runnerpool.ReasonMemberMismatch) {
			t.Errorf("PUT %s with a self-hosted pool in the remote slot = %d %s", path, w.Code, w.Body)
		}
		if w := put(c, path, fmt.Sprintf(`{"remote_provided":%q}`, uuid.New())); w.Code != http.StatusNotFound {
			t.Errorf("PUT %s with an unknown pool = %d %s", path, w.Code, w.Body)
		}
		if w := put(c, path, `{"preferred_hosting":"nowhere"}`); w.Code != http.StatusBadRequest {
			t.Errorf("PUT %s with a bad hosting type = %d %s", path, w.Code, w.Body)
		}
	}
	if w := e.call(e.person, http.MethodPut, "/api/v1/runner-pool-defaults", `{}`); w.Code != http.StatusForbidden {
		t.Errorf("a member writing the organisation defaults = %d, want 403", w.Code)
	}

	good := fmt.Sprintf(`{"preferred_hosting":"self_hosted","remote_provided":%q,"self_hosted":%q}`, remote.ID, self.ID)
	if w := put(nil, "/api/v1/runner-pool-defaults", good); w.Code != http.StatusOK {
		t.Fatalf("org defaults = %d %s", w.Code, w.Body)
	}
	if w := put(e.person, "/api/v1/me/runner-pool-defaults", good); w.Code != http.StatusOK {
		t.Fatalf("personal defaults = %d %s", w.Code, w.Body)
	}
	// A pool that is switched off is not a default to set.
	e.admin(http.MethodPut, "/api/v1/runner-pools/"+self.ID.String(), `{"revision":1,"state":"disabled"}`)
	if w := put(e.person, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"self_hosted":%q}`, self.ID)); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != string(runnerpool.ReasonUnavailable) {
		t.Errorf("a disabled pool as a default = %d %s, want 422 runner_pool_unavailable", w.Code, w.Body)
	}
	if w := e.call(e.person, http.MethodDelete, "/api/v1/me/runner-pool-defaults", ""); w.Code != http.StatusNoContent {
		t.Fatalf("clear = %d", w.Code)
	}
	if w := e.call(e.person, http.MethodGet, "/api/v1/me/runner-pool-defaults", ""); w.Body.String() != "{}\n" {
		t.Errorf("a cleared record reads %q, want {}", w.Body)
	}
	if len(e.audits(auditPersonDefaultSet)) != 1 || len(e.audits(auditPersonDefaultClear)) != 1 || len(e.audits(auditPoolDefaultSet)) != 1 {
		t.Errorf("default audit rows: person set %d, clear %d, org %d", len(e.audits(auditPersonDefaultSet)), len(e.audits(auditPersonDefaultClear)), len(e.audits(auditPoolDefaultSet)))
	}

	// The generic site-config document is never a second writer of pools or defaults.
	for _, key := range []string{"runner_pools", "runner_pool_defaults"} {
		if w := e.admin(http.MethodPut, "/api/v1/site-config", fmt.Sprintf(`{%q:{"remote_provided":%q}}`, key, remote.ID)); w.Code != http.StatusBadRequest {
			t.Errorf("PUT /site-config carrying %s = %d %s, want 400", key, w.Code, w.Body)
		}
	}
}

// The catalogue reads and writes leave an audit row each, with no secret, and the routes a stable pool
// lifecycle: create, executor membership, update, delete.
func TestPG_RunnerPoolsManagementIsAudited(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createPool("Provided", types.RunnerPoolRemoteProvided)
	path := "/api/v1/runner-pools/" + pool.ID.String()
	e.admin(http.MethodPut, path+"/executors/fake", "")
	e.admin(http.MethodPut, path+"/executors/fake", "") // idempotent: no second row
	e.admin(http.MethodPut, path, `{"revision":2,"name":"Renamed"}`)
	e.admin(http.MethodDelete, path+"/executors/fake", "")
	e.admin(http.MethodDelete, path, "")
	for action, want := range map[string]int{auditPoolCreate: 1, auditPoolExecutorAdd: 1, auditPoolUpdate: 1, auditPoolExecutorRemove: 1, auditPoolDelete: 1} {
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
