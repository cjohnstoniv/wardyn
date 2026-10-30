// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Round 1 of the review: what a single administrator cannot do around a held use policy, what a leaver
// purge leaves in one, what a manager is told, and the revisions and rows the audit trail records.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func (e *poolEnv) rawRunner(owner, orgURL string, claim bool) uuid.UUID {
	e.t.Helper()
	ctx := context.Background()
	id := uuid.New()
	fp := "fp-" + id.String()
	if _, err := e.pg.CreateRunner(ctx, types.Runner{ID: id, Owner: owner, Name: "laptop", PublicKey: append(id[:], id[:]...), KeyFingerprint: fp,
		OrgURLSHA256: federation.OrgURLSHA256(orgURL), Limits: roomyRunnerLimits()}); err != nil {
		e.t.Fatal(err)
	}
	if claim {
		if _, err := e.pg.ClaimRunner(ctx, id, owner, fp, time.Now()); err != nil {
			e.t.Fatal(err)
		}
	}
	return id
}

func poolPath(id uuid.UUID) string { return "/api/v1/runner-pools/" + id.String() }

// A narrowed executor cannot be put in front of a wider audience by one administrator: the attach is
// held, a pool with a policy is not deleted until the policy is cleared (which is held), and a held
// change goes stale when the pool's executors moved.
func TestPG_RunnerPoolUsePolicyCannotBeBypassedOnTheAdminTier(t *testing.T) {
	e := newPoolEnv(t, true)
	narrowed := e.createPool("Narrowed", types.RunnerPoolOrganization)
	if w := e.admin(http.MethodPut, poolPath(narrowed.ID)+"/remote-runners/fake", ""); w.Code != http.StatusOK {
		t.Fatalf("first attach (no policy anywhere) = %d %s", w.Code, w.Body)
	}
	e.approve(e.pending(e.call(e.alice, http.MethodPut, usePolicyPath(narrowed.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`)).ID)
	if _, ok := e.listed(e.person)[narrowed.ID]; ok {
		t.Fatal("the policy did not narrow")
	}
	before := e.pendingCount()

	// A new pool is not a widening on its own: it holds no executor until an attach.
	w := e.call(e.super, http.MethodPost, "/api/v1/runner-pools", `{"name":"Wide"}`)
	if w.Code != http.StatusCreated || e.pendingCount() != before {
		t.Fatalf("create = %d %s (pending %d -> %d)", w.Code, w.Body, before, e.pendingCount())
	}
	wide := decodeJSON[types.RunnerPool](t, w)

	// The same executor in an unpolicied pool is held, and nothing is attached until a second human decides.
	held := e.pending(e.call(e.super, http.MethodPut, poolPath(wide.ID)+"/remote-runners/fake", ""))
	if held.TargetKind != govKindRunnerPoolUsePolicy || held.Op != opExecutorAttach || held.TargetKey != wide.ID.String() {
		t.Errorf("held = %+v", held)
	}
	if n := e.members(wide.ID); n != 0 {
		t.Fatalf("a held attach stored %d members", n)
	}
	if got := e.listed(e.person)[wide.ID]; got.Availability != client.RunnerPoolUnavailable {
		t.Errorf("an unattached pool reads %+v", got)
	}
	e.approveExecutor(held.ID)
	if n := e.members(wide.ID); n != 1 {
		t.Fatalf("the approved attach stored %d members", n)
	}
	if rows := e.audits(auditPoolRunnerAdd); len(rows) != 2 || rows[1].Actor != poolSubSuperTwo {
		t.Errorf("attach audit rows = %+v, want the second by the approver", rows)
	}

	// Deleting the narrowed pool would drop its policy with no second human: refused until the clear is held and approved.
	del := e.call(e.super, http.MethodDelete, poolPath(narrowed.ID), "")
	if del.Code != http.StatusConflict || wireReason(t, del) != string(runnerpool.ReasonInvalid) {
		t.Errorf("deleting a pool with a policy = %d %s, want 409 runner_pool_invalid", del.Code, del.Body)
	}
	if n := e.count(`SELECT count(*) FROM runner_pool_use_policies WHERE pool_id = $1`, narrowed.ID); n != 1 {
		t.Errorf("the refused delete dropped the policy (%d rows)", n)
	}
	e.approve(e.pending(e.call(e.carol, http.MethodDelete, usePolicyPath(narrowed.ID), "")).ID)
	if w := e.call(e.super, http.MethodDelete, poolPath(narrowed.ID), ""); w.Code != http.StatusNoContent {
		t.Errorf("deleting after the clear = %d %s", w.Code, w.Body)
	}
}

// The admin token attaches directly and leaves its break-glass row; with no policy anywhere an attach is
// never held.
func TestPG_RunnerPoolExecutorAttachHoldIsOnlyForAnExecutorUnderAPolicy(t *testing.T) {
	e := newPoolEnv(t, true)
	a, b := e.createPool("A", types.RunnerPoolOrganization), e.createPool("B", types.RunnerPoolOrganization)
	if w := e.call(e.super, http.MethodPut, poolPath(a.ID)+"/remote-runners/fake", ""); w.Code != http.StatusOK {
		t.Fatalf("attach with no policy anywhere = %d %s", w.Code, w.Body)
	}
	// Another pool holding the executor without a policy is no reason to hold the attach.
	if w := e.call(e.super, http.MethodPut, poolPath(b.ID)+"/remote-runners/fake", ""); w.Code != http.StatusOK || e.members(b.ID) != 1 {
		t.Fatalf("attach beside a policy-less pool = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.super, http.MethodDelete, poolPath(b.ID)+"/remote-runners/fake", ""); w.Code != http.StatusNoContent {
		t.Fatalf("detach = %d", w.Code)
	}
	e.approve(e.pending(e.call(e.alice, http.MethodPut, usePolicyPath(a.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`)).ID)
	if w := e.admin(http.MethodPut, poolPath(b.ID)+"/remote-runners/fake", ""); w.Code != http.StatusOK || e.members(b.ID) != 1 {
		t.Fatalf("admin token attach = %d %s", w.Code, w.Body)
	}
	if n := len(e.audits("governance.change.bypass")); n != 1 {
		t.Errorf("%d bypass rows, want 1", n)
	}
	// Removing an executor never widens anything, so it is never held.
	if w := e.call(e.super, http.MethodDelete, poolPath(b.ID)+"/remote-runners/fake", ""); w.Code != http.StatusNoContent {
		t.Errorf("detach = %d", w.Code)
	}
}

// A held change binds the pool's executor set: it goes stale when the executors moved after it was proposed.
func TestPG_RunnerPoolHeldChangeGoesStaleWhenTheExecutorsMove(t *testing.T) {
	e := newPoolEnv(t, true)
	pool := e.createPool("Provided", types.RunnerPoolOrganization)
	e.admin(http.MethodPut, poolPath(pool.ID)+"/remote-runners/fake", "")
	held := e.pending(e.call(e.alice, http.MethodPut, usePolicyPath(pool.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`))
	if w := e.call(e.super, http.MethodDelete, poolPath(pool.ID)+"/remote-runners/fake", ""); w.Code != http.StatusNoContent {
		t.Fatalf("detach = %d", w.Code)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(held.ID), ""); w.Code == http.StatusOK {
		t.Fatalf("approving a change gone stale on the executors = %d", w.Code)
	}
	if n := e.count(`SELECT count(*) FROM runner_pool_use_policies`); n != 0 {
		t.Errorf("a stale approval stored a policy")
	}
}

// A leaver purge strips the user from every use policy and never deletes the policy: a list emptied by it
// permits nobody, for a manager's display as for a member's use.
func TestPG_RunnerPoolLeaverPurgeStripsUseSubjectsAndNeverWidens(t *testing.T) {
	e := newPoolEnv(t, false)
	ctx := context.Background()
	shared := e.createPool("Shared", types.RunnerPoolOrganization)
	solo := e.createPool("Solo", types.RunnerPoolOrganization)
	for _, p := range []types.RunnerPool{shared, solo} {
		e.admin(http.MethodPut, poolPath(p.ID)+"/remote-runners/fake", "")
	}
	e.call(e.alice, http.MethodPut, usePolicyPath(shared.ID), `{"subjects":[{"subject_type":"user","subject":"Sub-Leaver"},{"subject_type":"user","subject":"sub-stays"},{"subject_type":"group","subject":"pool-users"}]}`)
	e.call(e.alice, http.MethodPut, usePolicyPath(solo.ID), `{"subjects":[{"subject_type":"user","subject":"sub-leaver"}]}`)
	revShared, revSolo := e.revision(shared.ID), e.revision(solo.ID)

	n, err := e.pg.DeleteUserSubjectRows(ctx, uuid.New(), []string{"sub-leaver"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The purge says what it did to the pools: both narrowed, one of them left permitting nobody.
	if n.PoolsNarrowed != 2 || n.PoolsEmptied != 1 {
		t.Errorf("pools narrowed %d, emptied %d; want 2 and 1", n.PoolsNarrowed, n.PoolsEmptied)
	}
	// And names them, each with its policy as the purge left it, so the audit can say which pool changed.
	byPool := map[uuid.UUID]types.RunnerPoolUsePolicy{}
	for _, p := range n.Pools {
		byPool[p.PoolID] = p
	}
	if len(n.Pools) != 2 || len(byPool[shared.ID].Subjects) != 2 || byPool[shared.ID].Revision != revShared+1 ||
		len(byPool[solo.ID].Subjects) != 0 || byPool[solo.ID].Revision != revSolo+1 {
		t.Errorf("purged pools = %+v, want shared (2 subjects, revision %d) and solo (none, revision %d)", n.Pools, revShared+1, revSolo+1)
	}
	got := e.policySubjects(shared.ID)
	if len(got) != 2 || got[0].Subject != "sub-stays" || got[1].Subject != "pool-users" || e.revision(shared.ID) != revShared+1 {
		t.Errorf("shared policy after the purge = %v (revision %d -> %d)", got, revShared, e.revision(shared.ID))
	}
	if n := e.count(`SELECT count(*) FROM runner_pool_use_policies WHERE pool_id = $1 AND jsonb_array_length(subjects) = 0`, solo.ID); n != 1 || e.revision(solo.ID) != revSolo+1 {
		t.Fatalf("the emptied policy: %d rows (revision %d -> %d), want the row kept empty", n, revSolo, e.revision(solo.ID))
	}
	for name, c := range map[string]*http.Cookie{"member": e.person, "listed user": ssoSessionWith(t, "sub-stays", types.UserTypeStandard, "x"), "group": ssoSessionWith(t, "sub-g", types.UserTypeStandard, "pool-users")} {
		if _, ok := e.listed(c)[solo.ID]; ok {
			t.Errorf("%s may use a pool whose policy was emptied", name)
		}
	}
	if got := e.listed(e.alice)[solo.ID]; got.Availability != client.RunnerPoolUnavailable {
		t.Errorf("a manager is told the emptied pool is %+v", got)
	}
	if got := e.policySubjects(solo.ID); len(got) != 0 {
		t.Errorf("emptied policy reads %v", got)
	}
}

// A manager's list shows what they may use as available and what they may only manage as unavailable;
// the member routes keep their 404.
func TestPG_RunnerPoolManagerCatalogueDoesNotOfferWhatTheyMayNotUse(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createPool("Provided", types.RunnerPoolOrganization)
	e.admin(http.MethodPut, poolPath(pool.ID)+"/remote-runners/fake", "")
	if got := e.listed(e.alice)[pool.ID]; got.Availability != client.RunnerPoolAvailable {
		t.Fatalf("an open pool reads %+v for a manager", got)
	}
	e.call(e.alice, http.MethodPut, usePolicyPath(pool.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`)
	for name, c := range map[string]*http.Cookie{"security admin": e.alice, "admin": e.super} {
		got := e.listed(c)[pool.ID]
		if got.Availability != client.RunnerPoolUnavailable || got.Reason != runnerpool.ReasonNotFound {
			t.Errorf("%s sees the narrowed pool as %+v, want unavailable runner_pool_not_found", name, got)
		}
		if w := e.call(c, http.MethodGet, poolPath(pool.ID), ""); w.Code != http.StatusOK {
			t.Errorf("%s cannot manage-read the pool: %d", name, w.Code)
		}
		if w := e.call(c, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, pool.ID)); w.Code != http.StatusNotFound {
			t.Errorf("%s default for a pool they may not use = %d, want 404", name, w.Code)
		}
	}
}

// A personal default naming a pool the person may no longer use reads as unset.
func TestPG_RunnerPoolPersonalDefaultReadIsFilteredByTheCatalogue(t *testing.T) {
	e := newPoolEnv(t, false)
	narrowed := e.createPool("Narrowed", types.RunnerPoolOrganization)
	gone := e.createPool("Gone", types.RunnerPoolOrganization)
	for _, p := range []types.RunnerPool{narrowed, gone} {
		e.admin(http.MethodPut, poolPath(p.ID)+"/remote-runners/fake", "")
	}
	if w := e.call(e.person, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, narrowed.ID)); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	read := func() types.RunnerPoolDefaults {
		return decodeJSON[types.RunnerPoolDefaults](t, e.call(e.person, http.MethodGet, "/api/v1/me/runner-pool-defaults", ""))
	}
	if read().PoolID == nil {
		t.Fatal("a permitted default reads as unset")
	}
	e.call(e.alice, http.MethodPut, usePolicyPath(narrowed.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`)
	if d := read(); d.PoolID != nil {
		t.Errorf("a default for a pool the person may no longer use reads as %v", d.PoolID)
	}
	if n := e.count(`SELECT count(*) FROM principal_prefs WHERE principal = $1`, e.personP); n != 1 {
		t.Errorf("the stored document was rewritten: %d rows", n)
	}
	e.call(e.person, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, gone.ID))
	e.admin(http.MethodDelete, poolPath(gone.ID), "")
	if d := read(); d.PoolID != nil {
		t.Errorf("a default naming a deleted pool reads as %v", d.PoolID)
	}
}

// The revision in an audit row is the revision the change produced, whatever else moved afterwards, and a
// delete records what it dropped and clears the organisation default that named the pool.
func TestPG_RunnerPoolAuditRowsCarryTheirOwnRevisionAndWhatADeleteDropped(t *testing.T) {
	e := newPoolEnv(t, false)
	self := e.createPool("Laptops", types.RunnerPoolUser)
	a, b := e.claimedRunner(e.personP), e.claimedRunner(e.personP)
	e.call(e.person, http.MethodPut, addRunnerPath(self.ID, a), "")
	e.call(e.person, http.MethodPut, addRunnerPath(self.ID, b), "")
	e.call(e.person, http.MethodDelete, addRunnerPath(self.ID, a), "")
	revOf := func(action string) []float64 {
		var out []float64
		for _, ev := range e.audits(action) {
			out = append(out, auditData(t, ev)["revision"].(float64))
		}
		return out
	}
	if got := revOf(auditPoolRunnerAdd); len(got) != 2 || got[0] != 2 || got[1] != 3 {
		t.Errorf("attach revisions = %v, want [2 3]", got)
	}
	if got := revOf(auditPoolRunnerRemove); len(got) != 1 || got[0] != 4 {
		t.Errorf("detach revisions = %v, want [4]", got)
	}

	remote := e.createPool("Provided", types.RunnerPoolOrganization)
	e.admin(http.MethodPut, poolPath(remote.ID)+"/remote-runners/fake", "")
	e.admin(http.MethodPut, "/api/v1/runner-pool-defaults", fmt.Sprintf(`{"pool_id":%q}`, remote.ID))
	if w := e.call(e.person, http.MethodDelete, "/api/v1/me/runner-pools/"+self.ID.String(), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete self-hosted = %d %s", w.Code, w.Body)
	}
	if w := e.admin(http.MethodDelete, poolPath(remote.ID), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete remote = %d %s", w.Code, w.Body)
	}
	del := e.audits(auditPoolDelete)
	if len(del) != 2 {
		t.Fatalf("%d delete rows", len(del))
	}
	if d := auditData(t, del[0]); d["runner_count"] != float64(1) || !jsonListIs(d["remote_runner_ids"]) || d["owner"] != e.personP || d["scope"] != "user" {
		t.Errorf("user pool delete row = %v, want runner_count 1, no remote-hosted runners and the owner", d)
	}
	if d := auditData(t, del[1]); d["runner_count"] != float64(0) || !jsonListIs(d["remote_runner_ids"], "fake") || d["owner"] != nil || d["scope"] != "organization" {
		t.Errorf("organization pool delete row = %v, want remote_runner_ids [fake] and no owner", d)
	}
	if d := auditData(t, e.audits(auditPoolCreate)[0]); d["owner"] != e.personP {
		t.Errorf("self-hosted create row = %v, want the owner", d)
	}
	var rp *uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT pool_id FROM runner_pool_org_defaults`).Scan(&rp); err != nil {
		t.Fatal(err)
	}
	if rp != nil {
		t.Errorf("organisation defaults still name a deleted pool: %v", rp)
	}
	rows := e.audits(auditPoolDefaultSet)
	if len(rows) != 2 { // the admin write, then the clear of the deleted remote pool
		t.Fatalf("%d default audit rows, want 2", len(rows))
	}
	if d := auditData(t, rows[1]); d["cause"] != "pool_deleted" || d["after"] == nil {
		t.Errorf("clear audit data = %v", d)
	}
}

// The use policy's revision is the pool's: a read-modify-write after an unrelated change to the pool is
// not refused, a stale one is, and the check is made under the pool lock.
func TestPG_RunnerPoolUsePolicyRevisionIsThePoolsRevision(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createPool("Provided", types.RunnerPoolOrganization)
	e.call(e.alice, http.MethodPut, usePolicyPath(pool.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`)
	e.admin(http.MethodPut, poolPath(pool.ID)+"/remote-runners/fake", "") // moves the pool's revision, not the policy's
	read := decodeJSON[types.RunnerPoolUsePolicy](t, e.call(e.alice, http.MethodGet, usePolicyPath(pool.ID), ""))
	if read.Revision != e.revision(pool.ID) {
		t.Fatalf("the policy read carries revision %d, the pool is at %d", read.Revision, e.revision(pool.ID))
	}
	body := func(rev int64) string {
		return fmt.Sprintf(`{"revision":%d,"subjects":[{"subject_type":"user","subject":"sub-other"}]}`, rev)
	}
	if w := e.call(e.alice, http.MethodPut, usePolicyPath(pool.ID), body(read.Revision-1)); w.Code != http.StatusConflict || wireReason(t, w) != string(runnerpool.ReasonStale) {
		t.Errorf("a stale revision = %d %s, want 409 runner_pool_stale", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPut, usePolicyPath(pool.ID), body(read.Revision)); w.Code != http.StatusOK {
		t.Errorf("the revision the read returned = %d %s, want 200", w.Code, w.Body)
	}
}

// DONE WHEN (1) and (4): an other-organisation runner and an unclaimed one are never members.
func TestPG_RunnerPoolsOtherOrgAndUnclaimedRunnersAreNeverMembers(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createPool("Laptops", types.RunnerPoolUser)
	for name, id := range map[string]uuid.UUID{
		"other org": e.rawRunner(e.personP, "https://elsewhere.example.com", true),
		"unclaimed": e.rawRunner(e.personP, poolTestOrgURL, false),
	} {
		if w := e.call(e.person, http.MethodPut, addRunnerPath(pool.ID, id), ""); w.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s runner add = %d %s", name, w.Code, w.Body)
		}
	}
	if n := e.members(pool.ID); n != 0 {
		t.Errorf("%d members", n)
	}
}

// Boot on several replicas bootstraps once: one pool, one audit row.
func TestPG_RunnerPoolsConcurrentBootstrapMakesOnePool(t *testing.T) {
	e := newPoolEnv(t, false)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = e.srv.BootstrapRunnerPools(context.Background()) }()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Errorf("bootstrap: %v", err)
		}
	}
	if n := e.count(`SELECT count(*) FROM runner_pools`); n != 1 {
		t.Errorf("%d pools after concurrent bootstrap", n)
	}
	if n := len(e.audits(auditPoolCreate)); n != 1 {
		t.Errorf("%d create audit rows", n)
	}
}

// Detaching a narrowed executor is held like attaching it: otherwise one administrator detaches it, so
// that no policy pool holds it any more, and attaches it to an open pool.
func TestPG_RunnerPoolDetachFromAPolicyPoolIsHeld(t *testing.T) {
	e := newPoolEnv(t, true)
	a, b := e.createPool("A", types.RunnerPoolOrganization), e.createPool("B", types.RunnerPoolOrganization)
	if w := e.call(e.super, http.MethodPut, poolPath(a.ID)+"/remote-runners/fake", ""); w.Code != http.StatusOK {
		t.Fatalf("attach = %d", w.Code)
	}
	e.approve(e.pending(e.call(e.alice, http.MethodPut, usePolicyPath(a.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`)).ID)

	held := e.pending(e.call(e.super, http.MethodDelete, poolPath(a.ID)+"/remote-runners/fake", ""))
	if held.TargetKind != govKindRunnerPoolUsePolicy || held.TargetKey != a.ID.String() || held.Op != opExecutorAttach {
		t.Errorf("held = %+v", held)
	}
	if n := e.members(a.ID); n != 1 {
		t.Fatalf("a held detach removed the member (%d left)", n)
	}
	// With the detach pending, the executor is still narrowed: attaching it elsewhere is held too.
	e.pending(e.call(e.super, http.MethodPut, poolPath(b.ID)+"/remote-runners/fake", ""))
	if n := e.members(b.ID); n != 0 {
		t.Fatalf("an attach to an open pool stored %d members while the executor is narrowed", n)
	}
	if got := e.listed(e.person)[b.ID]; got.Availability == client.RunnerPoolAvailable {
		t.Errorf("an unnamed person is offered the narrowed executor through %+v", got)
	}
	e.approveExecutor(held.ID)
	if n := e.members(a.ID); n != 0 {
		t.Fatalf("the approved detach left %d members", n)
	}
	if rows := e.audits(auditPoolRunnerRemove); len(rows) != 1 || rows[0].Actor != poolSubSuperTwo {
		t.Errorf("detach audit rows = %+v, want one by the approver", rows)
	}
	// The admin token detaches directly and leaves its break-glass row.
	e.call(e.super, http.MethodPut, poolPath(a.ID)+"/remote-runners/fake", "")
	before := len(e.audits("governance.change.bypass"))
	if w := e.admin(http.MethodDelete, poolPath(a.ID)+"/remote-runners/fake", ""); w.Code != http.StatusNoContent || e.members(a.ID) != 0 {
		t.Fatalf("admin token detach = %d", w.Code)
	}
	if n := len(e.audits("governance.change.bypass")); n != before+1 {
		t.Errorf("%d bypass rows, want %d", n, before+1)
	}
}

// A held detach whose executor was taken out meanwhile is refused as stale, never applied as a
// no-op, so no detach row is written for a removal that did not happen.
func TestPG_RunnerPoolHeldDetachOfAnAbsentExecutorIsStale(t *testing.T) {
	e := newPoolEnv(t, true)
	a := e.createPool("A", types.RunnerPoolOrganization)
	if w := e.call(e.super, http.MethodPut, poolPath(a.ID)+"/remote-runners/fake", ""); w.Code != http.StatusOK {
		t.Fatalf("attach = %d", w.Code)
	}
	e.approve(e.pending(e.call(e.alice, http.MethodPut, usePolicyPath(a.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`)).ID)
	held := e.pending(e.call(e.super, http.MethodDelete, poolPath(a.ID)+"/remote-runners/fake", ""))
	if _, err := e.pool.Exec(t.Context(), `DELETE FROM runner_pool_members WHERE pool_id = $1`, a.ID); err != nil {
		t.Fatal(err)
	}
	before := len(e.audits(auditPoolRunnerRemove))
	w := e.call(e.superTwo, http.MethodPost, "/api/v1/governance/changes/"+held.ID.String()+"/approve", `{"reason":"ok"}`)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "governance_change_stale") {
		t.Fatalf("approving a detach of an absent executor = %d %s, want 409 governance_change_stale", w.Code, w.Body)
	}
	if n := len(e.audits(auditPoolRunnerRemove)); n != before {
		t.Errorf("a stale detach wrote %d detach rows", n-before)
	}
}

// within reports whether done closes inside d.
func within(done <-chan *httptest.ResponseRecorder, d time.Duration) (*httptest.ResponseRecorder, bool) {
	select {
	case w := <-done:
		return w, true
	case <-time.After(d):
		return nil, false
	}
}

// A decision is made under the pool row lock, taken before the executors are read: a member write that
// commits while the approval waits is seen by its stale check, so the approval cannot apply over it.
func TestPG_RunnerPoolConcurrentApprovalReadsTheExecutorsUnderThePoolLock(t *testing.T) {
	e := newPoolEnv(t, true)
	pool := e.createPool("Provided", types.RunnerPoolOrganization)
	e.admin(http.MethodPut, poolPath(pool.ID)+"/remote-runners/fake", "")
	held := e.pending(e.call(e.alice, http.MethodPut, usePolicyPath(pool.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`))
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM runner_pools WHERE id = $1 FOR UPDATE`, pool.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.call(e.bob, http.MethodPost, approvePath(held.ID), "") }()
	time.Sleep(700 * time.Millisecond)
	if _, err := tx.Exec(ctx, `DELETE FROM runner_pool_members WHERE pool_id = $1`, pool.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	w, ok := within(done, 20*time.Second)
	if !ok {
		t.Fatal("the approval never finished")
	}
	if w.Code == http.StatusOK {
		t.Errorf("approval applied over an executor set that moved: %d %s", w.Code, w.Body)
	}
	if n := e.count(`SELECT count(*) FROM runner_pool_use_policies`); n != 0 {
		t.Errorf("a stale approval stored a policy")
	}
}

// Direct executor writes queue behind a decision on the same target.
func TestPG_RunnerPoolConcurrentDirectExecutorWritesTakeTheGovernanceTargetLock(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createPool("Provided", types.RunnerPoolOrganization)
	ctx := context.Background()
	for name, do := range map[string]func() *httptest.ResponseRecorder{
		"attach": func() *httptest.ResponseRecorder {
			return e.admin(http.MethodPut, poolPath(pool.ID)+"/remote-runners/fake", "")
		},
		"detach": func() *httptest.ResponseRecorder {
			return e.admin(http.MethodDelete, poolPath(pool.ID)+"/remote-runners/fake", "")
		},
	} {
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.LockGovernanceTarget(ctx, tx, govKindRunnerPoolUsePolicy, pool.ID.String()); err != nil {
			t.Fatal(err)
		}
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { done <- do() }()
		if _, ok := within(done, 700*time.Millisecond); ok {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s did not wait for the decision holding the target lock", name)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if w, ok := within(done, 20*time.Second); !ok || w.Code >= 300 {
			t.Errorf("%s after the lock was released = %v", name, w)
		}
	}
}

const poolLimitsJSON = `{"background":{"cpu_millis":{"default":500,"cap":2000},"memory_mib":{"default":512,"cap":4096},"lifetime":{"default_sec":3600,"max_sec":86400}},` +
	`"interactive":{"cpu_millis":{"default":1000,"cap":4000},"memory_mib":{"default":1024,"cap":8192},"lifetime":{"default_sec":1800,"max_sec":28800},"idle":{"default_sec":600,"max_sec":3600}},` +
	`"barriers":["CC1","CC2"],"max_concurrent_runs":3}`

// poolLimitsWithoutCap is the same block with the concurrency cap removed.
const poolLimitsWithoutCap = `{"background":{"cpu_millis":{"default":500,"cap":2000},"memory_mib":{"default":512,"cap":4096},"lifetime":{"default_sec":3600,"max_sec":86400}},"barriers":["CC1"]}`

func (e *poolEnv) storedLimits(pool uuid.UUID) *types.RunnerPoolLimits {
	e.t.Helper()
	p, err := e.pg.GetRunnerPool(context.Background(), pool)
	if err != nil {
		e.t.Fatal(err)
	}
	return p.Limits
}

func (e *poolEnv) limitedPool(name string) types.RunnerPool {
	e.t.Helper()
	w := e.admin(http.MethodPost, "/api/v1/runner-pools", fmt.Sprintf(`{"name":%q,"limits":%s}`, name, poolLimitsJSON))
	if w.Code != http.StatusCreated {
		e.t.Fatalf("create limited pool = %d %s", w.Code, w.Body)
	}
	return decodeJSON[types.RunnerPool](e.t, w)
}

// Limits are stored on create and replaced whole on update; an update without them leaves the block,
// and a block a pool cannot keep is a 400 that moves nothing.
func TestPG_RunnerPoolCreateAndUpdateStoreLimits(t *testing.T) {
	e := newPoolEnv(t, false)
	created := e.limitedPool("Limited")
	if created.Limits == nil || created.Limits.MaxConcurrentRuns == nil || *created.Limits.MaxConcurrentRuns != 3 || created.Revision != 1 {
		t.Fatalf("created pool = %+v", created)
	}
	if got := e.storedLimits(created.ID); !reflect.DeepEqual(got, created.Limits) {
		t.Errorf("stored limits = %+v, want %+v", got, created.Limits)
	}
	if w := e.admin(http.MethodGet, poolPath(created.ID), ""); w.Code != http.StatusOK || !reflect.DeepEqual(decodeJSON[types.RunnerPool](t, w).Limits, created.Limits) {
		t.Errorf("GET pool = %d %s, want the limits back", w.Code, w.Body)
	}

	w := e.admin(http.MethodPut, poolPath(created.ID), fmt.Sprintf(`{"revision":1,"limits":%s}`, poolLimitsWithoutCap))
	got := decodeJSON[types.RunnerPool](t, w)
	if w.Code != http.StatusOK || got.Revision != 2 || got.Limits == nil || got.Limits.MaxConcurrentRuns != nil || got.Limits.Interactive != nil {
		t.Fatalf("replace = %d %s, want revision 2 and the block replaced whole", w.Code, w.Body)
	}
	replaced := e.storedLimits(created.ID)
	if w := e.admin(http.MethodPut, poolPath(created.ID), `{"revision":2,"name":"Renamed"}`); w.Code != http.StatusOK || !reflect.DeepEqual(e.storedLimits(created.ID), replaced) {
		t.Errorf("an update without limits = %d %s, and left %+v", w.Code, w.Body, e.storedLimits(created.ID))
	}

	rev := e.revision(created.ID)
	for name, bad := range map[string]string{
		"no run type":        `{"barriers":["CC1"]}`,
		"unknown barrier":    `{"background":{"cpu_millis":{"default":1,"cap":2},"memory_mib":{"default":1,"cap":2},"lifetime":{"default_sec":1,"max_sec":2}},"barriers":["CC9"]}`,
		"default above cap":  `{"background":{"cpu_millis":{"default":3,"cap":2},"memory_mib":{"default":1,"cap":2},"lifetime":{"default_sec":1,"max_sec":2}},"barriers":["CC1"]}`,
		"interactive no end": `{"interactive":{"cpu_millis":{"default":1,"cap":2},"memory_mib":{"default":1,"cap":2},"lifetime":{"default_sec":0,"unlimited":true},"idle":{"default_sec":1,"max_sec":2}},"barriers":["CC1"]}`,
	} {
		upd := e.admin(http.MethodPut, poolPath(created.ID), fmt.Sprintf(`{"revision":%d,"limits":%s}`, rev, bad))
		if upd.Code != http.StatusBadRequest || wireReason(t, upd) != string(runnerpool.ReasonInvalid) {
			t.Errorf("update with %s = %d %s, want 400 runner_pool_invalid", name, upd.Code, upd.Body)
		}
		cre := e.admin(http.MethodPost, "/api/v1/runner-pools", fmt.Sprintf(`{"name":"Bad","limits":%s}`, bad))
		if cre.Code != http.StatusBadRequest || wireReason(t, cre) != string(runnerpool.ReasonInvalid) {
			t.Errorf("create with %s = %d %s, want 400 runner_pool_invalid", name, cre.Code, cre.Body)
		}
	}
	if e.revision(created.ID) != rev || !reflect.DeepEqual(e.storedLimits(created.ID), replaced) {
		t.Errorf("a refused update moved the pool")
	}
	if n := e.count(`SELECT count(*) FROM runner_pools`); n != 1 {
		t.Errorf("a refused create stored a pool: %d pools", n)
	}
}

// Re-sending the stored block alone is a no-op: no revision, no audit row.
func TestPG_RunnerPoolIdenticalLimitsAreANoOp(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.limitedPool("Limited")
	if w := e.admin(http.MethodPut, poolPath(pool.ID), fmt.Sprintf(`{"revision":1,"limits":%s}`, poolLimitsJSON)); w.Code != http.StatusOK || decodeJSON[types.RunnerPool](t, w).Revision != 1 {
		t.Errorf("identical limits = %d %s, want 200 at revision 1", w.Code, w.Body)
	}
	if e.revision(pool.ID) != 1 || len(e.audits(auditPoolUpdate)) != 0 {
		t.Errorf("an identical block moved the pool: revision %d, %d update rows", e.revision(pool.ID), len(e.audits(auditPoolUpdate)))
	}
}

// A limits change is audited with the block before and after; a rename carries neither, and a create
// carries the stored block.
func TestPG_RunnerPoolLimitsChangeIsAuditedBeforeAndAfter(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.limitedPool("Limited")
	if d := auditData(t, e.audits(auditPoolCreate)[0]); d["limits"] == nil {
		t.Errorf("create audit data = %v, want the stored limits", d)
	}
	if w := e.admin(http.MethodPut, poolPath(pool.ID), fmt.Sprintf(`{"revision":1,"limits":%s}`, poolLimitsWithoutCap)); w.Code != http.StatusOK {
		t.Fatalf("limits update = %d %s", w.Code, w.Body)
	}
	if w := e.admin(http.MethodPut, poolPath(pool.ID), `{"revision":2,"name":"Renamed"}`); w.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", w.Code, w.Body)
	}
	rows := e.audits(auditPoolUpdate)
	if len(rows) != 2 {
		t.Fatalf("%d update rows, want 2", len(rows))
	}
	var withLimits, rename map[string]any
	for _, r := range rows {
		if d := auditData(t, r); d["name"] == "Renamed" {
			rename = d
		} else {
			withLimits = d
		}
	}
	before, _ := withLimits["limits_before"].(map[string]any)
	after, _ := withLimits["limits"].(map[string]any)
	if before["max_concurrent_runs"] != float64(3) || after == nil || after["max_concurrent_runs"] != nil {
		t.Errorf("limits update audit = %v, want limits_before with the cap and limits without", withLimits)
	}
	if _, ok := rename["limits"]; ok {
		t.Errorf("a rename carries limits: %v", rename)
	}
	if _, ok := rename["limits_before"]; ok {
		t.Errorf("a rename carries limits_before: %v", rename)
	}
}

// A limits update at an old revision is runner_pool_stale and the block stays.
func TestPG_RunnerPoolStaleRevisionRefusesALimitsChange(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.limitedPool("Limited")
	if w := e.admin(http.MethodPut, poolPath(pool.ID), `{"revision":1,"name":"Renamed"}`); w.Code != http.StatusOK {
		t.Fatalf("rename = %d %s", w.Code, w.Body)
	}
	before := e.storedLimits(pool.ID)
	w := e.admin(http.MethodPut, poolPath(pool.ID), fmt.Sprintf(`{"revision":1,"limits":%s}`, poolLimitsWithoutCap))
	if w.Code != http.StatusConflict || wireReason(t, w) != string(runnerpool.ReasonStale) {
		t.Errorf("stale limits update = %d %s, want 409 runner_pool_stale", w.Code, w.Body)
	}
	if !reflect.DeepEqual(e.storedLimits(pool.ID), before) || e.revision(pool.ID) != 2 {
		t.Errorf("a stale limits update changed the pool")
	}
}

// A detach that read "no policy" and then waits on the target lock while the pool's first use policy is
// approved must not remove the narrowed executor unheld: the store re-checks under the locks, and the
// handler decides again on the catalogue as it now stands, which holds the detach.
func TestPG_RunnerPoolConcurrentDetachRacingFirstPolicyApprovalIsHeld(t *testing.T) {
	e := newPoolEnv(t, true)
	a := e.createPool("A", types.RunnerPoolOrganization)
	if w := e.call(e.super, http.MethodPut, poolPath(a.ID)+"/remote-runners/fake", ""); w.Code != http.StatusOK {
		t.Fatalf("attach = %d %s", w.Code, w.Body)
	}
	held := e.pending(e.call(e.alice, http.MethodPut, usePolicyPath(a.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`))
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Stall the approval after it takes the target lock: it waits on the pool row.
	if _, err := tx.Exec(ctx, `SELECT 1 FROM runner_pools WHERE id = $1 FOR UPDATE`, a.ID); err != nil {
		t.Fatal(err)
	}
	appr := make(chan *httptest.ResponseRecorder, 1)
	go func() { appr <- e.call(e.bob, http.MethodPost, approvePath(held.ID), "") }()
	time.Sleep(700 * time.Millisecond)
	// The detach reads the catalogue (no policy yet), then waits on the target lock.
	det := make(chan *httptest.ResponseRecorder, 1)
	go func() { det <- e.call(e.super, http.MethodDelete, poolPath(a.ID)+"/remote-runners/fake", "") }()
	time.Sleep(700 * time.Millisecond)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wa, ok1 := within(appr, 20*time.Second)
	wd, ok2 := within(det, 20*time.Second)
	if !ok1 || !ok2 {
		t.Fatal("the approval or the detach did not finish")
	}
	if wa.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", wa.Code, wa.Body)
	}
	if wd.Code != http.StatusAccepted {
		t.Errorf("detach racing the first policy approval = %d %s, want 202 held", wd.Code, wd.Body)
	}
	if n := e.members(a.ID); n != 1 {
		t.Errorf("members(A) = %d after the raced detach, want 1: the executor must stay until a second human approves", n)
	}
}

