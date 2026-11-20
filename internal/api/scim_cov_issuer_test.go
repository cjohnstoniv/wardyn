// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// scimCovIssuerRows adds one identity per case a by-id route must tell apart: the endpoint's own bound user, a
// user of another issuer, a user of the same issuer under another tenant, and a same-issuer row with no tenant
// recorded.
func scimCovIssuerRows(st *scimCovStore) (own, foreignIssuer, foreignTenant, legacy store.PrincipalIdentity) {
	own = st.addIdentity(scimCovBound())
	foreignIssuer = st.addIdentity(store.PrincipalIdentity{
		Principal: "sub-other", Issuer: "https://idp.other.example", TenantID: scimCovTenant, ObjectID: "obj-other", EmailLower: "other@corp.example",
	})
	foreignTenant = st.addIdentity(store.PrincipalIdentity{
		Principal: "sub-tenant", Issuer: scimCovIssuer, TenantID: "99999999-8888-7777-6666-555555555555", ObjectID: "obj-tenant", EmailLower: "tenant@corp.example",
	})
	legacy = st.addIdentity(store.PrincipalIdentity{Principal: "sub-legacy", Issuer: scimCovIssuer, EmailLower: "legacy@corp.example"})
	return own, foreignIssuer, foreignTenant, legacy
}

// A by-id user route answers a user of another issuer or tenant as it answers an absent id, and reads or writes
// nothing past the lookup: the only store call is GetIdentity, and no row, audit entry or session cut changes.
func TestSCIMCovUserByIDRefusesAForeignIssuerAndTenant(t *testing.T) {
	routes := []struct{ method, body string }{
		{http.MethodGet, ""},
		{http.MethodPatch, scimCovPatch(scimCovReplace("active", "false"), scimCovReplace("userName", `"renamed"`))},
		{http.MethodPut, scimCovUserBody(scimCovOID, "renamed", false, "renamed@corp.example")},
		{http.MethodDelete, ""},
	}
	for _, foreign := range []string{"issuer", "tenant"} {
		for _, rt := range routes {
			t.Run(foreign+" "+rt.method, func(t *testing.T) {
				st := newSCIMCovStore()
				_, fIssuer, fTenant, _ := scimCovIssuerRows(st)
				target := fIssuer
				if foreign == "tenant" {
					target = fTenant
				}
				before := *st.idents[target.ID]
				e := newSCIMCovEnv(t, st)
				st.calls = nil

				w := e.scim(rt.method, "/scim/v2/Users/"+target.ID.String(), rt.body)
				scimCovWantError(t, w, http.StatusNotFound, "")
				if !strings.Contains(w.Body.String(), "no such user") {
					t.Errorf("%s: %s", rt.method, w.Body.String())
				}
				if got := *st.idents[target.ID]; got != before {
					t.Errorf("the foreign row changed: %+v, was %+v", got, before)
				}
				if slices.ContainsFunc(st.calls, func(c string) bool { return c != "GetIdentity" }) {
					t.Errorf("store calls past the lookup: %v", st.calls)
				}
				if len(st.updates) != 0 || len(st.plans) != 0 || len(st.purgeMarks) != 0 || len(st.jobs) != 0 {
					t.Errorf("a refused %s wrote: updates %v plans %v purge marks %v jobs %v", rt.method, st.updates, st.plans, st.purgeMarks, st.jobs)
				}
				if rows := e.h.audit.snapshot(); len(rows) != 0 {
					t.Errorf("a refused %s wrote audit rows: %v", rt.method, rows)
				}
			})
		}
	}
}

// A row of the endpoint's issuer with no tenant recorded is still the endpoint's own user.
func TestSCIMCovUserByIDKeepsALegacyRowWithoutATenant(t *testing.T) {
	st := newSCIMCovStore()
	_, _, _, legacy := scimCovIssuerRows(st)
	e := newSCIMCovEnv(t, st)

	w := e.scim(http.MethodGet, "/scim/v2/Users/"+legacy.ID.String(), "")
	if got := scimCovDecode[scimCovUser](t, w); w.Code != http.StatusOK || got.ID != legacy.ID.String() {
		t.Errorf("GET = %d %+v", w.Code, got)
	}
	w = e.scim(http.MethodPatch, "/scim/v2/Users/"+legacy.ID.String(), scimCovPatch(scimCovReplace("userName", `"legacy.renamed"`)))
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH = %d: %s", w.Code, w.Body.String())
	}
	if len(st.updates) != 1 || st.updates[0].ID != legacy.ID {
		t.Errorf("updates = %+v, want one for the legacy row", st.updates)
	}
}

