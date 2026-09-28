// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestIntegrationWriteRoutesRequireAdminAuth mirrors
// TestWorkspaceRoutesRequireAdminAuth (workspaces_test.go): the three write
// routes are operatorOnly, so they must fail closed on auth BEFORE ever
// touching the (here, unconfigured) Store.
func TestIntegrationWriteRoutesRequireAdminAuth(t *testing.T) {
	h := newHarness(t)
	cases := []struct{ method, path string }{
		{http.MethodPut, "/api/v1/integrations/acme-anthropic"},
		{http.MethodDelete, "/api/v1/integrations/acme-anthropic"},
	}
	for _, c := range cases {
		if w := do(t, h.srv, c.method, c.path, "", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s no token: code = %d, want 401", c.method, c.path, w.Code)
		}
		if w := do(t, h.srv, c.method, c.path, "wrong", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s wrong token: code = %d, want 401", c.method, c.path, w.Code)
		}
	}
}

// integrationWriteHarness builds a Server with a fakeSiteConfigStore seeded
// with `stored`, for the functional PUT/DELETE tests below.
func integrationWriteHarness(t *testing.T, stored []types.Integration) (*Server, *fakeSiteConfigStore, *recRecorder) {
	t.Helper()
	h := newHarness(t)
	fake := &fakeSiteConfigStore{cfg: types.SiteConfig{Integrations: stored}}
	cfg := baseTestConfig(h, fake)
	cfg.Secrets = &memSecrets{m: map[string][]byte{"acme-app-id": []byte("123"), "acme-app-key": []byte("key")}}
	return New(cfg), fake, h.audit
}

func TestHandlePutIntegration_CreatesStoredRow(t *testing.T) {
	srv, fake, audit := integrationWriteHarness(t, nil)
	body := `{"name":"Acme App","kind":"github_app",` +
		`"secrets":[{"role":"app_id","secret_name":"acme-app-id"},{"role":"app_key","secret_name":"acme-app-key"}]}`
	w := do(t, srv, http.MethodPut, "/api/v1/integrations/acme-app", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fake.cfg.Integrations) != 1 || fake.cfg.Integrations[0].ID != "acme-app" {
		t.Fatalf("stored integrations = %+v, want exactly one with id acme-app", fake.cfg.Integrations)
	}
	if fake.cfg.Integrations[0].CreatedAt.IsZero() || fake.cfg.Integrations[0].UpdatedAt.IsZero() {
		t.Error("expected CreatedAt/UpdatedAt to be stamped")
	}
	var got SetupIntegration
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Source != "stored" || got.ID != "acme-app" {
		t.Errorf("response row = %+v, want source=stored id=acme-app", got.integrationRow)
	}
	if n := auditCount(audit, "integration.write"); n != 1 {
		t.Errorf("integration.write audit events = %d, want 1", n)
	}
}

// A legacy-shaped PUT body (category/type/hosts/header/credentials) is
// rejected by decodeStrict — writes are NEW-SHAPE ONLY (the read-time fold is
// one-way; see types.Integration.UnmarshalJSON's doc).
func TestHandlePutIntegration_LegacyShapeBodyIs400(t *testing.T) {
	srv, fake, _ := integrationWriteHarness(t, nil)
	body := `{"name":"Acme Anthropic","category":"ai_provider","type":"anthropic_api_key",` +
		`"credentials":{"api_key":"acme-anthropic-key"}}`
	w := do(t, srv, http.MethodPut, "/api/v1/integrations/acme-anthropic", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (writes are new-shape only); body=%s", w.Code, w.Body.String())
	}
	if len(fake.cfg.Integrations) != 0 {
		t.Errorf("a rejected write must persist nothing; got %+v", fake.cfg.Integrations)
	}
}

