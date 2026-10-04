// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// keyDomainFixture is a server whose key-domain service declares domains a and
// b, over a real Postgres, with bob known to the directory.
func keyDomainFixture(t *testing.T) (*harness, *Server, *keydomain.Service) {
	t.Helper()
	h, srv, svc, _ := keyDomainFixturePool(t)
	return h, srv, svc
}

// keyDomainFixturePool is keyDomainFixture and the pool under it.
func keyDomainFixturePool(t *testing.T) (*harness, *Server, *keydomain.Service, *pgxpool.Pool) {
	t.Helper()
	pool := throwawayPGPool(t)
	svc := keydomain.NewService(pool, []string{"a", "b"})
	h := newHarness(t)
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	h.srv.cfg.Store = secretOwnerDirectory{toks: []types.APIToken{{ID: uuid.New(), Principal: "bob", Email: "bob@corp.example"}}}
	h.srv.cfg.KeyDomains = svc
	h.srv.router = h.srv.routes()
	return h, h.srv, svc, pool
}

func keyDomainPut(t *testing.T, srv *Server, subjectType, subject, body string) (int, string) {
	t.Helper()
	sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
	w := doSSO(t, srv, http.MethodPut, "/api/v1/key-domains/assignments/"+subjectType+"/"+url.PathEscape(subject), sa, body)
	return w.Code, w.Body.String()
}

func TestPG_KeyDomains_SetListDelete(t *testing.T) {
	h, srv, svc := keyDomainFixture(t)

	// An email resolves to the principal the directory knows.
	code, body := keyDomainPut(t, srv, "user", "bob@corp.example", `{"domain":"a"}`)
	if code != http.StatusCreated {
		t.Fatalf("PUT user = %d %s, want 201", code, body)
	}
	got, ok, err := svc.Get(t.Context(), "user", "bob")
	if err != nil || !ok || got.Domain != "a" || got.SetBy != "sec-1" {
		t.Fatalf("stored = (%+v, %v, %v); want bob -> a, set by sec-1", got, ok, err)
	}
	ev := lastAuditEvent(t, h.audit.events, "key_domain.assignment.set")
	var data map[string]any
	_ = json.Unmarshal(ev.Data, &data)
	if ev.Outcome != "success" || ev.Target != "user:bob" || data["domain"] != "a" || data["created"] != true || data["previous_domain"] != nil {
		t.Fatalf("set row = %s %s %s", ev.Outcome, ev.Target, ev.Data)
	}

	// Replacing it is a 200, and the row names what it replaced.
	if code, body = keyDomainPut(t, srv, "user", "bob", `{"domain":"b"}`); code != http.StatusOK {
		t.Fatalf("PUT again = %d %s, want 200", code, body)
	}
	ev = lastAuditEvent(t, h.audit.events, "key_domain.assignment.set")
	_ = json.Unmarshal(ev.Data, &data)
	if data["domain"] != "b" || data["previous_domain"] != "a" || data["created"] != false {
		t.Fatalf("replace row = %s", ev.Data)
	}

	// Group and all.
	if code, body = keyDomainPut(t, srv, "group", "eng", `{"domain":"a"}`); code != http.StatusCreated {
		t.Fatalf("PUT group = %d %s", code, body)
	}
	if code, body = keyDomainPut(t, srv, "all", "all", `{"domain":"default"}`); code != http.StatusCreated {
		t.Fatalf("PUT all = %d %s", code, body)
	}

	sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/key-domains", sa, "")
	var list keyDomainsResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &list) != nil {
		t.Fatalf("GET = %d %s", w.Code, w.Body.String())
	}
	if len(list.Assignments) != 3 || len(list.Domains) != 3 || list.Domains[0].Domain != keydomain.Default || !list.Domains[1].Declared {
		t.Fatalf("GET body = %s; want three assignments and default, a, b", w.Body.String())
	}

	// A delete is audited and removes it; a second finds none.
	w = doSSO(t, srv, http.MethodDelete, "/api/v1/key-domains/assignments/user/bob", sa, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d %s, want 204", w.Code, w.Body.String())
	}
	ev = lastAuditEvent(t, h.audit.events, "key_domain.assignment.delete")
	_ = json.Unmarshal(ev.Data, &data)
	if ev.Outcome != "success" || ev.Target != "user:bob" || data["domain"] != "b" {
		t.Fatalf("delete row = %s %s %s", ev.Outcome, ev.Target, ev.Data)
	}
	w = doSSO(t, srv, http.MethodDelete, "/api/v1/key-domains/assignments/user/bob", sa, "")
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), reasonKeyDomainAssignmentNotFound) {
		t.Fatalf("second DELETE = %d %s, want 404 %s", w.Code, w.Body.String(), reasonKeyDomainAssignmentNotFound)
	}
}

