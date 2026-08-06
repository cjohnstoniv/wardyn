// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// secretSet builds a capEnv.SecretPresent callback that reports true only for
// the named secrets — the common case of "these refs are stored, nothing
// else is."
func secretSet(names ...string) func(string) bool {
	set := make(map[string]bool, len(names))
	for _, n := range names {
		set[n] = true
	}
	return func(n string) bool { return set[n] }
}

// TestReasonCanonMatchesMock hardcodes the eight distinct verbatim reason
// strings capabilitiesFor (and, via harnessProviderReason, harness.go) surface,
// copied byte-for-byte from the Integrations screen mock (scratchpad
// mockup/wardyn-integrations.js, the T object). This is the one place a silent
// edit to the canon text would be caught — TestCapabilitiesFor below
// deliberately compares against the Go symbols, not literals, so it stays
// robust to renames and exercises the STATE MACHINE, not the prose.
func TestReasonCanonMatchesMock(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"X_KEY_CODEX", reasonXKeyCodex, "Codex CLI speaks the OpenAI API only — an Anthropic key can't drive it. Not a setting."},
		{"X_SUB_CODEX", reasonXSubCodex, "Codex CLI speaks the OpenAI API only — a Claude login can't drive it. Not a setting."},
		{"X_BEDROCK_CODEX", reasonXBedrockCodex, "Codex CLI speaks the OpenAI API only — Bedrock can't drive it. Not a setting."},
		{"X_OPENAI_CLAUDE", reasonXOpenAIClaude, "Claude Code speaks the Anthropic API only — an OpenAI key can't drive it. Not a setting."},
		{"X_AZURE_HARNESS", reasonXAzureHarness, "Neither agent tool can be pointed at an Azure OpenAI deployment. Azure powers Wardyn's own features only."},
		{"X_SUB_DIRECT", reasonXSubDirect, "A subscription token is accepted only for Claude-Code-shaped requests; anything else comes back 429. That's Anthropic's gate, not a Wardyn setting."},
		{"X_AZURE_DIRECT", reasonXAzureDirect, "No sandbox lane exists — Azure is called from the control plane only."},
		{"BEDROCK_FEATURES", reasonBedrockFeatures, "Wardyn's own features reach Bedrock through the AWS credential chain — the same lane this integration uses."},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q (verbatim from the Integrations mock canon)", tc.name, tc.got, tc.want)
		}
	}
}

