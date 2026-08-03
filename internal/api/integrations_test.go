// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"reflect"
	"testing"
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
				Config:      map[string]any{"ecosystems": []string{"npm", "pip"}},
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
				Config: map[string]any{"ecosystems": []string{"npm"}},
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
