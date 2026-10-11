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
		OrgURLSHA256: federation.OrgURLSHA256(orgURL)}); err != nil {
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
	narrowed := e.createPool("Narrowed", types.RunnerPoolRemoteProvided)
	if w := e.admin(http.MethodPut, poolPath(narrowed.ID)+"/executors/fake", ""); w.Code != http.StatusOK {
		t.Fatalf("first attach (no policy anywhere) = %d %s", w.Code, w.Body)
	}
	e.approve(e.pending(e.call(e.alice, http.MethodPut, usePolicyPath(narrowed.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`)).ID)
	if _, ok := e.listed(e.person)[narrowed.ID]; ok {
		t.Fatal("the policy did not narrow")
	}
	before := e.pendingCount()

	// A new pool is not a widening on its own: it holds no executor until an attach.
	w := e.call(e.super, http.MethodPost, "/api/v1/runner-pools", `{"name":"Wide","hosting_type":"remote_provided"}`)
	if w.Code != http.StatusCreated || e.pendingCount() != before {
		t.Fatalf("create = %d %s (pending %d -> %d)", w.Code, w.Body, before, e.pendingCount())
	}
	wide := decodeJSON[types.RunnerPool](t, w)

	// The same executor in an unpolicied pool is held, and nothing is attached until a second human decides.
	held := e.pending(e.call(e.super, http.MethodPut, poolPath(wide.ID)+"/executors/fake", ""))
	if held.TargetKind != govKindRunnerPoolUsePolicy || held.Op != opExecutorAttach || held.TargetKey != wide.ID.String() {
		t.Errorf("held = %+v", held)
	}
	if n := e.members(wide.ID); n != 0 {
		t.Fatalf("a held attach stored %d members", n)
	}
	if got := e.listed(e.person)[wide.ID]; got.Availability != client.RunnerPoolUnavailable {
		t.Errorf("an unattached pool reads %+v", got)
	}
	e.approve(held.ID)
	if n := e.members(wide.ID); n != 1 {
		t.Fatalf("the approved attach stored %d members", n)
	}
	if rows := e.audits(auditPoolExecutorAdd); len(rows) != 2 || rows[1].Actor != govSubBob {
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
	a, b := e.createPool("A", types.RunnerPoolRemoteProvided), e.createPool("B", types.RunnerPoolRemoteProvided)
	if w := e.call(e.super, http.MethodPut, poolPath(a.ID)+"/executors/fake", ""); w.Code != http.StatusOK {
		t.Fatalf("attach with no policy anywhere = %d %s", w.Code, w.Body)
	}
	// Another pool holding the executor without a policy is no reason to hold the attach.
	if w := e.call(e.super, http.MethodPut, poolPath(b.ID)+"/executors/fake", ""); w.Code != http.StatusOK || e.members(b.ID) != 1 {
		t.Fatalf("attach beside a policy-less pool = %d %s", w.Code, w.Body)
	}
	if w := e.call(e.super, http.MethodDelete, poolPath(b.ID)+"/executors/fake", ""); w.Code != http.StatusNoContent {
		t.Fatalf("detach = %d", w.Code)
	}
	e.approve(e.pending(e.call(e.alice, http.MethodPut, usePolicyPath(a.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`)).ID)
	if w := e.admin(http.MethodPut, poolPath(b.ID)+"/executors/fake", ""); w.Code != http.StatusOK || e.members(b.ID) != 1 {
		t.Fatalf("admin token attach = %d %s", w.Code, w.Body)
	}
	if n := len(e.audits("governance.change.bypass")); n != 1 {
		t.Errorf("%d bypass rows, want 1", n)
	}
	// Removing an executor never widens anything, so it is never held.
	if w := e.call(e.super, http.MethodDelete, poolPath(b.ID)+"/executors/fake", ""); w.Code != http.StatusNoContent {
		t.Errorf("detach = %d", w.Code)
	}
}

// A held change binds the pool's executor set: it goes stale when the executors moved after it was proposed.
func TestPG_RunnerPoolHeldChangeGoesStaleWhenTheExecutorsMove(t *testing.T) {
	e := newPoolEnv(t, true)
	pool := e.createPool("Provided", types.RunnerPoolRemoteProvided)
	e.admin(http.MethodPut, poolPath(pool.ID)+"/executors/fake", "")
	held := e.pending(e.call(e.alice, http.MethodPut, usePolicyPath(pool.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`))
	if w := e.call(e.super, http.MethodDelete, poolPath(pool.ID)+"/executors/fake", ""); w.Code != http.StatusNoContent {
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
	shared := e.createPool("Shared", types.RunnerPoolRemoteProvided)
	solo := e.createPool("Solo", types.RunnerPoolRemoteProvided)
	for _, p := range []types.RunnerPool{shared, solo} {
		e.admin(http.MethodPut, poolPath(p.ID)+"/executors/fake", "")
	}
	e.call(e.alice, http.MethodPut, usePolicyPath(shared.ID), `{"subjects":[{"subject_type":"user","subject":"Sub-Leaver"},{"subject_type":"user","subject":"sub-stays"},{"subject_type":"group","subject":"pool-users"}]}`)
	e.call(e.alice, http.MethodPut, usePolicyPath(solo.ID), `{"subjects":[{"subject_type":"user","subject":"sub-leaver"}]}`)
	revShared, revSolo := e.revision(shared.ID), e.revision(solo.ID)

	if _, _, _, err := e.pg.DeleteUserSubjectRows(ctx, uuid.New(), []string{"sub-leaver"}, nil); err != nil {
		t.Fatal(err)
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
	pool := e.createPool("Provided", types.RunnerPoolRemoteProvided)
	e.admin(http.MethodPut, poolPath(pool.ID)+"/executors/fake", "")
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
		if w := e.call(c, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"remote_provided":%q}`, pool.ID)); w.Code != http.StatusNotFound {
			t.Errorf("%s default for a pool they may not use = %d, want 404", name, w.Code)
		}
	}
}

// A personal default naming a pool the person may no longer use reads as unset.
func TestPG_RunnerPoolPersonalDefaultReadIsFilteredByTheCatalogue(t *testing.T) {
	e := newPoolEnv(t, false)
	narrowed := e.createPool("Narrowed", types.RunnerPoolRemoteProvided)
	gone := e.createPool("Gone", types.RunnerPoolRemoteProvided)
	for _, p := range []types.RunnerPool{narrowed, gone} {
		e.admin(http.MethodPut, poolPath(p.ID)+"/executors/fake", "")
	}
	if w := e.call(e.person, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"remote_provided":%q}`, narrowed.ID)); w.Code != http.StatusOK {
		t.Fatal(w.Body)
	}
	read := func() types.RunnerPoolDefaults {
		return decodeJSON[types.RunnerPoolDefaults](t, e.call(e.person, http.MethodGet, "/api/v1/me/runner-pool-defaults", ""))
	}
	if read().RemoteProvided == nil {
		t.Fatal("a permitted default reads as unset")
	}
	e.call(e.alice, http.MethodPut, usePolicyPath(narrowed.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`)
	if d := read(); d.RemoteProvided != nil {
		t.Errorf("a default for a pool the person may no longer use reads as %v", d.RemoteProvided)
	}
	if n := e.count(`SELECT count(*) FROM principal_prefs WHERE principal = $1`, e.personP); n != 1 {
		t.Errorf("the stored document was rewritten: %d rows", n)
	}
	e.call(e.person, http.MethodPut, "/api/v1/me/runner-pool-defaults", fmt.Sprintf(`{"remote_provided":%q}`, gone.ID))
	e.admin(http.MethodDelete, poolPath(gone.ID), "")
	if d := read(); d.RemoteProvided != nil {
		t.Errorf("a default naming a deleted pool reads as %v", d.RemoteProvided)
	}
}