func TestHandlePutIntegration_UpdatesExistingRow_PreservesCreatedAt(t *testing.T) {
	firstCreated := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	existing := types.Integration{
		ID: "acme-app", Kind: types.IntegrationKindGitHubApp,
		Secrets:   []types.IntegrationSecret{{Role: "app_id", SecretName: "acme-app-id"}},
		CreatedAt: firstCreated, UpdatedAt: firstCreated,
	}
	srv, fake, _ := integrationWriteHarness(t, []types.Integration{existing})

	body := `{"name":"Acme App (renamed)","kind":"github_app",` +
		`"secrets":[{"role":"app_id","secret_name":"acme-app-id"}]}`
	w := do(t, srv, http.MethodPut, "/api/v1/integrations/acme-app", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fake.cfg.Integrations) != 1 {
		t.Fatalf("expected the update to REPLACE the row, not append; got %+v", fake.cfg.Integrations)
	}
	if got := fake.cfg.Integrations[0].Name; got != "Acme App (renamed)" {
		t.Errorf("name = %q, want the new value", got)
	}
	if fake.cfg.Integrations[0].CreatedAt != firstCreated {
		t.Errorf("CreatedAt changed on update: got %v, want the original %v", fake.cfg.Integrations[0].CreatedAt, firstCreated)
	}
	if !fake.cfg.Integrations[0].UpdatedAt.After(firstCreated) {
		t.Errorf("UpdatedAt = %v, want a fresh timestamp after %v", fake.cfg.Integrations[0].UpdatedAt, firstCreated)
	}
}