const poolSubSuperTwo = "sub-super-two"

// approveExecutor approves a held executor attach or detach as a second super admin, after checking that a
// security admin can neither see nor decide it: the executor routes are written on the super-admin tier.
func (e *poolEnv) approveExecutor(id uuid.UUID) {
	e.t.Helper()
	for _, verb := range []string{"approve", "reject"} {
		if w := e.call(e.bob, http.MethodPost, "/api/v1/governance/changes/"+id.String()+"/"+verb, `{"reason":"no"}`); w.Code != http.StatusForbidden {
			e.t.Fatalf("security admin %s of a held executor change = %d %s, want 403", verb, w.Code, w.Body)
		}
	}
	if w := e.call(e.bob, http.MethodGet, "/api/v1/governance/changes/"+id.String(), ""); w.Code != http.StatusNotFound {
		e.t.Fatalf("security admin read of a held executor change = %d, want 404", w.Code)
	}
	for _, ch := range decodeJSON[[]types.GovernanceChange](e.t, e.call(e.bob, http.MethodGet, "/api/v1/governance/changes?state=pending", "")) {
		if ch.ID == id {
			e.t.Fatal("a held executor change is listed for a security admin")
		}
	}
	if w := e.call(e.superTwo, http.MethodPost, "/api/v1/governance/changes/"+id.String()+"/approve", ""); w.Code != http.StatusOK {
		e.t.Fatalf("second super admin approving %s = %d %s, want 200", id, w.Code, w.Body)
	}
}