// The revision in an audit row is the revision the change produced, whatever else moved afterwards, and a
// delete records what it dropped and clears the organisation default that named the pool.
func TestPG_RunnerPoolAuditRowsCarryTheirOwnRevisionAndWhatADeleteDropped(t *testing.T) {
	e := newPoolEnv(t, false)
	self := e.createPool("Laptops", types.RunnerPoolSelfHosted)
	a, b := e.claimedRunner(e.personP), e.claimedRunner(e.otherP)
	e.call(e.person, http.MethodPut, addRunnerPath(self.ID, a), "")
	e.call(e.other, http.MethodPut, addRunnerPath(self.ID, b), "")
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

	remote := e.createPool("Provided", types.RunnerPoolRemoteProvided)
	e.admin(http.MethodPut, poolPath(remote.ID)+"/executors/fake", "")
	e.admin(http.MethodPut, "/api/v1/runner-pool-defaults", fmt.Sprintf(`{"remote_provided":%q,"self_hosted":%q}`, remote.ID, self.ID))
	if w := e.admin(http.MethodDelete, poolPath(self.ID), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete self-hosted = %d %s", w.Code, w.Body)
	}
	if w := e.admin(http.MethodDelete, poolPath(remote.ID), ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete remote = %d %s", w.Code, w.Body)
	}
	del := e.audits(auditPoolDelete)
	if len(del) != 2 {
		t.Fatalf("%d delete rows", len(del))
	}
	if d := auditData(t, del[0]); d["runner_count"] != float64(1) || !jsonListIs(d["executor_ids"]) {
		t.Errorf("self-hosted delete row = %v, want runner_count 1 and no executors", d)
	}
	if d := auditData(t, del[1]); d["runner_count"] != float64(0) || !jsonListIs(d["executor_ids"], "fake") {
		t.Errorf("remote delete row = %v, want executor_ids [fake]", d)
	}
	var rp, sh *uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT remote_provided, self_hosted FROM runner_pool_org_defaults`).Scan(&rp, &sh); err != nil {
		t.Fatal(err)
	}
	if rp != nil || sh != nil {
		t.Errorf("organisation defaults still name deleted pools: %v %v", rp, sh)
	}
	rows := e.audits(auditPoolDefaultSet)
	if len(rows) != 3 { // the admin write, then one clear per deleted pool
		t.Fatalf("%d default audit rows, want 3", len(rows))
	}
	if d := auditData(t, rows[2]); d["cause"] != "pool_deleted" || d["after"] == nil {
		t.Errorf("clear audit data = %v", d)
	}
}

// The use policy's revision is the pool's: a read-modify-write after an unrelated change to the pool is
// not refused, a stale one is, and the check is made under the pool lock.
func TestPG_RunnerPoolUsePolicyRevisionIsThePoolsRevision(t *testing.T) {
	e := newPoolEnv(t, false)
	pool := e.createPool("Provided", types.RunnerPoolRemoteProvided)
	e.call(e.alice, http.MethodPut, usePolicyPath(pool.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`)
	e.admin(http.MethodPut, poolPath(pool.ID)+"/executors/fake", "") // moves the pool's revision, not the policy's
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
	pool := e.createPool("Laptops", types.RunnerPoolSelfHosted)
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
	a, b := e.createPool("A", types.RunnerPoolRemoteProvided), e.createPool("B", types.RunnerPoolRemoteProvided)
	if w := e.call(e.super, http.MethodPut, poolPath(a.ID)+"/executors/fake", ""); w.Code != http.StatusOK {
		t.Fatalf("attach = %d", w.Code)
	}
	e.approve(e.pending(e.call(e.alice, http.MethodPut, usePolicyPath(a.ID), `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`)).ID)

	held := e.pending(e.call(e.super, http.MethodDelete, poolPath(a.ID)+"/executors/fake", ""))
	if held.TargetKind != govKindRunnerPoolUsePolicy || held.TargetKey != a.ID.String() || held.Op != opExecutorAttach {
		t.Errorf("held = %+v", held)
	}
	if n := e.members(a.ID); n != 1 {
		t.Fatalf("a held detach removed the member (%d left)", n)
	}
	// With the detach pending, the executor is still narrowed: attaching it elsewhere is held too.
	e.pending(e.call(e.super, http.MethodPut, poolPath(b.ID)+"/executors/fake", ""))
	if n := e.members(b.ID); n != 0 {
		t.Fatalf("an attach to an open pool stored %d members while the executor is narrowed", n)
	}
	if got := e.listed(e.person)[b.ID]; got.Availability == client.RunnerPoolAvailable {
		t.Errorf("an unnamed person is offered the narrowed executor through %+v", got)
	}
	e.approve(held.ID)
	if n := e.members(a.ID); n != 0 {
		t.Fatalf("the approved detach left %d members", n)
	}
	if rows := e.audits(auditPoolExecutorRemove); len(rows) != 1 || rows[0].Actor != govSubBob {
		t.Errorf("detach audit rows = %+v, want one by the approver", rows)
	}
	// The admin token detaches directly and leaves its break-glass row.
	e.call(e.super, http.MethodPut, poolPath(a.ID)+"/executors/fake", "")
	before := len(e.audits("governance.change.bypass"))
	if w := e.admin(http.MethodDelete, poolPath(a.ID)+"/executors/fake", ""); w.Code != http.StatusNoContent || e.members(a.ID) != 0 {
		t.Fatalf("admin token detach = %d", w.Code)
	}
	if n := len(e.audits("governance.change.bypass")); n != before+1 {
		t.Errorf("%d bypass rows, want %d", n, before+1)
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
	pool := e.createPool("Provided", types.RunnerPoolRemoteProvided)
	e.admin(http.MethodPut, poolPath(pool.ID)+"/executors/fake", "")
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
	pool := e.createPool("Provided", types.RunnerPoolRemoteProvided)
	ctx := context.Background()
	for name, do := range map[string]func() *httptest.ResponseRecorder{
		"attach": func() *httptest.ResponseRecorder {
			return e.admin(http.MethodPut, poolPath(pool.ID)+"/executors/fake", "")
		},
		"detach": func() *httptest.ResponseRecorder {
			return e.admin(http.MethodDelete, poolPath(pool.ID)+"/executors/fake", "")
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