func TestHandlePutIntegration_ValidationRejections(t *testing.T) {
	cases := []struct {
		name, id, body string
	}{
		{"bad id shape (uppercase, from the URL)", "Bad_ID!", `{"kind":"anthropic_api_key"}`},
		{"kind not a slug", "acme-feed", `{"kind":"Not A Slug!"}`},
		{"reserved credential secret", "acme-anthropic", `{"kind":"anthropic_api_key","secrets":[{"role":"api_key","secret_name":"wardyn-signing-key"}]}`},
		{"bad credential secret shape", "acme-anthropic", `{"kind":"anthropic_api_key","secrets":[{"role":"api_key","secret_name":"Not Valid!"}]}`},
		{"duplicate secret role", "acme-anthropic", `{"kind":"anthropic_api_key","secrets":[{"role":"api_key","secret_name":"acme-anthropic-key"},{"role":"api_key","secret_name":"acme-anthropic-key"}]}`},
		{"unknown default_for", "acme-anthropic", `{"kind":"anthropic_api_key","default_for":["bogus"]}`},
		{"bedrock half-set region only", "acme-bedrock", `{"kind":"bedrock","config":{"region":"us-east-1"}}`},
		{"bedrock half-set model only", "acme-bedrock", `{"kind":"bedrock","config":{"model":"anthropic.claude-3"}}`},

		// Config keys are CLOSED per closed kind — a typo'd key 400s by name
		// instead of silently storing config nothing reads.
		{"unknown config key on a closed kind", "acme-anthropic", `{"kind":"anthropic_api_key","config":{"regoin":"us-east-1"}}`},
		{"bedrock legacy lane key (renamed auth_lane)", "acme-bedrock", `{"kind":"bedrock","config":{"lane":"auto","region":"us-east-1","model":"anthropic.claude-3"}}`},
		// bug-integrations-2: resolveBedrockAuth reads FOUR FIXED global
		// secret names, never this row's own secret_name — a row naming
		// anything else is decorative (the write succeeds, the stored
		// secret is silently never read). Reject at write time.
		{"bedrock secret_name not one of the four fixed global names", "acme-bedrock",
			`{"kind":"bedrock","config":{"region":"us-east-1","model":"anthropic.claude-3"},"secrets":[{"role":"access_key","secret_name":"acme-bedrock-key"}]}`},

		// A generic kind's secret rows must SAY how they deliver — the row is
		// the whole contract; only a closed kind's bespoke transport may omit it.
		{"generic secret without a delivery", "acme-feed", `{"kind":"artifactory","secrets":[{"role":"token","secret_name":"acme-anthropic-key"}]}`},
		{"unknown delivery mode", "acme-feed", `{"kind":"artifactory","secrets":[{"role":"token","secret_name":"acme-anthropic-key","delivery":{"mode":"carrier_pigeon"}}]}`},
		// Resident deliveries are refused OUTRIGHT, not merely shape-checked:
		// there is no generic lane that materializes a named secret into a
		// sandbox path/env var, so storing one would be a promise nothing keeps
		// (the resident lanes that DO exist are per-provider and declare no
		// delivery at all). See residentDeliveryRefusal.
		{"resident_file delivery", "acme-feed", `{"kind":"artifactory","secrets":[{"role":"token","secret_name":"acme-anthropic-key","delivery":{"mode":"resident_file","path":"/etc/acme/token"}}]}`},
		{"resident_env delivery", "acme-feed", `{"kind":"artifactory","secrets":[{"role":"token","secret_name":"acme-anthropic-key","delivery":{"mode":"resident_env","var":"ACME_TOKEN"}}]}`},
		{"cross-mode fields on a delivery", "acme-feed", `{"kind":"artifactory","secrets":[{"role":"token","secret_name":"acme-anthropic-key","delivery":{"mode":"proxy_header","header":"Authorization","path":"/etc/acme/token"}}]}`},

		// One credential header per row: the proxy injector is keyed by HOST and
		// every proxy_header secret targets this row's whole egress list, so a
		// second one has nowhere of its own to go.
		{"two proxy_header secrets", "acme-feed",
			`{"kind":"artifactory","egress":["artifactory.corp.internal"],"secrets":[` +
				`{"role":"token","secret_name":"acme-anthropic-key","delivery":{"mode":"proxy_header","header":"Authorization"}},` +
				`{"role":"other","secret_name":"acme-openai-key","delivery":{"mode":"proxy_header","header":"X-Other"}}]}`},

		// Egress — the same shape rule every policy allowlist entry runs.
		{"host is a URL, not a host", "acme-feed", `{"kind":"artifactory","egress":["https://artifactory.corp.internal/repo"]}`},
		{"host has a mid-label wildcard that can never match", "acme-feed", `{"kind":"artifactory","egress":["oidc.*.amazonaws.com"]}`},
		{"host has a malformed port qualifier", "acme-db", `{"kind":"postgres","egress":["db.corp.internal:not-a-port"]}`},

		// The wildcard/injection interaction: a credential header is only ever
		// added to an EXACT allowlist entry, so this row would open the path and
		// silently never present the credential.
		{"wildcard host on a header-delivering integration", "acme-feed",
			`{"kind":"artifactory","egress":["*.corp.internal"],"secrets":[{"role":"token","secret_name":"acme-anthropic-key","delivery":{"mode":"proxy_header","header":"Authorization"}}]}`},
		// Worse than the wildcard: a port-qualified entry compiles into
		// allowedExactPort, which AllowedExactHost never consults, so
		// buildInjector REFUSES the rule and the proxy fails closed at startup —
		// a bricked run rather than a merely uncredentialed one.
		{"port-qualified host on a header-delivering integration", "acme-feed",
			`{"kind":"artifactory","egress":["nexus.corp.internal:8443"],"secrets":[{"role":"token","secret_name":"acme-anthropic-key","delivery":{"mode":"proxy_header","header":"Authorization"}}]}`},

		// Header name — the trust boundary. CRLF is the header-splitting shape.
		{"header name with CRLF", "acme-feed",
			`{"kind":"artifactory","egress":["artifactory.corp.internal"],"secrets":[{"role":"token","secret_name":"acme-anthropic-key","delivery":{"mode":"proxy_header","header":"X-Tok\r\nX-Evil: 1"}}]}`},
		{"header name with a bare newline", "acme-feed",
			`{"kind":"artifactory","egress":["artifactory.corp.internal"],"secrets":[{"role":"token","secret_name":"acme-anthropic-key","delivery":{"mode":"proxy_header","header":"X-Tok\nX-Evil: 1"}}]}`},
		{"header name containing a colon", "acme-feed",
			`{"kind":"artifactory","egress":["artifactory.corp.internal"],"secrets":[{"role":"token","secret_name":"acme-anthropic-key","delivery":{"mode":"proxy_header","header":"Authorization: Bearer"}}]}`},
		{"header name containing a space", "acme-feed",
			`{"kind":"artifactory","egress":["artifactory.corp.internal"],"secrets":[{"role":"token","secret_name":"acme-anthropic-key","delivery":{"mode":"proxy_header","header":"X Tok"}}]}`},
		{"proxy_header delivery with no header at all", "acme-feed",
			`{"kind":"artifactory","egress":["artifactory.corp.internal"],"secrets":[{"role":"token","secret_name":"acme-anthropic-key","delivery":{"mode":"proxy_header"}}]}`},
		{"proxy_header delivery names no secret", "acme-feed",
			`{"kind":"artifactory","egress":["artifactory.corp.internal"],"secrets":[{"role":"token","secret_name":"","delivery":{"mode":"proxy_header","header":"Authorization"}}]}`},

		// Format — the other half of the same wire value.
		{"format with no %s drops the credential silently", "acme-feed",
			`{"kind":"artifactory","egress":["artifactory.corp.internal"],"secrets":[{"role":"token","secret_name":"acme-anthropic-key","delivery":{"mode":"proxy_header","header":"Authorization","format":"Bearer"}}]}`},
		{"format with two verbs", "acme-feed",
			`{"kind":"artifactory","egress":["artifactory.corp.internal"],"secrets":[{"role":"token","secret_name":"acme-anthropic-key","delivery":{"mode":"proxy_header","header":"Authorization","format":"Bearer %s %s"}}]}`},
		{"format with a line break", "acme-feed",
			`{"kind":"artifactory","egress":["artifactory.corp.internal"],"secrets":[{"role":"token","secret_name":"acme-anthropic-key","delivery":{"mode":"proxy_header","header":"Authorization","format":"Bearer %s\r\nX-Evil: 1"}}]}`},

		// Probe — stored now, probed by B2; still validated at write.
		{"probe with a non-GET/HEAD method", "acme-feed",
			`{"kind":"artifactory","probe":{"method":"POST","url":"https://artifactory.corp.internal/"}}`},
		{"probe with a junk URL", "acme-feed",
			`{"kind":"artifactory","probe":{"method":"GET","url":"not a url"}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, fake, _ := integrationWriteHarness(t, nil)
			w := do(t, srv, http.MethodPut, "/api/v1/integrations/"+c.id, adminToken, c.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if len(fake.cfg.Integrations) != 0 {
				t.Errorf("a rejected write must persist nothing; got %+v", fake.cfg.Integrations)
			}
		})
	}
}

// Generic kinds are refused: a kind Wardyn has no code for (artifactory,
// postgres) cannot be written with its own egress + proxy-header delivery.
// Connections are the four Settings cards, over closed kinds only.
//
// The two properties that matter are pinned below: the refusal itself, and the
// fact that a row stored before the refusal existed is not destroyed by it (the
// read-time fold is a passthrough, so site config keeps it and
// integrations_run.go keeps injecting it — it just can't be edited here).
func TestHandlePutIntegration_GenericKindIsRefused(t *testing.T) {
	srv, fake, _ := integrationWriteHarness(t, nil)
	body := `{"name":"Corp Artifactory","kind":"artifactory",` +
		`"egress":["artifactory.corp.internal"],` +
		`"secrets":[{"role":"token","secret_name":"acme-anthropic-key",` +
		`"delivery":{"mode":"proxy_header","header":"Authorization","format":"Bearer %s"}}]}`
	w := do(t, srv, http.MethodPut, "/api/v1/integrations/corp-artifactory", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "not a supported integration kind") {
		t.Errorf("body = %s, want the refusal to name the rule", w.Body.String())
	}
	// The error names what IS accepted, so the operator isn't left guessing.
	if !strings.Contains(w.Body.String(), types.IntegrationKindGitHost) {
		t.Errorf("body = %s, want the closed-kind list in the message", w.Body.String())
	}
	if len(fake.cfg.Integrations) != 0 {
		t.Errorf("stored %+v, want nothing written on a refused kind", fake.cfg.Integrations)
	}
}

// An egress-only generic row (no proxy-header secret) is refused for the same
// reason — the kind gate runs before any delivery check.
func TestHandlePutIntegration_GenericEgressOnlyIsRefused(t *testing.T) {
	srv, _, _ := integrationWriteHarness(t, nil)
	body := `{"name":"Prod Postgres","kind":"postgres","egress":["*.db.corp.internal:5432"]}`
	w := do(t, srv, http.MethodPut, "/api/v1/integrations/prod-postgres", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// A generic row STORED under an earlier release still reads back with its kind,
// egress and delivery intact — the removal refuses new writes, it does not erase
// what an operator already configured.
func TestGenericKindStoredEarlier_StillReadsBack(t *testing.T) {
	stored := types.Integration{
		ID: "corp-artifactory", Name: "Corp Artifactory", Kind: "artifactory",
		Egress: []string{"artifactory.corp.internal"},
		Secrets: []types.IntegrationSecret{{
			Role: "token", SecretName: "acme-anthropic-key",
			Delivery: &types.IntegrationDelivery{Mode: types.DeliveryProxyHeader, Header: "Authorization", Format: "Bearer %s"},
		}},
	}
	srv, _, _ := integrationWriteHarness(t, []types.Integration{stored})
	w := do(t, srv, http.MethodGet, "/api/v1/integrations", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "artifactory.corp.internal") {
		t.Errorf("body = %s, want the legacy generic row's egress still present", w.Body.String())
	}
}

// TestHandlePutIntegration_AIKindIsRefused pins #547: model access is a model
// provider, so an integration write naming any of the four AI kinds is refused,
// pointing at Settings → Model providers, and nothing is stored.
func TestHandlePutIntegration_AIKindIsRefused(t *testing.T) {
	for _, kind := range []string{types.IntegrationKindAnthropicAPIKey, types.IntegrationKindAnthropicSubscription,
		types.IntegrationKindBedrock, types.IntegrationKindOpenAIAPIKey} {
		srv, fake, audit := integrationWriteHarness(t, nil)
		w := do(t, srv, http.MethodPut, "/api/v1/integrations/acme-model", adminToken, `{"kind":"`+kind+`"}`)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "Settings → Model providers") {
			t.Errorf("PUT kind %q: %d %s, want 400 naming Settings → Model providers", kind, w.Code, w.Body.String())
		}
		if len(fake.cfg.Integrations) != 0 || auditCount(audit, "integration.write") != 0 {
			t.Errorf("PUT kind %q persisted %+v", kind, fake.cfg.Integrations)
		}
	}
}

// TestHandlePutIntegration_DefaultForIsRefused: default_for marked an AI-kind
// row as a site default. No writable kind takes it, so the strict decode
// refuses the field by name on every kind.
func TestHandlePutIntegration_DefaultForIsRefused(t *testing.T) {
	srv, fake, _ := integrationWriteHarness(t, nil)
	body := `{"kind":"github_app","default_for":["agent_runs"]}`
	w := do(t, srv, http.MethodPut, "/api/v1/integrations/acme-app", adminToken, body)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "default_for") {
		t.Fatalf("code = %d, want 400 naming default_for; body=%s", w.Code, w.Body.String())
	}
	if len(fake.cfg.Integrations) != 0 {
		t.Errorf("rejected write must not persist, got %+v", fake.cfg.Integrations)
	}
}

func TestHandleDeleteIntegration_RemovesStoredRow(t *testing.T) {
	existing := types.Integration{ID: "acme-anthropic", Kind: types.IntegrationKindAnthropicAPIKey}
	srv, fake, audit := integrationWriteHarness(t, []types.Integration{existing})
	w := do(t, srv, http.MethodDelete, "/api/v1/integrations/acme-anthropic", adminToken, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("code = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if len(fake.cfg.Integrations) != 0 {
		t.Errorf("expected the row to be removed, got %+v", fake.cfg.Integrations)
	}
	if n := auditCount(audit, "integration.delete"); n != 1 {
		t.Errorf("integration.delete audit events = %d, want 1", n)
	}
}

func TestHandleDeleteIntegration_UnknownIDIs404(t *testing.T) {
	srv, _, _ := integrationWriteHarness(t, nil)
	w := do(t, srv, http.MethodDelete, "/api/v1/integrations/does-not-exist", adminToken, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestPutIntegration_ColonIDRoundTrips keeps the colon-id invariant alive. A
// colon-qualified id ("git_host:ghes.corp.example", the id legacy derivation
// mints) must survive a PUT: the URL carries the percent-encoded colon a real
// browser fetch() sends (encodeURIComponent), so this exercises
// integrationIDParam's unescape and validateIntegrationWrite's id gate, which
// must not 400 on it.
func TestPutIntegration_ColonIDRoundTrips(t *testing.T) {
	srv, fake, _ := integrationWriteHarness(t, nil)
	const id = "git_host:ghes.corp.example"
	escapedPath := "/api/v1/integrations/" + url.PathEscape(id)

	body := `{"name":"ghes.corp.example","kind":"git_host"}`
	w := do(t, srv, http.MethodPut, escapedPath, adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT a colon id: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fake.cfg.Integrations) != 1 {
		t.Fatalf("expected exactly one stored row, got %+v", fake.cfg.Integrations)
	}
	got := fake.cfg.Integrations[0]
	if got.ID != id {
		t.Errorf("stored row id = %q, want the colon id retained", got.ID)
	}

	// PUT is create-or-REPLACE: the same id again must not duplicate.
	if w = do(t, srv, http.MethodPut, escapedPath, adminToken, body); w.Code != http.StatusOK {
		t.Fatalf("second PUT: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fake.cfg.Integrations) != 1 {
		t.Fatalf("PUT must replace, not duplicate: got %+v", fake.cfg.Integrations)
	}
}

// auditCount counts recorded audit events with the given action.
func auditCount(audit *recRecorder, action string) int {
	n := 0
	for _, ev := range audit.events {
		if ev.Action == action {
			n++
		}
	}
	return n
}

// R5: putIntegrationRequest's doc comment prescribed a procedure the
// handler rejects — "a caller must round-trip a GET first to preserve fields it
// does not intend to change", while decodeStrict 400s the very first field
// every GET emits ("unknown field \"id\""). The contract chosen is the strict
// one: decodeStrict stays (a typo'd field name must not be silently dropped on
// a corp-wide write), and the comment now names what a caller has to drop.
// This checks each half of that comment against the running handler.
func TestPutIntegration_DocumentedProcedureIsTheOneThatWorks(t *testing.T) {
	srv, _, _ := integrationWriteHarness(t, []types.Integration{{
		ID: "acme-app", Name: "Acme", Kind: types.IntegrationKindGitHubApp,
		Secrets: []types.IntegrationSecret{{Role: "app_id", SecretName: "acme-app-id"}},
		Egress:  []string{"github.example.com"},
	}})

	// The GET half of the documented round trip.
	w := do(t, srv, http.MethodGet, "/api/v1/integrations", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /integrations: code = %d, body=%s", w.Code, w.Body.String())
	}
	var listed struct {
		Integrations []map[string]any `json:"integrations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var row map[string]any
	for _, r := range listed.Integrations {
		if r["id"] == "acme-app" {
			row = r
		}
	}
	if row == nil {
		t.Fatalf("acme-app not in GET /integrations: %s", w.Body.String())
	}

	put := func(body map[string]any) int {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return do(t, srv, http.MethodPut, "/api/v1/integrations/acme-app", adminToken, string(raw)).Code
	}

	// Everything the GET emits that the PUT body has no field for is
	// server-owned and must be dropped by the caller.
	settable := jsonTagNames(t, reflect.TypeOf(putIntegrationRequest{}))
	var serverOwned []string
	stripped := map[string]any{}
	for k, v := range row {
		if settable[k] {
			stripped[k] = v
			continue
		}
		serverOwned = append(serverOwned, k)
	}
	sort.Strings(serverOwned)
	if len(serverOwned) == 0 {
		t.Fatal("the GET document is already PUT-shaped; this test no longer describes the endpoint")
	}

	// 1. The raw GET document does NOT round-trip — the procedure as written.
	if code := put(row); code != http.StatusBadRequest {
		t.Errorf("PUT of the unmodified GET document: code = %d, want 400 (decodeStrict rejects server-owned fields)", code)
	}
	// 2. The documented procedure — drop exactly those fields — does.
	if code := put(stripped); code != http.StatusOK {
		t.Errorf("PUT of the GET document minus %v: code = %d, want 200", serverOwned, code)
	}
	// 3. Each one really is load-bearing, so the comment's list is exact and
	//    not merely a superset someone pasted.
	for _, k := range serverOwned {
		body := maps.Clone(stripped)
		body[k] = row[k]
		if code := put(body); code != http.StatusBadRequest {
			t.Errorf("PUT with server-owned field %q re-added: code = %d, want 400", k, code)
		}
	}
	// 4. The doc comment has to NAME every one of them, or a caller cannot
	//    perform the procedure from the comment alone — which is the defect.
	doc := putIntegrationRequestDoc(t)
	for _, k := range serverOwned {
		if !strings.Contains(doc, k) {
			t.Errorf("putIntegrationRequest's doc comment does not name server-owned field %q, which a caller MUST drop from a GET document (400 otherwise). Full comment:\n%s", k, doc)
		}
	}
}

// putIntegrationRequestDoc returns the doc comment immediately above
// `type putIntegrationRequest struct` in setup_integrations.go — the text a
// caller reads to learn what to send.
func putIntegrationRequestDoc(t *testing.T) string {
	t.Helper()
	src, err := os.ReadFile("setup_integrations.go")
	if err != nil {
		t.Fatalf("read setup_integrations.go: %v", err)
	}
	lines := strings.Split(string(src), "\n")
	decl := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "type putIntegrationRequest struct") })
	if decl < 0 {
		t.Fatal("type putIntegrationRequest struct not found in setup_integrations.go")
	}
	start := decl
	for start > 0 && strings.HasPrefix(lines[start-1], "//") {
		start--
	}
	if start == decl {
		t.Fatal("putIntegrationRequest has no doc comment")
	}
	return strings.Join(lines[start:decl], "\n")
}

