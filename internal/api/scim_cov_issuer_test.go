// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
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
