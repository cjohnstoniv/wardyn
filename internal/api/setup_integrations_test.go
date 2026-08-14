// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestHandleListIntegrations_ShapeAndNoSecretValues asserts the GET
// /integrations response shape ({integrations: [{...row, source,
// capabilities}]}) and, critically, that the raw response bytes never
// contain a stored secret VALUE — only its name.
func TestHandleListIntegrations_ShapeAndNoSecretValues(t *testing.T) {
	const secretValue = "sk-ant-super-secret-value-must-never-leak"
	h := newHarness(t)
	cfg := baseTestConfig(h, &fakeSiteConfigStore{})
	cfg.Secrets = &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte(secretValue)}}
	srv := New(cfg)

	w := do(t, srv, http.MethodGet, "/api/v1/integrations", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), secretValue) {
		t.Fatalf("response body leaked a secret VALUE: %s", w.Body.String())
	}

	var got struct {
		Integrations []SetupIntegration `json:"integrations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var row *SetupIntegration
	for i := range got.Integrations {
		if got.Integrations[i].ID == "anthropic_api_key" {
			row = &got.Integrations[i]
		}
	}
	if row == nil {
		t.Fatalf("expected an anthropic_api_key entry, got %+v", got.Integrations)
	}
	if row.Source != "legacy" {
		t.Errorf("source = %q, want legacy", row.Source)
	}
	if row.RoleSecret("api_key") != "anthropic-api-key" {
		t.Errorf("secrets = %+v, want the secret NAME only", row.Secrets)
	}
	var modelAPI *Capability
	for i := range row.Capabilities {
		if row.Capabilities[i].ID == "model_api" {
			modelAPI = &row.Capabilities[i]
		}
	}
	if modelAPI == nil || modelAPI.State != CapAvailable {
		t.Errorf("model_api capability = %+v, want available (the secret is stored)", modelAPI)
	}
}

// GET /integrations enumerates provider/credential presence (capability
// disclosure), so it must live behind the same auth as /setup/status and
// /site-config: an anonymous non-local caller is rejected.
func TestHandleListIntegrations_AnonymousNonLocal401(t *testing.T) {
	h := newHarness(t) // AdminToken set, not LocalMode
	w := do(t, h.srv, http.MethodGet, "/api/v1/integrations", "", "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous non-local: code = %d, want 401", w.Code)
	}
}

// A viewer (any authenticated human, not just an operator) can read
// GET /integrations — it is registered on r, not operatorOnly, mirroring
// GET /site-config.
func TestHandleListIntegrations_NoStoredRowsIsEmptyNot404(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, &fakeSiteConfigStore{})
	srv := New(cfg)
	w := do(t, srv, http.MethodGet, "/api/v1/integrations", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (unconfigured is a valid, common state); body=%s", w.Code, w.Body.String())
	}
}

func TestSetupHarnessTools(t *testing.T) {
	tools := setupHarnessTools()
	if len(tools) != len(harnessCatalog) {
		t.Fatalf("len = %d, want %d (one per catalog row)", len(tools), len(harnessCatalog))
	}
	var claude *SetupHarnessTool
	for i := range tools {
		if tools[i].ID == "claude-code" {
			claude = &tools[i]
		}
	}
	if claude == nil {
		t.Fatal("expected a claude-code entry")
	}
	if !claude.HasGateway || !claude.HasLogin {
		t.Errorf("claude-code = %+v, want HasGateway and HasLogin both true", claude)
	}
}