// A domain the file does not declare is refused with its own reason and an
// authz.denied row, and nothing is written.
func TestPG_KeyDomains_UnknownDomainIsRefused(t *testing.T) {
	h, srv, svc := keyDomainFixture(t)
	code, body := keyDomainPut(t, srv, "user", "bob", `{"domain":"ghost"}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "key_domain_unknown") || !strings.Contains(body, "ghost") {
		t.Fatalf("PUT to an undeclared domain = %d %s; want 422 key_domain_unknown", code, body)
	}
	ev := lastAuditEvent(t, h.audit.events, "authz.denied")
	if !strings.Contains(string(ev.Data), "key_domain_unknown") {
		t.Fatalf("authz.denied row = %s", ev.Data)
	}
	if list, _ := svc.List(t.Context()); len(list) != 0 {
		t.Fatalf("a refused write left %+v", list)
	}
}

// A group assignment that would leave someone in two groups of different
// domains, with no assignment of their own, is refused and counts them.
func TestPG_KeyDomains_AmbiguousMembershipIsRefused(t *testing.T) {
	h, srv, svc := keyDomainFixture(t)
	if err := svc.RecordLoginGroups(t.Context(), "gina", []string{"eng", "ops"}, false); err != nil {
		t.Fatal(err)
	}
	if code, body := keyDomainPut(t, srv, "group", "eng", `{"domain":"a"}`); code != http.StatusCreated {
		t.Fatalf("PUT eng = %d %s", code, body)
	}
	code, body := keyDomainPut(t, srv, "group", "ops", `{"domain":"b"}`)
	if code != http.StatusConflict || !strings.Contains(body, "key_domain_ambiguous_membership") || !strings.Contains(body, "1 people") {
		t.Fatalf("PUT ops to another domain = %d %s; want 409 key_domain_ambiguous_membership counting one person", code, body)
	}
	if !strings.Contains(string(lastAuditEvent(t, h.audit.events, "authz.denied").Data), "key_domain_ambiguous_membership") {
		t.Fatal("no authz.denied row for the ambiguity")
	}
	// The same domain, or a user assignment first, goes through.
	if code, body = keyDomainPut(t, srv, "group", "ops", `{"domain":"a"}`); code != http.StatusCreated {
		t.Fatalf("PUT ops to the same domain = %d %s", code, body)
	}
}

// A person whose last sign-in lost groups (an Entra overage always does) cannot be placed once any
// group is assigned, so a group write is refused while such a person has no user assignment of
// their own, and counts them. Assigning them as a user first lets it through.
func TestPG_KeyDomains_TruncatedLoginRefusesAGroupWrite(t *testing.T) {
	h, srv, svc := keyDomainFixture(t)
	if err := svc.RecordLoginGroups(t.Context(), "gina", []string{"eng"}, true); err != nil {
		t.Fatal(err)
	}
	code, body := keyDomainPut(t, srv, "group", "finance", `{"domain":"a"}`)
	if code != http.StatusConflict || !strings.Contains(body, "key_domain_ambiguous_membership") || !strings.Contains(body, "1 people") || strings.Contains(body, "sign in again") {
		t.Fatalf("PUT group beside a truncated login = %d %s; want 409 key_domain_ambiguous_membership counting one person", code, body)
	}
	if !strings.Contains(string(lastAuditEvent(t, h.audit.events, "authz.denied").Data), "key_domain_ambiguous_membership") {
		t.Fatal("no authz.denied row for the truncated login")
	}
	if list, _ := svc.List(t.Context()); len(list) != 0 {
		t.Fatalf("a refused write left %+v", list)
	}
	if code, body = keyDomainPut(t, srv, "user", "gina", `{"domain":"default"}`); code != http.StatusCreated {
		t.Fatalf("PUT user gina = %d %s", code, body)
	}
	if code, body = keyDomainPut(t, srv, "group", "finance", `{"domain":"a"}`); code != http.StatusCreated {
		t.Fatalf("PUT group once gina is a user = %d %s, want 201", code, body)
	}
}

func TestPG_KeyDomains_RequestValidation(t *testing.T) {
	_, srv, _ := keyDomainFixture(t)
	for name, tc := range map[string]struct {
		typ, subject, body string
		want               int
	}{
		"an unknown subject type":     {"team", "x", `{"domain":"a"}`, http.StatusBadRequest},
		"all with another subject":    {"all", "everyone", `{"domain":"a"}`, http.StatusBadRequest},
		"no domain":                   {"user", "bob", `{}`, http.StatusBadRequest},
		"an unknown field":            {"user", "bob", `{"domain":"a","extra":1}`, http.StatusBadRequest},
		"a non-ASCII group":           {"group", "gé", `{"domain":"a"}`, http.StatusBadRequest},
		"an email nobody is known by": {"user", "nobody@corp.example", `{"domain":"a"}`, http.StatusUnprocessableEntity},
	} {
		if code, body := keyDomainPut(t, srv, tc.typ, tc.subject, tc.body); code != tc.want {
			t.Errorf("%s: PUT = %d %s, want %d", name, code, body, tc.want)
		}
	}
}

// Only the security tier reaches the routes; a user is refused, and without a
// service the routes answer 501.
func TestPG_KeyDomains_Tiers(t *testing.T) {
	_, srv, _ := keyDomainFixture(t)
	user := ssoSession(t, "u-1", "u@corp.example", oidc.RoleUser)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/key-domains", ""},
		{http.MethodPut, "/api/v1/key-domains/assignments/user/bob", `{"domain":"a"}`},
		{http.MethodDelete, "/api/v1/key-domains/assignments/user/bob", ""},
	} {
		if w := doSSO(t, srv, c.method, c.path, user, c.body); w.Code != http.StatusForbidden {
			t.Errorf("%s %s as a user = %d, want 403", c.method, c.path, w.Code)
		}
	}
	admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/key-domains", admin, ""); w.Code != http.StatusOK {
		t.Errorf("GET as an admin = %d, want 200", w.Code)
	}
	srv.cfg.KeyDomains = nil
	sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/key-domains", sa, ""); w.Code != http.StatusNotImplemented {
		t.Errorf("GET with no service = %d, want 501", w.Code)
	}
}