// TestCapabilitiesFor is the exhaustive matrix test: every ai-integration TYPE
// documented in the approved capabilitiesFor spec, in every state its
// interesting env toggles (credential present/absent/dangling, subscription
// lane, Bedrock lane + region/model unset, HostLike) can put it in, plus the
// SCM/artifact/proxy types and the cross-cutting Disabled/DisabledCaps
// overrides.
func TestCapabilitiesFor(t *testing.T) {
	tests := []struct {
		name string
		v    integrationView
		env  capEnv
		want []Capability
	}{
		// ---------------- anthropic_api_key ----------------
		{
			name: "anthropic_api_key: credential stored",
			v:    integrationView{Type: "anthropic_api_key", Credentials: map[string]string{"api_key": "anthropic-api-key"}},
			env:  capEnv{SecretPresent: secretSet("anthropic-api-key")},
			want: []Capability{
				{ID: "model_api", State: CapAvailable, Residency: "proxy_injected"},
				{ID: "tool:claude-code", State: CapAvailable, Residency: "proxy_injected"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXKeyCodex},
				{ID: "wardyn_features", State: CapAvailable, Residency: "proxy_injected"},
			},
		},
		{
			name: "anthropic_api_key: credential never configured",
			v:    integrationView{Type: "anthropic_api_key"},
			env:  capEnv{SecretPresent: secretSet()},
			want: []Capability{
				{ID: "model_api", State: CapNeedsSetup, Reason: "no credential configured"},
				{ID: "tool:claude-code", State: CapNeedsSetup, Reason: "no credential configured"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXKeyCodex},
				{ID: "wardyn_features", State: CapNeedsSetup, Reason: "no credential configured"},
			},
		},
		{
			name: "anthropic_api_key: credential ref configured but dangling",
			v:    integrationView{Type: "anthropic_api_key", Credentials: map[string]string{"api_key": "anthropic-api-key"}},
			env:  capEnv{SecretPresent: secretSet()},
			want: []Capability{
				{ID: "model_api", State: CapNeedsSetup, Reason: `secret "anthropic-api-key" not stored`},
				{ID: "tool:claude-code", State: CapNeedsSetup, Reason: `secret "anthropic-api-key" not stored`},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXKeyCodex},
				{ID: "wardyn_features", State: CapNeedsSetup, Reason: `secret "anthropic-api-key" not stored`},
			},
		},

		// ---------------- openai_api_key (mirror of anthropic_api_key) ----------------
		{
			name: "openai_api_key: credential stored",
			v:    integrationView{Type: "openai_api_key", Credentials: map[string]string{"api_key": "openai-api-key"}},
			env:  capEnv{SecretPresent: secretSet("openai-api-key")},
			want: []Capability{
				{ID: "model_api", State: CapAvailable, Residency: "proxy_injected"},
				{ID: "tool:codex-cli", State: CapAvailable, Residency: "proxy_injected"},
				{ID: "tool:claude-code", State: CapImpossible, Reason: reasonXOpenAIClaude},
				{ID: "wardyn_features", State: CapAvailable, Residency: "proxy_injected"},
			},
		},
		{
			name: "openai_api_key: credential absent",
			v:    integrationView{Type: "openai_api_key"},
			env:  capEnv{SecretPresent: secretSet()},
			want: []Capability{
				{ID: "model_api", State: CapNeedsSetup, Reason: "no credential configured"},
				{ID: "tool:codex-cli", State: CapNeedsSetup, Reason: "no credential configured"},
				{ID: "tool:claude-code", State: CapImpossible, Reason: reasonXOpenAIClaude},
				{ID: "wardyn_features", State: CapNeedsSetup, Reason: "no credential configured"},
			},
		},

		// ---------------- anthropic_subscription ----------------
		{
			name: "subscription: managed lane, blob present",
			v:    integrationView{Type: "anthropic_subscription", Config: map[string]any{"lane": "managed"}},
			env:  capEnv{ManagedBlobPresent: func(p string) bool { return p == "anthropic" }},
			want: []Capability{
				{ID: "model_api", State: CapImpossible, Reason: reasonXSubDirect},
				{ID: "tool:claude-code", State: CapAvailable, Residency: "proxy_injected"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXSubCodex},
				{ID: "wardyn_features", State: CapAvailable, Residency: "proxy_injected"},
			},
		},
		{
			name: "subscription: managed lane, no blob connected",
			v:    integrationView{Type: "anthropic_subscription", Config: map[string]any{"lane": "managed"}},
			env:  capEnv{ManagedBlobPresent: func(string) bool { return false }},
			want: []Capability{
				{ID: "model_api", State: CapImpossible, Reason: reasonXSubDirect},
				{ID: "tool:claude-code", State: CapNeedsSetup, Reason: "no managed Claude subscription connected", Residency: "proxy_injected"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXSubCodex},
				{ID: "wardyn_features", State: CapAvailable, Residency: "proxy_injected"},
			},
		},
		{
			name: "subscription: unset lane defaults to managed",
			v:    integrationView{Type: "anthropic_subscription"},
			env:  capEnv{ManagedBlobPresent: func(p string) bool { return p == "anthropic" }},
			want: []Capability{
				{ID: "model_api", State: CapImpossible, Reason: reasonXSubDirect},
				{ID: "tool:claude-code", State: CapAvailable, Residency: "proxy_injected"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXSubCodex},
				{ID: "wardyn_features", State: CapAvailable, Residency: "proxy_injected"},
			},
		},
		{
			name: "subscription: resident_host lane, session live",
			v:    integrationView{Type: "anthropic_subscription", Config: map[string]any{"lane": "resident_host"}},
			env:  capEnv{ResidentSubscriptionLive: true, HostLike: true},
			want: []Capability{
				{ID: "model_api", State: CapImpossible, Reason: reasonXSubDirect},
				{ID: "tool:claude-code", State: CapAvailable, Residency: "resident_mount"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXSubCodex},
				{ID: "wardyn_features", State: CapOff, Reason: reasonHostCLIOptIn, Residency: "resident_mount"},
			},
		},
		{
			name: "subscription: resident_host lane, host-like but no live session",
			v:    integrationView{Type: "anthropic_subscription", Config: map[string]any{"lane": "resident_host"}},
			env:  capEnv{ResidentSubscriptionLive: false, HostLike: true},
			want: []Capability{
				{ID: "model_api", State: CapImpossible, Reason: reasonXSubDirect},
				{ID: "tool:claude-code", State: CapNeedsSetup, Reason: "host-only: no live Claude CLI session found on this host", Residency: "resident_mount"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXSubCodex},
				{ID: "wardyn_features", State: CapOff, Reason: reasonHostCLIOptIn, Residency: "resident_mount"},
			},
		},
		{
			name: "subscription: resident_host lane, sealed container (not host-like)",
			v:    integrationView{Type: "anthropic_subscription", Config: map[string]any{"lane": "resident_host"}},
			env:  capEnv{ResidentSubscriptionLive: false, HostLike: false},
			want: []Capability{
				{ID: "model_api", State: CapImpossible, Reason: reasonXSubDirect},
				{ID: "tool:claude-code", State: CapNeedsSetup, Reason: "host-only: wardynd runs in a container and can only see a ~/.claude mounted into it — the managed lane avoids this", Residency: "resident_mount"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXSubCodex},
				{ID: "wardyn_features", State: CapOff, Reason: reasonHostCLIOptIn, Residency: "resident_mount"},
			},
		},

		// ---------------- bedrock ----------------
		{
			name: "bedrock: bearer lane, region+model set",
			v:    integrationView{Type: "bedrock", Config: map[string]any{"lane": "bearer"}},
			env:  capEnv{BedrockRegionSet: true, BedrockModelSet: true},
			want: []Capability{
				{ID: "model_api", State: CapAvailable, Residency: "proxy_injected"},
				{ID: "tool:claude-code", State: CapAvailable, Residency: "proxy_injected"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXBedrockCodex},
				{ID: "wardyn_features", State: CapAvailable, Residency: "proxy_injected", Reason: reasonBedrockFeatures},
			},
		},
		{
			name: "bedrock: auto lane (resident, not bearer), region+model set",
			v:    integrationView{Type: "bedrock", Config: map[string]any{"lane": "auto"}},
			env:  capEnv{BedrockRegionSet: true, BedrockModelSet: true},
			want: []Capability{
				{ID: "model_api", State: CapAvailable, Residency: "resident_env"},
				{ID: "tool:claude-code", State: CapAvailable, Residency: "resident_env"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXBedrockCodex},
				{ID: "wardyn_features", State: CapAvailable, Residency: "resident_env", Reason: reasonBedrockFeatures},
			},
		},
		{
			name: "bedrock: region unset — every cell needs setup (resolveBedrockAuth is unready)",
			v:    integrationView{Type: "bedrock", Config: map[string]any{"lane": "sso"}},
			env:  capEnv{BedrockRegionSet: false, BedrockModelSet: true},
			want: []Capability{
				{ID: "model_api", State: CapNeedsSetup, Reason: reasonBedrockUnset},
				{ID: "tool:claude-code", State: CapNeedsSetup, Reason: reasonBedrockUnset},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXBedrockCodex},
				{ID: "wardyn_features", State: CapNeedsSetup, Reason: reasonBedrockUnset},
			},
		},
		{
			name: "bedrock: model unset — same, either unset takes the whole integration out",
			v:    integrationView{Type: "bedrock", Config: map[string]any{"lane": "static"}},
			env:  capEnv{BedrockRegionSet: true, BedrockModelSet: false},
			want: []Capability{
				{ID: "model_api", State: CapNeedsSetup, Reason: reasonBedrockUnset},
				{ID: "tool:claude-code", State: CapNeedsSetup, Reason: reasonBedrockUnset},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXBedrockCodex},
				{ID: "wardyn_features", State: CapNeedsSetup, Reason: reasonBedrockUnset},
			},
		},

		// ---------------- azure_openai (no gating — a flat protocol/control-plane fact) ----------------
		{
			name: "azure_openai: unconditional regardless of credentials",
			v:    integrationView{Type: "azure_openai"},
			env:  capEnv{},
			want: []Capability{
				{ID: "model_api", State: CapImpossible, Reason: reasonXAzureDirect},
				{ID: "tool:claude-code", State: CapImpossible, Reason: reasonXAzureHarness},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXAzureHarness},
				{ID: "wardyn_features", State: CapAvailable, Residency: "control_plane"},
			},
		},

		// ---------------- scm: github_app ----------------
		{
			name: "github_app: both credentials stored",
			v: integrationView{Type: "github_app", Credentials: map[string]string{
				"app_id": "github-app-id", "app_key": "github-app-key",
			}},
			env: capEnv{SecretPresent: secretSet("github-app-id", "github-app-key")},
			want: []Capability{
				{ID: "clone:app", State: CapAvailable, Residency: "brokered"},
				{ID: "egress_host", State: CapAvailable},
			},
		},
		{
			name: "github_app: app_id missing",
			v: integrationView{Type: "github_app", Credentials: map[string]string{
				"app_key": "github-app-key",
			}},
			env: capEnv{SecretPresent: secretSet("github-app-key")},
			want: []Capability{
				{ID: "clone:app", State: CapNeedsSetup, Reason: "needs both app_id and app_key credentials"},
				{ID: "egress_host", State: CapAvailable},
			},
		},
		{
			name: "github_app: neither credential stored",
			v:    integrationView{Type: "github_app"},
			env:  capEnv{SecretPresent: secretSet()},
			want: []Capability{
				{ID: "clone:app", State: CapNeedsSetup, Reason: "needs both app_id and app_key credentials"},
				{ID: "egress_host", State: CapAvailable},
			},
		},

		// ---------------- scm: git_host ----------------
		{
			name: "git_host: pat only",
			v:    integrationView{Type: "git_host", Credentials: map[string]string{"pat": "git-pat-github-com"}},
			env:  capEnv{SecretPresent: secretSet("git-pat-github-com")},
			want: []Capability{
				{ID: "clone:pat", State: CapAvailable, Residency: "resident_env"},
				{ID: "clone:ssh", State: CapNeedsSetup, Reason: "no credential configured"},
				{ID: "egress_host", State: CapAvailable},
			},
		},
		{
			name: "git_host: ssh only",
			v:    integrationView{Type: "git_host", Credentials: map[string]string{"ssh_key": "ssh-key-github-com"}},
			env:  capEnv{SecretPresent: secretSet("ssh-key-github-com")},
			want: []Capability{
				{ID: "clone:pat", State: CapNeedsSetup, Reason: "no credential configured"},
				{ID: "clone:ssh", State: CapAvailable, Residency: "resident_env"},
				{ID: "egress_host", State: CapAvailable},
			},
		},
		{
			name: "git_host: both lanes present",
			v: integrationView{Type: "git_host", Credentials: map[string]string{
				"pat": "git-pat-github-com", "ssh_key": "ssh-key-github-com",
			}},
			env: capEnv{SecretPresent: secretSet("git-pat-github-com", "ssh-key-github-com")},
			want: []Capability{
				{ID: "clone:pat", State: CapAvailable, Residency: "resident_env"},
				{ID: "clone:ssh", State: CapAvailable, Residency: "resident_env"},
				{ID: "egress_host", State: CapAvailable},
			},
		},
		{
			name: "git_host: neither lane configured",
			v:    integrationView{Type: "git_host"},
			env:  capEnv{SecretPresent: secretSet()},
			want: []Capability{
				{ID: "clone:pat", State: CapNeedsSetup, Reason: "no credential configured"},
				{ID: "clone:ssh", State: CapNeedsSetup, Reason: "no credential configured"},
				{ID: "egress_host", State: CapAvailable},
			},
		},

		// ---------------- artifact_mirror ----------------
		{
			name: "artifact_mirror: token present, two ecosystems",
			v: integrationView{Type: "artifact_mirror",
				Credentials: map[string]string{"token": "artifactory-token"},
				Config:      map[string]any{"ecosystems": []any{"npm", "pip"}},
			},
			env: capEnv{SecretPresent: secretSet("artifactory-token")},
			want: []Capability{
				{ID: "redirect:npm", State: CapAvailable, Residency: "proxy_injected"},
				{ID: "redirect:pip", State: CapAvailable, Residency: "proxy_injected"},
			},
		},
		{
			name: "artifact_mirror: no token — degrades to config-only, never needs_setup",
			v: integrationView{Type: "artifact_mirror",
				Config: map[string]any{"ecosystems": []any{"npm"}},
			},
			env: capEnv{SecretPresent: secretSet()},
			want: []Capability{
				{ID: "redirect:npm", State: CapAvailable, Residency: "config_only", Reason: "no token configured — URL-only redirect (anonymous read)"},
			},
		},
		{
			name: "artifact_mirror: ecosystems came through a JSON round-trip ([]any)",
			v: integrationView{Type: "artifact_mirror",
				Credentials: map[string]string{"token": "artifactory-token"},
				Config:      map[string]any{"ecosystems": []any{"go"}},
			},
			env: capEnv{SecretPresent: secretSet("artifactory-token")},
			want: []Capability{
				{ID: "redirect:go", State: CapAvailable, Residency: "proxy_injected"},
			},
		},
		{
			name: "artifact_mirror: no ecosystems configured — no capabilities at all",
			v:    integrationView{Type: "artifact_mirror"},
			env:  capEnv{},
			want: nil,
		},

		// ---------------- host_proxy ----------------
		{
			name: "host_proxy: secret present",
			v:    integrationView{Type: "host_proxy", Credentials: map[string]string{"secret": "host-proxy-token"}},
			env:  capEnv{SecretPresent: secretSet("host-proxy-token")},
			want: []Capability{
				{ID: "egress_upstream", State: CapAvailable, Residency: "proxy_injected"},
			},
		},
		{
			name: "host_proxy: secret absent",
			v:    integrationView{Type: "host_proxy"},
			env:  capEnv{SecretPresent: secretSet()},
			want: []Capability{
				{ID: "egress_upstream", State: CapNeedsSetup, Reason: "no credential configured"},
			},
		},

		// ---------------- generic categories ----------------
		{
			name: "generic category: header + stored secret but NO hosts — credential still needs_setup",
			v: integrationView{Type: "acme_feed", Category: string(types.IntegrationPackageFeed),
				Header:      "x-api-key",
				Credentials: map[string]string{types.IntegrationCredentialToken: "feed-token"},
			},
			env: capEnv{SecretPresent: secretSet("feed-token")},
			want: []Capability{
				{ID: "egress_host", State: CapNeedsSetup, Reason: "No hosts named yet — nothing becomes reachable."},
				{ID: "credential", State: CapNeedsSetup, Reason: "No hosts named yet — nothing becomes reachable."},
			},
		},
		{
			name: "generic category: hosts present — credential gates on the stored secret as before",
			v: integrationView{Type: "acme_feed", Category: string(types.IntegrationPackageFeed),
				Header: "x-api-key", Hosts: []string{"feed.corp.example"},
				Credentials: map[string]string{types.IntegrationCredentialToken: "feed-token"},
			},
			env: capEnv{SecretPresent: secretSet("feed-token")},
			want: []Capability{
				{ID: "egress_host", State: CapAvailable},
				{ID: "credential", State: CapAvailable, Residency: "proxy_injected"},
			},
		},
		{
			name: "generic category routes to genericCaps even when Type collides with a typed name",
			v: integrationView{Type: "host_proxy", Category: string(types.IntegrationOtherService),
				Hosts: []string{"tool.internal"},
			},
			env: capEnv{},
			want: []Capability{
				{ID: "egress_host", State: CapAvailable},
				{ID: "credential", State: CapImpossible, Reason: reasonNoDeliveryLane},
			},
		},

		// ---------------- DisabledCaps / Disabled overrides ----------------
		{
			name: "DisabledCaps turns off one cell and leaves the rest alone",
			v: integrationView{Type: "anthropic_api_key",
				Credentials:  map[string]string{"api_key": "anthropic-api-key"},
				DisabledCaps: []string{"wardyn_features"},
			},
			env: capEnv{SecretPresent: secretSet("anthropic-api-key")},
			want: []Capability{
				{ID: "model_api", State: CapAvailable, Residency: "proxy_injected"},
				{ID: "tool:claude-code", State: CapAvailable, Residency: "proxy_injected"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXKeyCodex},
				{ID: "wardyn_features", State: CapOff, Reason: "disabled"},
			},
		},
		{
			name: "Disabled integration forces EVERY cell off, including a protocol impossibility",
			v: integrationView{Type: "anthropic_api_key",
				Credentials: map[string]string{"api_key": "anthropic-api-key"},
				Disabled:    true,
			},
			env: capEnv{SecretPresent: secretSet("anthropic-api-key")},
			want: []Capability{
				{ID: "model_api", State: CapOff, Reason: "integration disabled"},
				{ID: "tool:claude-code", State: CapOff, Reason: "integration disabled"},
				{ID: "tool:codex-cli", State: CapOff, Reason: "integration disabled"},
				{ID: "wardyn_features", State: CapOff, Reason: "integration disabled"},
			},
		},

		// ---------------- unknown type ----------------
		{
			name: "unrecognized type produces no capabilities",
			v:    integrationView{Type: "something-a-future-wave-invented"},
			env:  capEnv{},
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := capabilitiesFor(tc.v, tc.env)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("capabilitiesFor(%+v, env) =\n  %#v\nwant\n  %#v", tc.v, got, tc.want)
			}
		})
	}
}

