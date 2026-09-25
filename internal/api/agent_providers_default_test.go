// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func defaultRow(agent, provider string) types.AgentProvider {
	return types.AgentProvider{ID: agent, Mechanism: types.AgentMechanismAnthropicAPIKey, DefaultProvider: provider}
}

// TestValidateDefaultProviders is the cross-check table: a default names a
// provider enabled for its agent, and a turned-off one still counts.
func TestValidateDefaultProviders(t *testing.T) {
	off := keyProvider("off", "claude-code")
	off.Disabled = true
	providers := providerBlock(keyProvider("anthropic", "claude-code"), keyProvider("unused"), off)
	for _, tc := range []struct {
		name      string
		roster    *types.AgentProviders
		providers *types.ModelProviders
		want      string
	}{
		{name: "no roster", roster: nil, providers: providers},
		{name: "a row with no default is today", roster: agentBlock(agentRow("claude-code", types.AgentMechanismBedrockSSO))},
		{name: "a default enabled for its agent", roster: agentBlock(defaultRow("claude-code", "anthropic")), providers: providers},
		{name: "a turned-off provider may stay the default", roster: agentBlock(defaultRow("claude-code", "off")), providers: providers},
		{name: "a default naming no provider", roster: agentBlock(defaultRow("claude-code", "ghost")), providers: providers,
			want: "names no model provider"},
		{name: "a default with no provider block at all", roster: agentBlock(defaultRow("claude-code", "anthropic")),
			want: "names no model provider"},
		{name: "a default not enabled for its agent", roster: agentBlock(defaultRow("claude-code", "unused")), providers: providers,
			want: "is not enabled for"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDefaultProviders(tc.roster, tc.providers)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestAgentProvidersPutChecksDefaults: the roster door checks defaults against
// the STORED providers, and audits them as agent:provider pairs.
func TestAgentProvidersPutChecksDefaults(t *testing.T) {
	fake := &fakeSiteConfigStore{cfg: types.SiteConfig{ModelProviders: providerBlock(keyProvider("anthropic", "claude-code"))}}
	srv, audit := newAgentProvidersHarness(t, fake)

	w := do(t, srv, http.MethodPut, "/api/v1/agent-providers", adminToken,
		`{"agents":[{"id":"claude-code","mechanism":"anthropic_api_key","default_provider":"ghost"}]}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "names no model provider") {
		t.Fatalf("PUT = %d %s, want 400 naming the missing provider", w.Code, w.Body.String())
	}

	w = do(t, srv, http.MethodPut, "/api/v1/agent-providers", adminToken,
		`{"agents":[{"id":"claude-code","mechanism":"anthropic_api_key","default_provider":"anthropic"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
	}
	if got := fake.putSeen.AgentProviders.Agents[0].DefaultProvider; got != "anthropic" {
		t.Errorf("default_provider stored as %q", got)
	}
	d := agentProviderWriteDatum(t, audit)
	if got, _ := json.Marshal(d["defaults"]); string(got) != `["claude-code:anthropic"]` {
		t.Errorf("defaults = %s, want the one agent:provider pair", got)
	}
}

// TestAgentProviderAuditWithoutDefaultsIsToday is the golden: a roster naming no
// default writes exactly the datum keys it wrote before the field existed.
func TestAgentProviderAuditWithoutDefaultsIsToday(t *testing.T) {
	srv, audit := newAgentProvidersHarness(t, &fakeSiteConfigStore{})
	if w := do(t, srv, http.MethodPut, "/api/v1/agent-providers", adminToken,
		`{"agents":[{"id":"claude-code","mechanism":"anthropic_api_key"}]}`); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
	}
	want := []string{"agent_count", "credential_sources", "disabled", "ids", "mechanisms", "pins"}
	if got := slices.Sorted(maps.Keys(agentProviderWriteDatum(t, audit))); !slices.Equal(got, want) {
		t.Errorf("agent_provider.write keys = %v, want exactly today's %v", got, want)
	}
}

// TestSiteConfigDoorChecksDefaults: the MDM door checks the pair it will store,
// after the carry-forward — so removing a default's provider there is refused
// too, whichever block the body named.
func TestSiteConfigDoorChecksDefaults(t *testing.T) {
	stored := types.SiteConfig{
		ModelProviders: providerBlock(keyProvider("anthropic", "claude-code")),
		AgentProviders: agentBlock(defaultRow("claude-code", "anthropic")),
	}
	for _, tc := range []struct{ name, body string }{
		{"clearing the providers a default names", `{"model_providers":{}}`},
		{"unticking the default's agent", `{"model_providers":{"providers":[{"id":"anthropic","kind":"anthropic_api_key"}]}}`},
		{"a roster naming a provider the carried-forward block lacks",
			`{"agent_providers":{"agents":[{"id":"claude-code","mechanism":"anthropic_api_key","default_provider":"ghost"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newAgentProvidersHarness(t, &fakeSiteConfigStore{cfg: stored})
			if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, tc.body); w.Code != http.StatusBadRequest {
				t.Fatalf("PUT = %d, want 400; body=%s", w.Code, w.Body.String())
			}
		})
	}
	t.Run("turning the default's provider off is the incident switch, not a refusal", func(t *testing.T) {
		srv, _ := newAgentProvidersHarness(t, &fakeSiteConfigStore{cfg: stored})
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken,
			`{"model_providers":{"providers":[{"id":"anthropic","kind":"anthropic_api_key","disabled":true,"harnesses":[{"harness":"claude-code"}]}]}}`)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
		}
	})
}

func agentProviderWriteDatum(t *testing.T, audit *recRecorder) map[string]any {
	t.Helper()
	var d map[string]any
	for _, ev := range audit.events {
		if ev.Action == "agent_provider.write" {
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				t.Fatal(err)
			}
		}
	}
	if d == nil {
		t.Fatal("no agent_provider.write event")
	}
	return d
}
