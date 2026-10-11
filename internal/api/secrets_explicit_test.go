// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// #1510: the routes that say whose a stored secret is. The namespace follows the
// route, never the caller's role or view, and neither route can reach another
// person's namespace.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const explicitValue = `{"value":"explicit-route-secret-value"}`

func hasOwn(t *testing.T, sec *memSecrets, owner, name string) bool {
	t.Helper()
	return slices.Contains(namesOf(t, sec, owner), name)
}

func TestExplicitSecrets_PersonalWriteLandsInTheCallersNamespaceForEveryRole(t *testing.T) {
	for _, role := range []string{oidc.RoleUser, oidc.RoleSecurityAdmin, oidc.RoleAdmin} {
		t.Run(role, func(t *testing.T) {
			sec := &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
			h, srv := secretsRBACServer(t, sec)
			who := ssoSession(t, "alice", "alice@corp.example", role)
			if w := doSSO(t, srv, http.MethodPut, "/api/v1/me/secrets/npm-token", who, explicitValue); w.Code != http.StatusNoContent {
				t.Fatalf("PUT /me/secrets = %d %s", w.Code, w.Body)
			}
			if !hasOwn(t, sec, "alice", "npm-token") {
				t.Fatal("the personal write is not in alice's namespace")
			}
			if _, err := sec.Get(context.Background(), "npm-token"); !errors.Is(err, secretstore.ErrNotFound) {
				t.Fatalf("a personal write reached the shared namespace (err=%v)", err)
			}
			ev := lastAuditEvent(t, h.audit.events, "secret.write")
			if scope, _ := auditDataField(t, ev, "scope"); scope != secretScopePersonal {
				t.Errorf("audit scope = %q, want %q", scope, secretScopePersonal)
			}
			if owner, _ := auditDataField(t, ev, "secret_owner"); owner != "alice" {
				t.Errorf("audit secret_owner = %q, want alice", owner)
			}
			if w := doSSO(t, srv, http.MethodDelete, "/api/v1/me/secrets/npm-token", who, ""); w.Code != http.StatusNoContent {
				t.Fatalf("DELETE /me/secrets = %d %s", w.Code, w.Body)
			}
			if hasOwn(t, sec, "alice", "npm-token") {
				t.Fatal("the personal delete left alice's row")
			}
		})
	}
}

// The same person is personal on the personal route and shared on the shared one,
// before and after a role change: the role decides who MAY write shared, never
// where a route writes.
func TestExplicitSecrets_RoleChangeNeverFlipsPersonalAndShared(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	_, srv := secretsRBACServer(t, sec)
	asUser := ssoSession(t, "alice", "alice@corp.example", oidc.RoleUser)
	asAdmin := ssoSession(t, "alice", "alice@corp.example", oidc.RoleAdmin)

	if w := doSSO(t, srv, http.MethodPut, "/api/v1/shared-secrets/svc-token", asUser, explicitValue); w.Code != http.StatusForbidden {
		t.Fatalf("a user's shared write = %d, want 403", w.Code)
	}
	for i, who := range []*http.Cookie{asUser, asAdmin} {
		name := []string{"before-promotion", "after-promotion"}[i]
		if w := doSSO(t, srv, http.MethodPut, "/api/v1/me/secrets/"+name, who, explicitValue); w.Code != http.StatusNoContent {
			t.Fatalf("personal write %s = %d", name, w.Code)
		}
	}
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/shared-secrets/svc-token", asAdmin, explicitValue); w.Code != http.StatusNoContent {
		t.Fatalf("an admin's shared write = %d %s", w.Code, w.Body)
	}
	if !slices.Equal(namesOf(t, sec, "alice"), []string{"after-promotion", "before-promotion"}) {
		t.Errorf("alice's namespace = %v", namesOf(t, sec, "alice"))
	}
	if !slices.Equal(namesOf(t, sec, ""), []string{"svc-token"}) {
		t.Errorf("shared namespace = %v", namesOf(t, sec, ""))
	}
}

// The legacy route is unchanged, and is the ambiguity the explicit routes exist for: an admin's
// plain PUT still lands in the shared namespace.
func TestExplicitSecrets_LegacyRouteStaysRoleDependent(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	h, srv := secretsRBACServer(t, sec)
	admin := ssoSession(t, "alice", "alice@corp.example", oidc.RoleAdmin)
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/secrets/legacy-token", admin, explicitValue); w.Code != http.StatusNoContent {
		t.Fatalf("legacy PUT = %d", w.Code)
	}
	if hasOwn(t, sec, "alice", "legacy-token") || !slices.Equal(namesOf(t, sec, ""), []string{"legacy-token"}) {
		t.Fatalf("legacy admin write moved: alice=%v shared=%v", namesOf(t, sec, "alice"), namesOf(t, sec, ""))
	}
	if _, present := auditDataField(t, lastAuditEvent(t, h.audit.events, "secret.write"), "scope"); present {
		t.Error("the legacy route's audit row gained a scope")
	}
}