// ─── effectiveIntegrations ───────────────────────────────────────────────────

// integrationsTestConfig builds the Config preamble for effectiveIntegrations
// tests: admin/identity/audit wiring from newHarness, a fakeSiteConfigStore
// seeded with sc, and a memSecrets seeded with secrets. Callers needing
// SubscriptionToken/ManagedToken/Bedrock knobs set them on the returned value
// before calling New.
func integrationsTestConfig(t *testing.T, sc types.SiteConfig, secrets map[string][]byte) Config {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, &fakeSiteConfigStore{cfg: sc})
	cfg.Secrets = &memSecrets{m: secrets}
	return cfg
}

func findRow(rows []integrationRow, id string) (integrationRow, bool) {
	for _, r := range rows {
		if r.ID == id {
			return r, true
		}
	}
	return integrationRow{}, false
}

func decodeRowConfig(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode row config: %v", err)
	}
	return m
}

func TestEffectiveIntegrations_AnthropicAPIKey(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{}, map[string][]byte{"anthropic-api-key": []byte("sk-ant-x")}))
	row, ok := findRow(srv.effectiveIntegrations(context.Background()), "anthropic_api_key")
	if !ok {
		t.Fatal("expected an anthropic_api_key row")
	}
	if row.Source != "legacy" || row.Category != types.IntegrationAIProvider || row.Type != "anthropic_api_key" {
		t.Errorf("row = %+v", row)
	}
	if row.Credentials["api_key"] != "anthropic-api-key" {
		t.Errorf("credentials = %+v, want api_key=anthropic-api-key", row.Credentials)
	}
}

