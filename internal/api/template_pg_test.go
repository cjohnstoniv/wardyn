// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_TemplateRoutesThroughSessionsAndTokens runs the template routes
// against the real Postgres store with SSO sessions and API tokens: a group
// manager publishes with either, a non-member reads the missing-id 404 for the
// group's template, a revoked grant stops the token's writes, every route
// answers non-501, and no stored row holds a value the door refused.
// Guarded by WARDYN_TEST_PG.
func TestPG_TemplateRoutesThroughSessionsAndTokens(t *testing.T) {
	const canary = "ghp_A1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6Q7r8"
	pool := throwawayPGPool(t)
	h := newHarness(t)
	cfg := baseTestConfig(h, store.NewPG(pool))
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)
	ctx := context.Background()

	if w := do(t, srv, http.MethodPut, "/api/v1/admin/template-group-admins", adminToken, `{"group_id":"Eng","person":"gary","label":"Engineering"}`); w.Code != http.StatusOK {
		t.Fatalf("grant = %d %s", w.Code, w.Body)
	}
	garySess := govSession(t, "gary", []string{"eng"}, false)
	garyTok, _ := mintToken(t, srv, garySess, "ci")
	bobTok, _ := mintToken(t, srv, govSession(t, "bob", []string{"ops"}, false), "ci")

	w := do(t, srv, http.MethodPost, "/api/v1/templates", garyTok, templateSaveBody("group", "eng", "via token"))
	if w.Code != http.StatusCreated {
		t.Fatalf("token publish = %d %s", w.Code, w.Body)
	}
	id := decodeTemplate(t, w.Body.String()).ID
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/templates", garySess, templateSaveBody("group", "eng", "via session")); w.Code != http.StatusCreated {
		t.Fatalf("session publish = %d %s", w.Code, w.Body)
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/templates/"+id.String(), govSession(t, "gina", []string{"eng"}, false), ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"group_label":"Engineering"`) {
		t.Fatalf("a member reads = %d %s", w.Code, w.Body)
	}
	missing := do(t, srv, http.MethodGet, "/api/v1/templates/"+uuid.NewString(), bobTok, "")
	foreign := do(t, srv, http.MethodGet, "/api/v1/templates/"+id.String(), bobTok, "")
	if foreign.Code != http.StatusNotFound || foreign.Body.String() != missing.Body.String() {
		t.Fatalf("a non-member's token reads %d %q, want the missing id's %q", foreign.Code, foreign.Body, missing.Body)
	}

	refused := `{"scope":"group","group_id":"eng","name":"x","document":{"api_version":"wardyn/v1","kind":"RunTemplate","coverage":"partial","description":"` +
		canary + `","intent":` + templateIntentEgress + `}}`
	if w := do(t, srv, http.MethodPost, "/api/v1/templates", garyTok, refused); w.Code < 400 || w.Code >= 500 {
		t.Fatalf("a value in the document = %d %s", w.Code, w.Body)
	}

	if w := do(t, srv, http.MethodDelete, "/api/v1/admin/template-group-admins", adminToken, `{"group_id":"eng","person":"gary"}`); w.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", w.Code, w.Body)
	}
	if w := do(t, srv, http.MethodPost, "/api/v1/templates", garyTok, templateSaveBody("group", "eng", "after revoke")); w.Code != http.StatusForbidden {
		t.Fatalf("token publish after the revoke = %d %s", w.Code, w.Body)
	}

	for _, row := range types.TemplateRoutes {
		method, path, _ := strings.Cut(row.Route, " ")
		path = strings.ReplaceAll(strings.ReplaceAll(path, "{id}", id.String()), "{revision}", "1")
		if w := do(t, srv, method, path, adminToken, `{}`); w.Code == http.StatusNotImplemented {
			t.Errorf("%s answers 501 on the Postgres store: %s", row.Route, w.Body)
		}
	}

	var leaked int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM run_template_revisions WHERE document::text LIKE '%' || $1 || '%' OR name LIKE '%' || $1 || '%' OR description LIKE '%' || $1 || '%') +
		(SELECT count(*) FROM run_templates WHERE name LIKE '%' || $1 || '%' OR description LIKE '%' || $1 || '%')`, canary).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("%d stored rows hold the refused value", leaked)
	}
}