// A group's members value names identity ids. An id of another issuer or tenant is treated as an unknown id: it
// joins no group, and a removal that names it cuts nothing.
func TestSCIMCovGroupMembersIgnoreAForeignIssuerAndTenant(t *testing.T) {
	st := newSCIMCovStore()
	own, fIssuer, fTenant, legacy := scimCovIssuerRows(st)
	e := newSCIMCovEnv(t, st)
	ids := []string{own.ID.String(), fIssuer.ID.String(), fTenant.ID.String(), legacy.ID.String(), uuid.NewString()}
	want := []uuid.UUID{own.ID, legacy.ID}

	w := e.scim(http.MethodPost, "/scim/v2/Groups", `{"externalId":"g-new","displayName":"New","members":[`+
		`{"value":"`+strings.Join(ids, `"},{"value":"`)+`"}]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST group = %d: %s", w.Code, w.Body.String())
	}
	created := scimCovDecode[scimCovGroupBody](t, w)
	gid, err := uuid.Parse(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.groupMembers(gid); !slices.Equal(got, want) {
		t.Errorf("members after create = %v, want %v", got, want)
	}

	g := st.addGroup("g-patch", "Patch")
	w = e.scim(http.MethodPatch, "/scim/v2/Groups/"+g.ID.String(), scimCovPatch(scimCovGroupAdd(ids...)))
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH add = %d: %s", w.Code, w.Body.String())
	}
	if got := st.groupMembers(g.ID); !slices.Equal(got, want) {
		t.Errorf("members after add = %v, want %v", got, want)
	}

	// A foreign row that is already a member (added before the boundary was enforced) is left alone by a removal that names it.
	held := st.addGroup("g-held", "Held", fIssuer.ID, fTenant.ID)
	st.calls = nil
	w = e.scim(http.MethodPatch, "/scim/v2/Groups/"+held.ID.String(), scimCovPatch(scimCovGroupRemove(fIssuer.ID.String(), fTenant.ID.String())))
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH remove = %d: %s", w.Code, w.Body.String())
	}
	if got := st.groupMembers(held.ID); !slices.Equal(got, []uuid.UUID{fIssuer.ID, fTenant.ID}) {
		t.Errorf("members after a foreign removal = %v", got)
	}
	if slices.Contains(st.calls, "StartGroupRemoval") || len(st.jobs) != 0 {
		t.Errorf("a foreign removal started a ledger: calls %v jobs %v", st.calls, st.jobs)
	}
}

// scimCovForeignHolds seeds a group that still holds a user of the endpoint's own, one of another issuer and one
// of another tenant (as an earlier build let them join), each with a token of its own principal.
func scimCovForeignHolds(st *scimCovStore) (g store.ScimGroup, own, fIssuer, fTenant store.PrincipalIdentity, foreignTokens []uuid.UUID) {
	own, fIssuer, fTenant, _ = scimCovIssuerRows(st)
	g = st.addGroup("g-held", "Held", own.ID, fIssuer.ID, fTenant.ID)
	for _, p := range []string{fIssuer.Principal, fTenant.Principal} {
		foreignTokens = append(foreignTokens, scimCovTokenIn(st, p, nil, nil))
	}
	return g, own, fIssuer, fTenant, foreignTokens
}

// wantNoForeignEffect fails when anything about a foreign identity was started, cut, revoked or audited.
func (e *scimCovEnv) wantNoForeignEffect(t *testing.T, st *scimCovStore, tokens []uuid.UUID, foreign ...store.PrincipalIdentity) {
	t.Helper()
	for _, tok := range tokens {
		if st.tokenRevoked(tok) {
			t.Errorf("a foreign member's token %v was revoked", tok)
		}
	}
	revoked, cut := e.rev.snapshot()
	for _, f := range foreign {
		for _, c := range append(revoked, cut...) {
			if strings.Contains(c, f.Principal) || strings.Contains(c, f.EmailLower) || strings.Contains(c, f.ID.String()) {
				t.Errorf("a cut or revoke names foreign %s: %q", f.Principal, c)
			}
		}
		for _, ev := range e.h.audit.snapshot() {
			if ev.Target == f.ID.String() || ev.Target == f.Principal {
				t.Errorf("an audit row names foreign %s: %s %s", f.Principal, ev.Action, ev.Target)
			}
		}
	}
}

// A foreign member the group still holds is left alone by a remove-all PATCH and by a group DELETE, while the
// endpoint's own member is removed: only the own member opens a ledger.
func TestSCIMCovRetainedForeignMembersAreNotRemoved(t *testing.T) {
	for _, c := range []struct {
		name, method, body string
	}{
		{"remove all", http.MethodPatch, scimCovPatch(`{"op":"Remove","path":"members"}`)},
		{"delete group", http.MethodDelete, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := newSCIMCovStore()
			g, own, fIssuer, fTenant, tokens := scimCovForeignHolds(st)
			e := newSCIMCovEnv(t, st)

			w := e.scim(c.method, "/scim/v2/Groups/"+g.ID.String(), c.body)
			if w.Code != http.StatusOK && w.Code != http.StatusNoContent {
				t.Fatalf("%s = %d: %s", c.method, w.Code, w.Body.String())
			}
			if got := st.callCount("StartGroupRemoval"); got != 1 {
				t.Errorf("StartGroupRemoval ran %d times, want once, for the own member", got)
			}
			if steps := st.pendingSteps(own.ID, store.JobKindGroupRemove); len(steps) != 0 {
				t.Errorf("the own member's removal is pending: %v", steps)
			}
			e.wantNoForeignEffect(t, st, tokens, fIssuer, fTenant)
		})
	}
}

// The sweeper's resume of a pending removal that belongs to a foreign identity does nothing for it: it is not
// resumed, and nothing is cut, revoked or audited. As for an absent identity, the ledger row is left as it is.
func TestSCIMCovSweeperResumeOfAForeignRemovalDoesNothing(t *testing.T) {
	for _, foreign := range []string{"issuer", "tenant"} {
		t.Run(foreign, func(t *testing.T) {
			st := newSCIMCovStore()
			_, fIssuer, fTenant, _ := scimCovIssuerRows(st)
			target := fIssuer
			if foreign == "tenant" {
				target = fTenant
			}
			tok := scimCovTokenIn(st, target.Principal, nil, nil)
			g := st.addGroup("g-resume", "Resume")
			for _, step := range store.GroupRemovalSteps {
				st.jobs = append(st.jobs, &store.DeprovisionJob{IdentityID: target.ID, Kind: store.JobKindGroupRemove, Step: step, Target: g.ID.String()})
			}
			e := newSCIMCovEnv(t, st)

			if err := e.srv.removeGroupMember(context.Background(), st, g, target.ID, scimSweeperSlot, true); err != nil {
				t.Fatalf("removeGroupMember = %v, want nil", err)
			}
			if got := st.callCount("ResumeGroupRemoval") + st.callCount("StartGroupRemoval"); got != 0 {
				t.Errorf("a ledger was resumed or started %d times", got)
			}
			if len(st.jobs) != len(store.GroupRemovalSteps) {
				t.Errorf("the ledger rows changed: %+v", st.jobs)
			}
			for _, j := range st.jobs {
				if j.Done {
					t.Errorf("ledger step %s was settled: %+v", j.Step, j)
				}
			}
			e.wantNoForeignEffect(t, st, []uuid.UUID{tok}, target)
		})
	}
}

// One failed read of a member must not hold back the removal of the others named in the request: the request
// answers a retryable failure, the member whose read failed stays a member for the retry, the other is removed,
// and the retry finishes the first.
func TestSCIMCovAFailedMemberReadDoesNotSuppressOtherNamedRemovals(t *testing.T) {
	st := newSCIMCovStore()
	p1, p2 := scimCovPerson(st, "sub-p1"), scimCovPerson(st, "sub-p2")
	g := st.addGroup(scimCovGroupExternal, "Engineering", p1.ID, p2.ID)
	tok1, tok2 := scimCovTokenIn(st, "sub-p1", nil, nil), scimCovTokenIn(st, "sub-p2", nil, nil)
	e := newSCIMCovEnv(t, st)
	path := "/scim/v2/Groups/" + g.ID.String()
	body := scimCovPatch(scimCovGroupRemove(p1.ID.String(), p2.ID.String()))

	st.failAfter("GetIdentity", errSCIMCovBoom, 0)
	scimCovWantRetry(t, e.scim(http.MethodPatch, path, body), "secret-dsn")
	if got := st.groupMembers(g.ID); !slices.Equal(got, []uuid.UUID{p1.ID}) {
		t.Errorf("members after the failed request = %v, want only the first, left for the retry", got)
	}
	if !st.tokenRevoked(tok2) || st.tokenRevoked(tok1) || len(st.pendingSteps(p2.ID, store.JobKindGroupRemove)) != 0 {
		t.Errorf("second member done = %t, first revoked = %t, second pending %v", st.tokenRevoked(tok2), st.tokenRevoked(tok1), st.pendingSteps(p2.ID, store.JobKindGroupRemove))
	}
	if got := len(e.auditRows("scim.group.member_remove")); got != 1 {
		t.Errorf("scim.group.member_remove rows = %d, want the second member's only", got)
	}

	if w := e.scim(http.MethodPatch, path, body); w.Code != http.StatusOK {
		t.Fatalf("retry = %d %s", w.Code, w.Body.String())
	}
	first := 0
	for _, ev := range e.auditRows("scim.group.member_remove") {
		if ev.Target == p1.ID.String() {
			first++
		}
	}
	if !st.tokenRevoked(tok1) || first != 1 || len(st.pendingSteps(p1.ID, store.JobKindGroupRemove)) != 0 {
		t.Errorf("after the retry: first revoked = %t, its rows %d, pending %v", st.tokenRevoked(tok1), first, st.pendingSteps(p1.ID, store.JobKindGroupRemove))
	}
}