func TestEffectiveIntegrations_OpenAIAPIKey(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{}, map[string][]byte{"openai-api-key": []byte("sk-oai-x")}))
	row, ok := findRow(srv.effectiveIntegrations(context.Background()), "openai_api_key")
	if !ok {
		t.Fatal("expected an openai_api_key row")
	}
	if row.Source != "legacy" || row.Category != types.IntegrationAIProvider || row.Type != "openai_api_key" {
		t.Errorf("row = %+v", row)
	}
	if row.Credentials["api_key"] != "openai-api-key" {
		t.Errorf("credentials = %+v, want api_key=openai-api-key", row.Credentials)
	}
}

func TestEffectiveIntegrations_ResidentSubscriptionLive(t *testing.T) {
	cfg := integrationsTestConfig(t, types.SiteConfig{}, nil)
	cfg.SubscriptionToken = fakeSubProvider{tok: subscription.Token{Value: "live-token"}}
	srv := New(cfg)
	row, ok := findRow(srv.effectiveIntegrations(context.Background()), "anthropic_subscription:resident_host")
	if !ok {
		t.Fatal("expected an anthropic_subscription:resident_host row")
	}
	if row.Category != types.IntegrationAIProvider || row.Type != "anthropic_subscription" {
		t.Errorf("row = %+v", row)
	}
	if got := decodeRowConfig(t, row.Config); got["lane"] != "resident_host" {
		t.Errorf("config = %+v, want lane=resident_host", got)
	}
}

