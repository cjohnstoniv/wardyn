// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// composeIntegrationStore is fakeSiteConfigStore (site_config_test.go) plus a
// no-op ListWorkspaces: the full compose pipeline's setup-checklist stage
// (deriveSetupItems -> referencedWorkspaces) calls Store.ListWorkspaces
// whenever a Store is configured at all, which fakeSiteConfigStore's embedded
// nil store.Store does not implement.
type composeIntegrationStore struct{ fakeSiteConfigStore }

func (composeIntegrationStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return nil, nil
}

// TestComposeRun_IntegrationID_UsesNamedAPIKeySecret proves compose's new
// integration_id field (Task 2) actually steers the proposed grant: an
// explicit ai_provider integration overrides ensureLLMGrant's PROVIDER-
// DEFAULT secret ("anthropic-api-key") with the pinned integration's OWN.
func TestComposeRun_IntegrationID_UsesNamedAPIKeySecret(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Composer = singleBackendRegistry(t, &composer.FakeComposer{Result: composer.Proposal{
		Run:          composer.RunInput{Agent: "claude-code", Task: "build a small website"},
		InlinePolicy: types.RunPolicySpec{AllowedDomains: []string{"github.com"}},
		Summary:      "throwaway sandbox",
	}})
	h.srv.cfg.Store = &composeIntegrationStore{fakeSiteConfigStore{cfg: types.SiteConfig{Integrations: []types.Integration{
		apiKeyIntegration("acme-anthropic", "acme-anthropic-key"),
	}}}}
	h.srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"acme-anthropic-key": []byte("sk-acme")}}
	// clampGrants (composer/clamp.go) matches an eligible grant by KIND only
	// (any scope), so the operator ceiling just needs ONE api_key template —
	// newHarness's DefaultPolicy carries none, which would otherwise drop
	// every proposed grant regardless of which secret it names.
	h.srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{{Kind: types.GrantAPIKey}}

	body := `{"prompt":"build a small website","workspace":{"kind":"ephemeral"},"mode":"skip","integration_id":"acme-anthropic"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("compose code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp composeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	g, ok := apiKeyGrantForHost(&resp.Proposed.InlinePolicy, "api.anthropic.com")
	if !ok {
		t.Fatalf("expected an api_key grant for api.anthropic.com; policy=%+v", resp.Proposed.InlinePolicy)
	}
	var scope map[string]string
	_ = json.Unmarshal(g.Scope, &scope)
	if scope["secret_name"] != "acme-anthropic-key" {
		t.Errorf("grant secret = %q, want the pinned integration's own secret (acme-anthropic-key)", scope["secret_name"])
	}
	if resp.LLMAccess == nil || !resp.LLMAccess.Provisioned {
		t.Errorf("LLMAccess = %+v, want provisioned=true", resp.LLMAccess)
	}
}

// TestComposeRun_IntegrationID_UsesBedrockRegionOverride proves the compose
// preview folds ANY pinned non-subscription ai_provider integration through
// applyIntegrationCreds — not just the two direct-api-key types: a pinned
// bedrock integration's region/model override reaches the proposed policy
// (the region's own Bedrock hosts, not the default Anthropic api-key grant
// ensureLLMGrant would otherwise add) and the resolved WorkspaceBedrockRef is
// threaded into the proposal instead of discarded.
func TestComposeRun_IntegrationID_UsesBedrockRegionOverride(t *testing.T) {
	const region, model = "eu-central-1", "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"
	h := newHarness(t)
	h.srv.cfg.Composer = singleBackendRegistry(t, &composer.FakeComposer{Result: composer.Proposal{
		Run:          composer.RunInput{Agent: "claude-code", Task: "build a small website"},
		InlinePolicy: types.RunPolicySpec{AllowedDomains: []string{"github.com"}},
		Summary:      "throwaway sandbox",
	}})
	h.srv.cfg.Store = &composeIntegrationStore{fakeSiteConfigStore{cfg: types.SiteConfig{Integrations: []types.Integration{
		{ID: "acme-bedrock", Category: types.IntegrationAIProvider, Type: "bedrock",
			Config: mustJSON(map[string]any{"region": region, "model": model})},
	}}}}
	// The operator ceiling must bless the region's own Bedrock hosts for them
	// to survive Clamp's allow-list intersection (composer/clamp.go) — the
	// same reason TestComposeRun_IntegrationID_UsesNamedAPIKeySecret extends
	// DefaultPolicy.EligibleGrants for its own grant to survive.
	h.srv.cfg.DefaultPolicy.AllowedDomains = append(h.srv.cfg.DefaultPolicy.AllowedDomains,
		bedrockRuntimeHost(region), bedrockControlHost(region))

	body := `{"prompt":"build a small website","workspace":{"kind":"ephemeral"},"mode":"skip","integration_id":"acme-bedrock"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("compose code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp composeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, want := range []string{bedrockRuntimeHost(region), bedrockControlHost(region)} {
		if !domainAllowedExact(resp.Proposed.InlinePolicy.AllowedDomains, want) {
			t.Errorf("AllowedDomains missing %q (the pinned integration's own region); got %v",
				want, resp.Proposed.InlinePolicy.AllowedDomains)
		}
	}
	if _, ok := apiKeyGrantForHost(&resp.Proposed.InlinePolicy, "api.anthropic.com"); ok {
		t.Error("compose added a default Anthropic api_key grant despite a pinned Bedrock integration — the region/model override was discarded")
	}
	if resp.Proposed.BedrockRef == nil || resp.Proposed.BedrockRef.Region != region || resp.Proposed.BedrockRef.Model != model {
		t.Errorf("Proposed.BedrockRef = %+v, want region=%s model=%s", resp.Proposed.BedrockRef, region, model)
	}
}

// TestComposeRun_IntegrationID_NonAIProviderIs400 pins the shared rule
// (runs_create.go's decodeAndValidateCreateRun applies the identical check for
// plain create-run): naming a non-ai_provider integration is a 400, checked
// eagerly before any pipeline stage runs (the zero-value FakeComposer below is
// never even invoked).
func TestComposeRun_IntegrationID_NonAIProviderIs400(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Composer = singleBackendRegistry(t, &composer.FakeComposer{})
	h.srv.cfg.Store = &fakeSiteConfigStore{cfg: types.SiteConfig{Integrations: []types.Integration{
		{ID: "acme-scm", Category: types.IntegrationSCMHost, Type: "git_host"},
	}}}

	body := `{"prompt":"build a small website","workspace":{"kind":"ephemeral"},"mode":"skip","integration_id":"acme-scm"}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/compose", adminToken, body)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("compose code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}