// TestHandlePutIntegration_CredentialOnModelHostIsRefused pins #547's write-time
// half: an integration whose header credential would be presented on a host
// that serves a model — a vendor host, or any model provider row's own host —
// is refused; the same row on a host that serves no model is stored.
func TestHandlePutIntegration_CredentialOnModelHostIsRefused(t *testing.T) {
	srv, fake, _ := integrationWriteHarness(t, nil)
	fake.cfg.ModelProviders = providerBlock(types.ModelProvider{ID: "corp-gateway", Kind: types.ModelProviderCustomEndpoint,
		BaseURL: "https://llm.corp.example", Harnesses: []types.ProviderHarness{{Harness: "claude-code", Path: "/anthropic"}}})
	body := func(host string) string {
		return `{"kind":"git_host","egress":["` + host + `"],"secrets":[{"role":"pat","secret_name":"acme-app-id",` +
			`"delivery":{"mode":"proxy_header","header":"Authorization","format":"Bearer %s"}}]}`
	}
	for _, host := range []string{"llm.corp.example", "api.anthropic.com"} {
		w := do(t, srv, http.MethodPut, "/api/v1/integrations/acme-row", adminToken, body(host))
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "serves a model") {
			t.Errorf("credential on %s: %d %s, want 400 naming the model host", host, w.Code, w.Body.String())
		}
	}
	if len(fake.cfg.Integrations) != 0 {
		t.Fatalf("a refused write persisted %+v", fake.cfg.Integrations)
	}
	if w := do(t, srv, http.MethodPut, "/api/v1/integrations/acme-row", adminToken, body("git.corp.example")); w.Code != http.StatusOK {
		t.Fatalf("credential on a host that serves no model: %d %s, want 200", w.Code, w.Body.String())
	}
}