// A wired SubscriptionToken with no LIVE token (Peek errors, or returns an
// empty value) must not synthesize a row — "wired" alone is not "live".
func TestEffectiveIntegrations_ResidentSubscriptionNotLive(t *testing.T) {
	cfg := integrationsTestConfig(t, types.SiteConfig{}, nil)
	cfg.SubscriptionToken = fakeSubProvider{tok: subscription.Token{}}
	srv := New(cfg)
	if _, ok := findRow(srv.effectiveIntegrations(context.Background()), "anthropic_subscription:resident_host"); ok {
		t.Error("expected no row when the wired provider has no live token")
	}
}

func TestEffectiveIntegrations_ManagedSubscription(t *testing.T) {
	cfg := integrationsTestConfig(t, types.SiteConfig{}, nil)
	cfg.ManagedToken = fakeSubProvider{tok: subscription.Token{Value: "sk-ant-oat01-managed"}}
	srv := New(cfg)
	row, ok := findRow(srv.effectiveIntegrations(context.Background()), "anthropic_subscription:managed")
	if !ok {
		t.Fatal("expected an anthropic_subscription:managed row")
	}
	if row.Category != types.IntegrationAIProvider || row.Type != "anthropic_subscription" {
		t.Errorf("row = %+v", row)
	}
	if got := decodeRowConfig(t, row.Config); got["lane"] != "managed" {
		t.Errorf("config = %+v, want lane=managed", got)
	}
}

