// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
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
		{http.MethodPost, "/api/v1/integrations/acme-anthropic/adopt"},
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
// with `stored`, for the functional PUT/DELETE/adopt tests below.
func integrationWriteHarness(t *testing.T, stored []types.Integration) (*Server, *fakeSiteConfigStore, *recRecorder) {
	t.Helper()
	h := newHarness(t)
	fake := &fakeSiteConfigStore{cfg: types.SiteConfig{Integrations: stored}}
	cfg := baseTestConfig(h, fake)
	cfg.Secrets = &memSecrets{m: map[string][]byte{"acme-anthropic-key": []byte("sk-acme")}}
	return New(cfg), fake, h.audit
}

func TestHandlePutIntegration_CreatesStoredRow(t *testing.T) {
	srv, fake, audit := integrationWriteHarness(t, nil)
	body := `{"name":"Acme Anthropic","category":"ai_provider","type":"anthropic_api_key",` +
		`"credentials":{"api_key":"acme-anthropic-key"}}`
	w := do(t, srv, http.MethodPut, "/api/v1/integrations/acme-anthropic", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fake.cfg.Integrations) != 1 || fake.cfg.Integrations[0].ID != "acme-anthropic" {
		t.Fatalf("stored integrations = %+v, want exactly one with id acme-anthropic", fake.cfg.Integrations)
	}
	if fake.cfg.Integrations[0].CreatedAt.IsZero() || fake.cfg.Integrations[0].UpdatedAt.IsZero() {
		t.Error("expected CreatedAt/UpdatedAt to be stamped")
	}
	var got SetupIntegration
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Source != "stored" || got.ID != "acme-anthropic" {
		t.Errorf("response row = %+v, want source=stored id=acme-anthropic", got.integrationRow)
	}
	if n := auditCount(audit, "integration.write"); n != 1 {
		t.Errorf("integration.write audit events = %d, want 1", n)
	}
}

