// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"net/http/httptest"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// peopleStore serves one fixed directory page and counts the role-mapping reads a list makes.
type peopleStore struct {
	store.Store
	page     store.PeopleDirectoryPage
	failRead bool
	reads    atomic.Int32
}

func (p *peopleStore) ListPeopleDirectory(context.Context, store.PeopleDirectoryFilter) (store.PeopleDirectoryPage, error) {
	return p.page, nil
}

func (p *peopleStore) ListRoleMappings(context.Context) ([]types.RoleMapping, error) {
	p.reads.Add(1)
	if p.failRead {
		return nil, errors.New("role mappings unavailable")
	}
	return nil, nil
}

func (p *peopleStore) ListUserTypes(context.Context) ([]types.UserType, error) {
	p.reads.Add(1)
	return nil, nil
}

const peoplePortalIssuer = "https://idp.example.com/oauth2/default"

var (
	portalListing = store.PersonListing{
		Principal: types.PortalPrincipal(types.PortalIssuerKey(peoplePortalIssuer), "00uA"), Email: "sam@corp.example",
		PortalIssuer: peoplePortalIssuer, PortalInternalIssuer: "https://idp.internal.example.com/oauth2/default",
	}
	workforceListing = store.PersonListing{Principal: "sub-1", Email: "wes@corp.example"}
)

func newPeopleEnv(t *testing.T, st *peopleStore) (*Server, *httptest.Server) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = regConsoleAuth(t, regConsoleIssuer)
	srv := New(cfg)
	ts := httptest.NewServer(panicFails(t, srv.Handler()))
	t.Cleanup(ts.Close)
	return srv, ts
}

// A page of only portal people is answered without reading the role mappings or the user types, so a failing
// mapping read does not fail it: every portal person's role is user.
func TestPeopleList_PortalOnlyPageReadsNoRoleMapping(t *testing.T) {
	st := &peopleStore{page: store.PeopleDirectoryPage{People: []store.PersonListing{portalListing}}, failRead: true}
	_, ts := newPeopleEnv(t, st)
	list, err := client.New(ts.URL, adminToken).ListPeople(context.Background())
	if err != nil {
		t.Fatalf("a portal-only page with a failing mapping read: %v", err)
	}
	if n := st.reads.Load(); n != 0 {
		t.Errorf("mapping reads = %d, want 0", n)
	}
	if len(list.People) != 1 || list.People[0].Role != "user" {
		t.Errorf("people = %+v, want one portal person with role user", list.People)
	}
}

// A mixed page reads the mappings once, for the workforce row, and a failed read still fails the page.
func TestPeopleList_MixedPageReadsMappingsOnceAndFailureFailsIt(t *testing.T) {
	st := &peopleStore{page: store.PeopleDirectoryPage{People: []store.PersonListing{portalListing, workforceListing, {Principal: "sub-2", Email: "kim@corp.example"}}}}
	srv, ts := newPeopleEnv(t, st)
	list, err := client.New(ts.URL, adminToken).ListPeople(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n := st.reads.Load(); n != 2 {
		t.Errorf("reads = %d, want 2 (mappings and user types, once)", n)
	}
	if len(list.People) != 3 || list.People[0].Role != "user" {
		t.Errorf("people = %+v", list.People)
	}
	st.failRead = true
	if w := do(t, srv, http.MethodGet, "/api/v1/people", adminToken, ""); w.Code != http.StatusInternalServerError {
		t.Errorf("a failing mapping read on a page with workforce rows = %d, want 500", w.Code)
	}
}

// The typed client and the CLI's --json output (the same page re-marshalled) keep a portal person's issuer
// and internal address, and a workforce person carries neither.
func TestPeopleList_TypedClientKeepsPortalFields(t *testing.T) {
	st := &peopleStore{page: store.PeopleDirectoryPage{People: []store.PersonListing{portalListing, workforceListing}}}
	_, ts := newPeopleEnv(t, st)
	page, err := client.New(ts.URL, adminToken).ListPeople(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(page.People) != 2 {
		t.Fatalf("people = %+v", page.People)
	}
	portal, workforce := page.People[0], page.People[1]
	if portal.Issuer != peoplePortalIssuer || portal.InternalAddress != "https://idp.internal.example.com/oauth2/default" || portal.IssuerKind != issuerKindOIDC {
		t.Errorf("portal person = %+v", portal)
	}
	if workforce.Issuer != "" || workforce.InternalAddress != "" {
		t.Errorf("workforce person = %+v", workforce)
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var rows struct {
		People []map[string]any `json:"people"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if rows.People[0]["issuer"] != peoplePortalIssuer || rows.People[0]["internal_address"] == nil {
		t.Errorf("--json row = %v, want issuer and internal_address", rows.People[0])
	}
	if _, has := rows.People[1]["issuer"]; has {
		t.Errorf("--json workforce row carries issuer: %v", rows.People[1])
	}
}