func TestEffectiveIntegrations_Bedrock(t *testing.T) {
	cfg := integrationsTestConfig(t, types.SiteConfig{}, nil)
	cfg.BedrockRegion = "us-east-1"
	cfg.BedrockModel = "us.anthropic.claude-x"
	srv := New(cfg)
	row, ok := findRow(srv.effectiveIntegrations(context.Background()), "bedrock")
	if !ok {
		t.Fatal("expected a bedrock row")
	}
	if row.Category != types.IntegrationAIProvider || row.Type != "bedrock" {
		t.Errorf("row = %+v", row)
	}
	got := decodeRowConfig(t, row.Config)
	if got["lane"] != "auto" || got["region"] != "us-east-1" || got["model"] != "us.anthropic.claude-x" {
		t.Errorf("config = %+v", got)
	}
}

func TestEffectiveIntegrations_BedrockNotConfigured(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{}, nil))
	if _, ok := findRow(srv.effectiveIntegrations(context.Background()), "bedrock"); ok {
		t.Error("expected no bedrock row when nothing is configured")
	}
}

func TestEffectiveIntegrations_GitHubApp(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{}, map[string][]byte{
		secretGitHubAppID: []byte("123"), secretGitHubAppKey: []byte("key"),
	}))
	row, ok := findRow(srv.effectiveIntegrations(context.Background()), "github_app")
	if !ok {
		t.Fatal("expected a github_app row")
	}
	if row.Category != types.IntegrationSCMHost || row.Type != "github_app" {
		t.Errorf("row = %+v", row)
	}
	if row.Credentials["app_id"] != secretGitHubAppID || row.Credentials["app_key"] != secretGitHubAppKey {
		t.Errorf("credentials = %+v", row.Credentials)
	}
	if got := decodeRowConfig(t, row.Config); got["host"] != "github.com" {
		t.Errorf("config = %+v", got)
	}
}

func TestEffectiveIntegrations_GitHubApp_OnlyOneSecret(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{}, map[string][]byte{secretGitHubAppID: []byte("123")}))
	if _, ok := findRow(srv.effectiveIntegrations(context.Background()), "github_app"); ok {
		t.Error("expected no github_app row with only one of the two required secrets present")
	}
}