func TestHandlePutIntegration_UpdatesExistingRow_PreservesCreatedAt(t *testing.T) {
	firstCreated := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	existing := types.Integration{
		ID: "acme-anthropic", Category: types.IntegrationAIProvider, Type: "anthropic_api_key",
		Credentials: map[string]string{"api_key": "acme-anthropic-key"},
		CreatedAt:   firstCreated, UpdatedAt: firstCreated,
	}
	srv, fake, _ := integrationWriteHarness(t, []types.Integration{existing})

	body := `{"name":"Acme Anthropic (renamed)","category":"ai_provider","type":"anthropic_api_key",` +
		`"credentials":{"api_key":"acme-anthropic-key"}}`
	w := do(t, srv, http.MethodPut, "/api/v1/integrations/acme-anthropic", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fake.cfg.Integrations) != 1 {
		t.Fatalf("expected the update to REPLACE the row, not append; got %+v", fake.cfg.Integrations)
	}
	if got := fake.cfg.Integrations[0].Name; got != "Acme Anthropic (renamed)" {
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
		{"bad id shape (uppercase, from the URL)", "Bad_ID!", `{"category":"ai_provider","type":"anthropic_api_key"}`},
		{"unknown category", "acme-anthropic", `{"category":"bogus","type":"anthropic_api_key"}`},
		{"unknown type for category", "acme-anthropic", `{"category":"ai_provider","type":"bogus"}`},
		{"reserved credential secret", "acme-anthropic", `{"category":"ai_provider","type":"anthropic_api_key","credentials":{"api_key":"wardyn-signing-key"}}`},
		{"bad credential secret shape", "acme-anthropic", `{"category":"ai_provider","type":"anthropic_api_key","credentials":{"api_key":"Not Valid!"}}`},
		{"unknown default_for", "acme-anthropic", `{"category":"ai_provider","type":"anthropic_api_key","default_for":["bogus"]}`},
		{"bedrock half-set region only", "acme-bedrock", `{"category":"ai_provider","type":"bedrock","config":{"region":"us-east-1"}}`},
		{"bedrock half-set model only", "acme-bedrock", `{"category":"ai_provider","type":"bedrock","config":{"model":"anthropic.claude-3"}}`},
		{"artifact_mirror unknown ecosystem", "acme-mirror", `{"category":"artifact_mirror","type":"artifact_mirror","config":{"ecosystems":["rubygems"]}}`},
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

func TestHandlePutIntegration_BedrockBothSetIsAccepted(t *testing.T) {
	srv, fake, _ := integrationWriteHarness(t, nil)
	body := `{"category":"ai_provider","type":"bedrock","config":{"region":"us-east-1","model":"anthropic.claude-3"}}`
	w := do(t, srv, http.MethodPut, "/api/v1/integrations/acme-bedrock", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fake.cfg.Integrations) != 1 {
		t.Fatalf("expected the row to persist, got %+v", fake.cfg.Integrations)
	}
}

// TestHandlePutIntegration_DefaultForRadioSemantics pins the approved spec
// verbatim: setting a DefaultFor mark on one row CLEARS that same mark from
// every OTHER row in the SAME write — never a 409.
func TestHandlePutIntegration_DefaultForRadioSemantics(t *testing.T) {
	rowA := types.Integration{
		ID: "acme-a", Category: types.IntegrationAIProvider, Type: "anthropic_api_key",
		DefaultFor: []string{"agent_runs", "wardyn_features"},
	}
	srv, fake, _ := integrationWriteHarness(t, []types.Integration{rowA})
	body := `{"category":"ai_provider","type":"openai_api_key","default_for":["agent_runs"]}`
	w := do(t, srv, http.MethodPut, "/api/v1/integrations/acme-b", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (radio semantics must never 409); body=%s", w.Code, w.Body.String())
	}
	var a, b *types.Integration
	for i := range fake.cfg.Integrations {
		switch fake.cfg.Integrations[i].ID {
		case "acme-a":
			a = &fake.cfg.Integrations[i]
		case "acme-b":
			b = &fake.cfg.Integrations[i]
		}
	}
	if a == nil || b == nil {
		t.Fatalf("expected both rows to persist, got %+v", fake.cfg.Integrations)
	}
	if got := a.DefaultFor; len(got) != 1 || got[0] != "wardyn_features" {
		t.Errorf("row A DefaultFor = %v, want [wardyn_features] (agent_runs cleared by B's write)", got)
	}
	if got := b.DefaultFor; len(got) != 1 || got[0] != "agent_runs" {
		t.Errorf("row B DefaultFor = %v, want [agent_runs]", got)
	}
}

func TestHandleDeleteIntegration_RemovesStoredRow(t *testing.T) {
	existing := types.Integration{ID: "acme-anthropic", Category: types.IntegrationAIProvider, Type: "anthropic_api_key"}
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

// TestHandleAdoptIntegration_PersistsDerivedRowVerbatim exercises a REAL
// legacy-derived row (the anthropic-api-key secret is present in
// integrationWriteHarness), adopting it into a stored row with the SAME id,
// category, type, and credentials the derived row already carried.
func TestHandleAdoptIntegration_PersistsDerivedRowVerbatim(t *testing.T) {
	srv, fake, audit := integrationWriteHarness(t, nil)
	// Legacy derivation keys off "anthropic-api-key" (integrations.go), not the
	// "acme-anthropic-key" secret integrationWriteHarness seeds for the PUT
	// tests — seed the one the legacy path actually reads.
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte("sk-live")}}

	w := do(t, srv, http.MethodPost, "/api/v1/integrations/anthropic_api_key/adopt", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if len(fake.cfg.Integrations) != 1 {
		t.Fatalf("expected exactly one stored row after adopt, got %+v", fake.cfg.Integrations)
	}
	got := fake.cfg.Integrations[0]
	if got.ID != "anthropic_api_key" || got.Type != "anthropic_api_key" || got.Category != types.IntegrationAIProvider {
		t.Errorf("adopted row = %+v, want the derived anthropic_api_key row verbatim", got)
	}
	if got.Credentials["api_key"] != "anthropic-api-key" {
		t.Errorf("adopted credentials = %+v, want api_key -> anthropic-api-key", got.Credentials)
	}
	if got.CreatedAt.IsZero() {
		t.Error("expected the adopted row to be stamped with CreatedAt")
	}
	if n := auditCount(audit, "integration.adopt"); n != 1 {
		t.Errorf("integration.adopt audit events = %d, want 1", n)
	}

	// Adopting again now that it's stored is a 409.
	w2 := do(t, srv, http.MethodPost, "/api/v1/integrations/anthropic_api_key/adopt", adminToken, "")
	if w2.Code != http.StatusConflict {
		t.Fatalf("second adopt: code = %d, want 409; body=%s", w2.Code, w2.Body.String())
	}
}

func TestHandleAdoptIntegration_UnknownIDIs404(t *testing.T) {
	srv, _, _ := integrationWriteHarness(t, nil)
	w := do(t, srv, http.MethodPost, "/api/v1/integrations/does-not-exist/adopt", adminToken, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404; body=%s", w.Code, w.Body.String())
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