func TestExplicitSecrets_SharedWriteNeedsTheOperatorTierAndNeverTouchesAPerson(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	h, srv := secretsRBACServer(t, sec)
	for _, role := range []string{oidc.RoleUser, oidc.RoleSecurityAdmin} {
		who := ssoSession(t, "bob", "bob@corp.example", role)
		for _, m := range []string{http.MethodPut, http.MethodDelete} {
			if w := doSSO(t, srv, m, "/api/v1/shared-secrets/svc-token", who, explicitValue); w.Code != http.StatusForbidden {
				t.Errorf("%s /shared-secrets as %s = %d, want 403", m, role, w.Code)
			}
		}
		if w := doSSO(t, srv, http.MethodGet, "/api/v1/shared-secrets", who, ""); w.Code != http.StatusForbidden {
			t.Errorf("GET /shared-secrets as %s = %d, want 403", role, w.Code)
		}
	}
	admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
	if w := doSSO(t, srv, http.MethodPut, "/api/v1/shared-secrets/svc-token", admin, explicitValue); w.Code != http.StatusNoContent {
		t.Fatalf("operator shared PUT = %d %s", w.Code, w.Body)
	}
	ev := lastAuditEvent(t, h.audit.events, "secret.write")
	if scope, _ := auditDataField(t, ev, "scope"); scope != secretScopeShared {
		t.Errorf("audit scope = %q, want shared", scope)
	}
	if _, present := auditDataField(t, ev, "secret_owner"); present {
		t.Error("a shared write names a secret_owner")
	}
	if len(sec.owned) != 0 && len(sec.owned["admin-1"]) != 0 {
		t.Errorf("the shared write reached a person's namespace: %v", sec.owned)
	}
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/shared-secrets/svc-token", admin, ""); w.Code != http.StatusNoContent {
		t.Fatalf("operator shared DELETE = %d", w.Code)
	}
}

// A person's own delete removes their row and leaves the shared one of the same name.
func TestExplicitSecrets_DeleteIsScopedToItsRoute(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	_, srv := secretsRBACServer(t, sec)
	ctx := context.Background()
	_ = sec.Put(ctx, "both", []byte("shared-value-0000000"))
	_ = sec.For("alice").Put(ctx, "both", []byte("alice-value-00000000"))
	_ = sec.For("bob").Put(ctx, "both", []byte("bob-value-0000000000"))
	admin := ssoSession(t, "alice", "alice@corp.example", oidc.RoleAdmin)

	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/me/secrets/both", admin, ""); w.Code != http.StatusNoContent {
		t.Fatalf("personal DELETE = %d", w.Code)
	}
	if hasOwn(t, sec, "alice", "both") || !hasOwn(t, sec, "bob", "both") || !slices.Equal(namesOf(t, sec, ""), []string{"both"}) {
		t.Fatalf("a personal delete reached beyond alice: alice=%v bob=%v shared=%v", namesOf(t, sec, "alice"), namesOf(t, sec, "bob"), namesOf(t, sec, ""))
	}
	if w := doSSO(t, srv, http.MethodDelete, "/api/v1/shared-secrets/both", admin, ""); w.Code != http.StatusNoContent {
		t.Fatalf("shared DELETE = %d", w.Code)
	}
	if len(namesOf(t, sec, "")) != 0 || !hasOwn(t, sec, "bob", "both") {
		t.Fatalf("a shared delete touched a person: shared=%v bob=%v", namesOf(t, sec, ""), namesOf(t, sec, "bob"))
	}
}

