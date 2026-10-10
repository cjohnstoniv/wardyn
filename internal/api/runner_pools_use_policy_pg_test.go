// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The pool-use policy (DONE WHEN 6) against a real Postgres: it narrows a remote-provided pool and
// nothing else, who it narrows is matched by user, group and role, and under
// WARDYN_GOVERNANCE_SECOND_HUMAN a set and a clear are both held for a second human.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func usePolicyPath(pool uuid.UUID) string { return "/api/v1/runner-pools/" + pool.String() + "/use-policy" }

// ssoSessionWith is a member session carrying a group snapshot, which ssoSession leaves absent.
func ssoSessionWith(t *testing.T, sub, userType string, groups ...string) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(oidc.Session{V: oidc.SessionCodecVersion, Sub: sub, Email: sub + "@corp.example", Role: oidc.RoleUser,
		UserType: userType, Groups: groups, Expiry: time.Now().UTC().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return signedSessionCookie(payload)
}

func (e *poolEnv) policySubjects(pool uuid.UUID) []types.RunnerPoolSubject {
	e.t.Helper()
	w := e.call(e.alice, http.MethodGet, usePolicyPath(pool), "")
	if w.Code != http.StatusOK {
		e.t.Fatalf("read use policy = %d %s", w.Code, w.Body)
	}
	return decodeJSON[types.RunnerPoolUsePolicy](e.t, w).Subjects
}

func TestPG_RunnerPoolUsePolicyOnlyNarrowsARemoteProvidedPool(t *testing.T) {
	e := newPoolEnv(t, false)
	remote := e.createPool("Provided", types.RunnerPoolRemoteProvided)
	self := e.createPool("Laptops", types.RunnerPoolSelfHosted)
	if _, err := e.pg.CreateUserType(context.Background(), types.UserType{ID: "contractor", Name: "Contractor"}); err != nil {
		t.Fatal(err)
	}
	grouped := ssoSessionWith(t, "sub-grouped", types.UserTypeStandard, "pool-users")
	contractor := ssoSessionWith(t, "sub-contractor", "contractor", "")
	named := ssoSession(t, "sub-named", "named@corp.example", oidc.RoleUser)
	stranger := ssoSessionWith(t, "sub-stranger", types.UserTypeStandard, "elsewhere")
	staleGroups := ssoSession(t, "sub-stale", "stale@corp.example", oidc.RoleUser) // no group snapshot

	// Refused: a policy on a self-hosted pool, an empty or "all" list, an unknown type, a policy a
	// member writes. None leaves a row.
	for _, c := range []struct {
		name, path, body string
		who              *http.Cookie
		want             int
		reason           string
	}{
		{"self-hosted pool", usePolicyPath(self.ID), `{"subjects":[{"subject_type":"user","subject":"a"}]}`, e.alice, http.StatusUnprocessableEntity, string(runnerpool.ReasonMemberMismatch)},
		{"empty list", usePolicyPath(remote.ID), `{"subjects":[]}`, e.alice, http.StatusBadRequest, string(runnerpool.ReasonInvalid)},
		{"no list", usePolicyPath(remote.ID), `{}`, e.alice, http.StatusBadRequest, string(runnerpool.ReasonInvalid)},
		{"all", usePolicyPath(remote.ID), `{"subjects":[{"subject_type":"all","subject":"*"}]}`, e.alice, http.StatusBadRequest, string(runnerpool.ReasonInvalid)},
		{"a type that does not exist", usePolicyPath(remote.ID), `{"subjects":[{"subject_type":"user_type","subject":"ghost"}]}`, e.alice, http.StatusBadRequest, reasonAccessUnknownUserType},
		{"non-ASCII group", usePolicyPath(remote.ID), `{"subjects":[{"subject_type":"group","subject":"é"}]}`, e.alice, http.StatusBadRequest, string(runnerpool.ReasonInvalid)},
		{"a member", usePolicyPath(remote.ID), `{"subjects":[{"subject_type":"user","subject":"a"}]}`, e.person, http.StatusForbidden, ""},
		{"stale revision", usePolicyPath(remote.ID), `{"revision":99,"subjects":[{"subject_type":"user","subject":"a"}]}`, e.alice, http.StatusConflict, string(runnerpool.ReasonStale)},
	} {
		w := e.call(c.who, http.MethodPut, c.path, c.body)
		if w.Code != c.want || (c.reason != "" && wireReason(t, w) != c.reason) {
			t.Errorf("%s = %d %s, want %d %s", c.name, w.Code, w.Body, c.want, c.reason)
		}
	}
	if n := e.count(`SELECT count(*) FROM runner_pool_use_policies`); n != 0 {
		t.Fatalf("%d refused policies were stored", n)
	}
	if got := e.policySubjects(remote.ID); len(got) != 0 {
		t.Fatalf("an unset policy reads %v", got)
	}

	// Unset: everyone who may launch remote runs may use the pool.
	for _, c := range []*http.Cookie{e.person, grouped, contractor, stranger} {
		if _, ok := e.listed(c)[remote.ID]; !ok {
			t.Fatalf("with no policy the pool is hidden from someone")
		}
	}
	rev := e.revision(remote.ID)
	w := e.call(e.alice, http.MethodPut, usePolicyPath(remote.ID), `{"subjects":[
		{"subject_type":"group","subject":"Pool-Users"},{"subject_type":"user_type","subject":"contractor"},{"subject_type":"user","subject":"Named@Corp.Example"}]}`)
	if w.Code != http.StatusOK || e.revision(remote.ID) != rev+1 {
		t.Fatalf("set policy = %d %s (revision %d -> %d)", w.Code, w.Body, rev, e.revision(remote.ID))
	}
	if got := e.policySubjects(remote.ID); len(got) != 3 || got[0].Subject != "pool-users" || got[2].Subject != "named@corp.example" {
		t.Errorf("subjects are stored as written, not in matching form: %v", got)
	}
	// Narrowed: the listed people, groups and roles keep it; everyone else, a missing group snapshot
	// included, loses it, and the loss reads as "no such pool".
	for who, want := range map[string]bool{"group": true, "role": true, "user": true, "stranger": false, "no snapshot": false, "member": false} {
		c := map[string]*http.Cookie{"group": grouped, "role": contractor, "user": named, "stranger": stranger, "no snapshot": staleGroups, "member": e.person}[who]
		_, ok := e.listed(c)[remote.ID]
		if ok != want {
			t.Errorf("%s: pool listed = %v, want %v", who, ok, want)
		}
		if code := e.call(c, http.MethodGet, "/api/v1/runner-pools/"+remote.ID.String(), "").Code; (code == http.StatusOK) != want {
			t.Errorf("%s: GET pool = %d, want listed=%v", who, code, want)
		}
	}
	if got := e.listed(e.person)[self.ID]; got.ID != self.ID {
		t.Errorf("a use policy on one pool hid the self-hosted pool")
	}
	if len(e.audits(auditPoolUsePolicySet)) != 1 {
		t.Errorf("%d use-policy audit rows", len(e.audits(auditPoolUsePolicySet)))
	}

	// Cleared: back to everyone.
	if w := e.call(e.alice, http.MethodDelete, usePolicyPath(remote.ID), ""); w.Code != http.StatusNoContent {
		t.Fatalf("clear = %d %s", w.Code, w.Body)
	}
	if _, ok := e.listed(e.person)[remote.ID]; !ok {
		t.Errorf("a cleared policy still hides the pool")
	}
	if len(e.audits(auditPoolUsePolicyClear)) != 1 {
		t.Errorf("%d use-policy clear audit rows", len(e.audits(auditPoolUsePolicyClear)))
	}
}

func TestPG_RunnerPoolUsePolicyIsHeldForASecondHuman(t *testing.T) {
	e := newPoolEnv(t, true)
	remote := e.createPool("Provided", types.RunnerPoolRemoteProvided)
	const set = `{"subjects":[{"subject_type":"user","subject":"sub-named"}]}`

	held := e.pending(e.call(e.alice, http.MethodPut, usePolicyPath(remote.ID), set))
	if held.TargetKind != govKindRunnerPoolUsePolicy || held.Op != "set" || held.TargetKey != remote.ID.String() {
		t.Errorf("held = %+v", held)
	}
	if n := e.count(`SELECT count(*) FROM runner_pool_use_policies`); n != 0 {
		t.Fatalf("a held policy was stored: %d rows", n)
	}
	if _, ok := e.listed(e.person)[remote.ID]; !ok {
		t.Fatalf("a held policy already narrows the pool")
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(held.ID), ""); w.Code != http.StatusForbidden {
		t.Errorf("self-approval = %d, want 403", w.Code)
	}
	if w := e.call(e.user, http.MethodPost, approvePath(held.ID), ""); w.Code != http.StatusForbidden {
		t.Errorf("a member approving = %d, want 403", w.Code)
	}
	e.approve(held.ID)
	if got := e.policySubjects(remote.ID); len(got) != 1 || got[0].Subject != "sub-named" {
		t.Fatalf("approved policy = %v", got)
	}
	if _, ok := e.listed(e.person)[remote.ID]; ok {
		t.Errorf("an approved policy does not narrow the pool")
	}
	rows := e.audits(auditPoolUsePolicySet)
	if len(rows) != 1 || rows[0].Actor != govSubBob {
		t.Fatalf("audit rows = %+v, want one by the approver", rows)
	}

	// A clear widens, so it is held as well.
	clear := e.pending(e.call(e.carol, http.MethodDelete, usePolicyPath(remote.ID), ""))
	if clear.Op != "delete" || len(e.policySubjects(remote.ID)) != 1 {
		t.Fatalf("held clear = %+v, policy now %v", clear, e.policySubjects(remote.ID))
	}
	e.approve(clear.ID)
	if got := e.policySubjects(remote.ID); len(got) != 0 {
		t.Errorf("approved clear left %v", got)
	}

	// A change goes stale when the policy it reviewed has moved first; the admin token's write is the
	// break-glass one and leaves its own row.
	stale := e.pending(e.call(e.alice, http.MethodPut, usePolicyPath(remote.ID), set))
	if w := e.admin(http.MethodPut, usePolicyPath(remote.ID), `{"subjects":[{"subject_type":"user","subject":"sub-other"}]}`); w.Code != http.StatusOK {
		t.Fatalf("admin token write = %d %s", w.Code, w.Body)
	}
	if n := len(e.audits("governance.change.bypass")); n != 1 {
		t.Errorf("%d bypass rows, want 1", n)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(stale.ID), ""); w.Code == http.StatusOK {
		t.Fatalf("approving a stale change = %d", w.Code)
	}
	if got := e.policySubjects(remote.ID); len(got) != 1 || got[0].Subject != "sub-other" {
		t.Errorf("a stale approval changed the policy: %v", got)
	}
	// Clearing a pool with no policy is not a change and is not held.
	e.admin(http.MethodDelete, usePolicyPath(remote.ID), "")
	if w := e.call(e.alice, http.MethodDelete, usePolicyPath(remote.ID), ""); w.Code != http.StatusNoContent {
		t.Errorf("clearing nothing = %d %s", w.Code, w.Body)
	}
}
