// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
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
	tools := setupHarnessTools(types.SiteConfig{}, nil)
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
	// No AgentProviders block: every row enabled, nothing claimed about its lane.
	// This is the legacy-open-mode half of the roster contract, and the reason an
	// upgraded install's New Run picker is byte-for-byte what it was.
	for _, tool := range tools {
		if !tool.Enabled || tool.Mechanism != "" || tool.CredentialSource != "" {
			t.Errorf("with no agent roster, %s = %+v; want enabled with no mechanism or source", tool.ID, tool)
		}
	}
}

// TestSetupHarnessTools_RosterCustomImageAgentAppended: OPERATIONS.md's
// "Capabilities: what one member, or one group, may do" section documents a
// WARDYN_AGENT_IMAGES id as a supported custom agent, so setupHarnessTools
// must publish roster rows as well as harnessCatalog rows — a roster entry
// naming an image-map id the catalog does not know would otherwise never
// appear in Harnesses, making a fresh pick of it impossible (only a clone of
// an existing custom-agent run would work, since that flow never consults
// this list). The invariant is catalog ∪ roster: every catalog row, PLUS any
// roster row whose id resolves through AgentImages and is not already a
// catalog id.
func TestSetupHarnessTools_RosterCustomImageAgentAppended(t *testing.T) {
	const customID = "internal-refactor-bot"
	sc := types.SiteConfig{AgentProviders: agentBlock(
		agentRow("claude-code", types.AgentMechanismAnthropicAPIKey),
		types.AgentProvider{ID: customID, Mechanism: types.AgentMechanismAnthropicAPIKey},
	)}
	images := map[string]string{customID: "registry.corp.internal/agents/refactor-bot:latest"}

	tools := setupHarnessTools(sc, images)
	if want := len(harnessCatalog) + 1; len(tools) != want {
		t.Fatalf("len = %d, want %d (catalog %d + the one roster-only image-map id)",
			len(tools), want, len(harnessCatalog))
	}
	var custom *SetupHarnessTool
	for i := range tools {
		if tools[i].ID == customID {
			custom = &tools[i]
		}
	}
	if custom == nil {
		t.Fatalf("no %q row in Harnesses; body=%+v", customID, tools)
	}
	if !custom.Enabled {
		t.Errorf("custom row = %+v, want enabled (the roster names it, undisabled)", custom)
	}
	if custom.Display != customID {
		t.Errorf("custom row Display = %q, want the id itself (no catalog display name exists)", custom.Display)
	}
	if custom.HasGateway || custom.HasLogin {
		t.Errorf("custom row = %+v, want no gateway/login — the image-map lane has neither", custom)
	}
	if !custom.NoManagedAuth {
		t.Errorf("custom row = %+v, want no_managed_auth=true", custom)
	}

	// A roster id with no AgentImages entry at all is unresolvable and stays
	// dropped — there is no image to run it with.
	toolsNoImage := setupHarnessTools(sc, nil)
	if want := len(harnessCatalog); len(toolsNoImage) != want {
		t.Fatalf("with no AgentImages entry: len = %d, want %d (the unresolvable roster row is dropped)",
			len(toolsNoImage), want)
	}
}