func TestEffectiveIntegrations_GitPatAndSSHKeySecrets(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{}, map[string][]byte{
		"git-pat-github-com":    []byte("pat"),
		"ssh-key-github-com":    []byte("key"),
		"git-pat-dev-azure-com": []byte("pat2"),
	}))
	rows := srv.effectiveIntegrations(context.Background())

	gh, ok := findRow(rows, "git_host:github.com")
	if !ok {
		t.Fatal("expected a git_host:github.com row")
	}
	if gh.Category != types.IntegrationSCMHost || gh.Type != "git_host" || gh.Name != "github.com" {
		t.Errorf("row = %+v", gh)
	}
	if gh.Credentials["pat"] != "git-pat-github-com" || gh.Credentials["ssh_key"] != "ssh-key-github-com" {
		t.Errorf("credentials = %+v, want both lanes merged onto one row", gh.Credentials)
	}

	ado, ok := findRow(rows, "git_host:dev.azure.com")
	if !ok {
		t.Fatal("expected a git_host:dev.azure.com row")
	}
	if ado.Credentials["pat"] != "git-pat-dev-azure-com" || ado.Credentials["ssh_key"] != "" {
		t.Errorf("credentials = %+v", ado.Credentials)
	}
}

func TestEffectiveIntegrations_ScmHostsWithNoCredential(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{ScmHosts: []string{"ghes.corp.example"}}, nil))
	row, ok := findRow(srv.effectiveIntegrations(context.Background()), "git_host:ghes.corp.example")
	if !ok {
		t.Fatal("expected a git_host row for a declared ScmHosts entry with no credential (egress_host only)")
	}
	if row.Credentials != nil {
		t.Errorf("credentials = %+v, want nil", row.Credentials)
	}
}

// A host that is BOTH a declared ScmHosts entry AND has a git-pat-<slug>
// secret must produce exactly ONE row (merged), not two.
func TestEffectiveIntegrations_ScmHostsMergesWithCredential(t *testing.T) {
	srv := New(integrationsTestConfig(t,
		types.SiteConfig{ScmHosts: []string{"github.com"}},
		map[string][]byte{"git-pat-github-com": []byte("pat")}))
	rows := srv.effectiveIntegrations(context.Background())
	var matches []integrationRow
	for _, r := range rows {
		if r.ID == "git_host:github.com" {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly ONE git_host:github.com row (merged), got %d: %+v", len(matches), matches)
	}
	if matches[0].Credentials["pat"] != "git-pat-github-com" {
		t.Errorf("merged row lost its credential: %+v", matches[0])
	}
}

// A HYPHENATED ScmHosts entry must merge with its own git-pat-<slug> secret
// onto ONE row: the naive reverse (hyphens back to dots) would instead spawn
// a SECOND, wrongly-named row ("ghe.prod.corp.com") holding the credential,
// leaving the correctly-named row credential-less.
func TestEffectiveIntegrations_ScmHostsHyphenatedHostMergesForward(t *testing.T) {
	srv := New(integrationsTestConfig(t,
		types.SiteConfig{ScmHosts: []string{"ghe-prod.corp.com"}},
		map[string][]byte{"git-pat-ghe-prod-corp-com": []byte("pat")}))
	rows := srv.effectiveIntegrations(context.Background())
	var matches []integrationRow
	for _, r := range rows {
		if strings.HasPrefix(r.ID, "git_host:") {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 || matches[0].ID != "git_host:ghe-prod.corp.com" {
		t.Fatalf("expected exactly ONE git_host row named git_host:ghe-prod.corp.com, got %+v", matches)
	}
	if matches[0].Credentials["pat"] != "git-pat-ghe-prod-corp-com" {
		t.Errorf("merged row lost its credential: %+v", matches[0])
	}
}

func TestEffectiveIntegrations_ArtifactMirror(t *testing.T) {
	sc := types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/api/npm/npm-remote/", TokenSecretRef: "npm-token", Ecosystem: "npm"},
		{From: "https://pypi.org/simple/", To: "https://artifactory.corp/api/pip/pip-remote/", Ecosystem: "pip"},
	}}
	srv := New(integrationsTestConfig(t, sc, nil))
	row, ok := findRow(srv.effectiveIntegrations(context.Background()), "artifact_mirror:artifactory.corp")
	if !ok {
		t.Fatal("expected one artifact_mirror row for the shared host")
	}
	if row.Category != types.IntegrationArtifactMirror || row.Type != "artifact_mirror" {
		t.Errorf("row = %+v", row)
	}
	if row.Credentials["token"] != "npm-token" {
		t.Errorf("credentials = %+v, want the npm token (first ecosystem alphabetically)", row.Credentials)
	}
	cfg := decodeRowConfig(t, row.Config)
	ecos, _ := cfg["ecosystems"].([]any)
	if len(ecos) != 2 {
		t.Errorf("ecosystems = %+v, want both npm and pip on the one shared-host row", ecos)
	}
}

// A host's FIRST-STORED redirect having no token must not permanently mark the
// derived row credential-less: the fix takes the first NON-EMPTY
// TokenSecretRef seen for the host, not just the literal first redirect.
func TestEffectiveIntegrations_ArtifactMirrorTokenFromLaterRedirect(t *testing.T) {
	sc := types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "https://pypi.org/simple/", To: "https://artifactory.corp/api/pip/pip-remote/", Ecosystem: "pip"},
		{From: "https://registry.npmjs.org/", To: "https://artifactory.corp/api/npm/npm-remote/", TokenSecretRef: "npm-token", Ecosystem: "npm"},
	}}
	srv := New(integrationsTestConfig(t, sc, nil))
	row, ok := findRow(srv.effectiveIntegrations(context.Background()), "artifact_mirror:artifactory.corp")
	if !ok {
		t.Fatal("expected one artifact_mirror row for the shared host")
	}
	if row.Credentials["token"] != "npm-token" {
		t.Errorf("credentials = %+v, want the npm token even though the FIRST-stored redirect for this host carried none", row.Credentials)
	}
}