func TestExplicitSecrets_NobodyWritesAnotherPersonsNamespace(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	h, srv := secretsRBACServer(t, sec)
	admin := ssoSession(t, "admin-1", "admin@corp.example", oidc.RoleAdmin)
	user := ssoSession(t, "bob", "bob@corp.example", oidc.RoleUser)
	cases := []struct {
		who          *http.Cookie
		method, path string
	}{
		{admin, http.MethodPut, "/api/v1/me/secrets/x?owner=bob"},
		{admin, http.MethodDelete, "/api/v1/me/secrets/x?owner=bob"},
		{admin, http.MethodPut, "/api/v1/shared-secrets/x?owner=bob"},
		{admin, http.MethodDelete, "/api/v1/shared-secrets/x?owner=bob"},
		{user, http.MethodPut, "/api/v1/me/secrets/x?owner=alice"},
	}
	for _, c := range cases {
		if w := doSSO(t, srv, c.method, c.path, c.who, explicitValue); w.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", c.method, c.path, w.Code)
		}
	}
	if len(sec.owned) != 0 || len(sec.m) != 0 {
		t.Fatalf("a refused ?owner= write stored something: %v %v", sec.owned, sec.m)
	}
	if lastAuditEvent(t, h.audit.events, "secret.write").Outcome != "denied" {
		t.Error("the refusal is not audited as denied")
	}
}

// The admin token and local mode have no person, so no personal namespace: a personal route
// answers 403 rather than guess a name that nothing reads.
func TestExplicitSecrets_PersonalNeedsAPerson(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	_, srv := secretsRBACServer(t, sec)
	for _, c := range []struct{ method, path string }{
		{http.MethodPut, "/api/v1/me/secrets/x"}, {http.MethodDelete, "/api/v1/me/secrets/x"}, {http.MethodGet, "/api/v1/me/secrets"},
	} {
		w := do(t, srv, c.method, c.path, adminToken, explicitValue)
		var body struct{ Reason string }
		_ = json.Unmarshal(w.Body.Bytes(), &body)
		if w.Code != http.StatusForbidden || body.Reason != reasonSecretPersonalNeedsPerson {
			t.Errorf("%s %s with the admin token = %d %s, want 403 %s", c.method, c.path, w.Code, w.Body, reasonSecretPersonalNeedsPerson)
		}
	}
	if len(sec.owned) != 0 || len(sec.m) != 0 {
		t.Fatalf("stored something: %v %v", sec.owned, sec.m)
	}
	// The admin token still sets a shared secret, explicitly.
	if w := do(t, srv, http.MethodPut, "/api/v1/shared-secrets/svc-token", adminToken, explicitValue); w.Code != http.StatusNoContent {
		t.Fatalf("admin-token shared PUT = %d %s", w.Code, w.Body)
	}
}

func TestExplicitSecrets_ListsAreScopedAndRoleIndependent(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	_, srv := secretsRBACServer(t, sec)
	ctx := context.Background()
	_ = sec.Put(ctx, "shared-one", []byte("shared-value-0000000"))
	_ = sec.For("alice").Put(ctx, "alice-one", []byte("alice-value-00000000"))
	admin := ssoSession(t, "alice", "alice@corp.example", oidc.RoleAdmin)
	read := func(path string) []string {
		w := doSSO(t, srv, http.MethodGet, path, admin, "")
		var body struct{ Names []string }
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("GET %s = %d %s", path, w.Code, w.Body)
		}
		return body.Names
	}
	if got := read("/api/v1/me/secrets"); !slices.Equal(got, []string{"alice-one"}) {
		t.Errorf("/me/secrets as an admin = %v, want only alice's own", got)
	}
	if got := read("/api/v1/shared-secrets"); !slices.Equal(got, []string{"shared-one"}) {
		t.Errorf("/shared-secrets = %v", got)
	}
}

// A delegated caller (a portal acting for someone) is refused as delegated on the personal routes,
// before ?owner= is looked at: the owner_param refusal must not mask the delegation one.
func TestExplicitSecrets_DelegationIsRefusedBeforeTheOwnerParam(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	h, srv := secretsRBACServer(t, sec)
	router := chi.NewRouter()
	router.Put("/me/secrets/{name}", srv.handlePutMySecret)
	router.Delete("/me/secrets/{name}", srv.handleDeleteMySecret)
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		r := httptest.NewRequest(method, "/me/secrets/x?owner=bob", strings.NewReader(explicitValue))
		r = r.WithContext(audit.WithDelegation(r.Context(), types.DelegationVia{Delegate: uuid.New(), Grant: uuid.New()}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), string(authz.ReasonDelegationScope)) {
			t.Errorf("%s with ?owner= as a delegate = %d %s, want 403 %s", method, w.Code, w.Body, authz.ReasonDelegationScope)
		}
	}
	for _, ev := range h.audit.events {
		if ev.Action == "secret.write" || ev.Action == "secret.delete" {
			if strings.Contains(string(ev.Data), "owner_param") {
				t.Errorf("a delegated request was audited as an owner_param refusal: %s", ev.Data)
			}
		}
	}
}