func TestEffectiveIntegrations_HostProxy(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{UpstreamProxySecretRef: "corp-proxy-url"}, nil))
	row, ok := findRow(srv.effectiveIntegrations(context.Background()), "host_proxy")
	if !ok {
		t.Fatal("expected a host_proxy row")
	}
	if row.Category != types.IntegrationHostProxy || row.Type != "host_proxy" {
		t.Errorf("row = %+v", row)
	}
	if row.Credentials["secret"] != "corp-proxy-url" {
		t.Errorf("credentials = %+v", row.Credentials)
	}
}

// A stored integration under an id a legacy rule would also synthesize wins
// (the legacy duplicate is suppressed); a stored row under any OTHER id
// coexists alongside the unrelated legacy rows untouched.
func TestEffectiveIntegrations_StoredRowWinsAndCoexists(t *testing.T) {
	stored := types.Integration{
		ID: "anthropic_api_key", Name: "Prod Anthropic key",
		Category: types.IntegrationAIProvider, Type: "anthropic_api_key",
		Credentials: map[string]string{"api_key": "anthropic-api-key"},
	}
	sc := types.SiteConfig{Integrations: []types.Integration{stored}, UpstreamProxySecretRef: "corp-proxy-url"}
	srv := New(integrationsTestConfig(t, sc, map[string][]byte{"anthropic-api-key": []byte("sk-ant-x")}))
	rows := srv.effectiveIntegrations(context.Background())

	var anthropicRows []integrationRow
	for _, r := range rows {
		if r.ID == "anthropic_api_key" {
			anthropicRows = append(anthropicRows, r)
		}
	}
	if len(anthropicRows) != 1 {
		t.Fatalf("expected exactly ONE anthropic_api_key row (stored wins over the legacy derivation), got %d: %+v", len(anthropicRows), anthropicRows)
	}
	if anthropicRows[0].Source != "stored" || anthropicRows[0].Name != "Prod Anthropic key" {
		t.Errorf("row = %+v, want the STORED row to win", anthropicRows[0])
	}

	if _, ok := findRow(rows, "host_proxy"); !ok {
		t.Error("expected the unrelated host_proxy legacy row to coexist alongside the stored row")
	}
}

// The response must be deterministically ordered (category, then id) across
// repeated calls, regardless of map iteration order upstream.
func TestEffectiveIntegrations_DeterministicOrdering(t *testing.T) {
	sc := types.SiteConfig{
		UpstreamProxySecretRef: "corp-proxy-url",
		ScmHosts:               []string{"github.com"},
	}
	srv := New(integrationsTestConfig(t, sc, map[string][]byte{
		"openai-api-key":    []byte("x"),
		"anthropic-api-key": []byte("x"),
	}))
	ctx := context.Background()
	first := srv.effectiveIntegrations(ctx)
	if len(first) < 3 {
		t.Fatalf("expected several rows to actually exercise ordering, got %d", len(first))
	}
	for i := 0; i < 5; i++ {
		got := srv.effectiveIntegrations(ctx)
		if len(got) != len(first) {
			t.Fatalf("row count changed across calls: %d vs %d", len(got), len(first))
		}
		for j := range got {
			if got[j].ID != first[j].ID {
				t.Fatalf("run %d: order not stable at index %d: got %q, want %q", i, j, got[j].ID, first[j].ID)
			}
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i-1].Category > first[i].Category {
			t.Errorf("not sorted by category: %q before %q", first[i-1].Category, first[i].Category)
		}
	}
}
