// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
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

// secretRow is one role→secret pair with NO declared delivery — a closed
// kind's own bespoke transport carries it (the github_app halves, git_host's
// clone credentials, an AI provider's conventional api_key).
func secretRow(role, name string) types.IntegrationSecret {
	return types.IntegrationSecret{Role: role, SecretName: name}
}

// headerSecretRow is one proxy_header-delivered secret: a generic connection's
// whole credential contract, and the only delivery an operator may declare.
func headerSecretRow(role, name, header string) types.IntegrationSecret {
	return types.IntegrationSecret{Role: role, SecretName: name,
		Delivery: &types.IntegrationDelivery{Mode: types.DeliveryProxyHeader, Header: header}}
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
		{"X_SUB_DIRECT", reasonXSubDirect, "A subscription token is accepted only for Claude-Code-shaped requests; anything else comes back 429. That's Anthropic's gate, not a Wardyn setting."},
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
		in   types.Integration
		env  capEnv
		want []Capability
	}{
		// ---------------- anthropic_api_key ----------------
		{
			name: "anthropic_api_key: credential stored",
			in:   types.Integration{Kind: "anthropic_api_key", Secrets: []types.IntegrationSecret{secretRow("api_key", "anthropic-api-key")}},
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
			in:   types.Integration{Kind: "anthropic_api_key"},
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
			in:   types.Integration{Kind: "anthropic_api_key", Secrets: []types.IntegrationSecret{secretRow("api_key", "anthropic-api-key")}},
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
			in:   types.Integration{Kind: "openai_api_key", Secrets: []types.IntegrationSecret{secretRow("api_key", "openai-api-key")}},
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
			in:   types.Integration{Kind: "openai_api_key"},
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
			in:   types.Integration{Kind: "anthropic_subscription", Config: map[string]any{"lane": "managed"}},
			env:  capEnv{ManagedBlobPresent: func(p string) bool { return p == "anthropic" }},
			want: []Capability{
				{ID: "model_api", State: CapImpossible, Reason: reasonXSubDirect},
				{ID: "tool:claude-code", State: CapAvailable, Residency: "proxy_injected"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXSubCodex},
				{ID: "wardyn_features", State: CapAvailable, Residency: "proxy_injected"},
			},
		},
		{
			// PLATFORM-API-4: with no managed token connected, wardyn_features
			// must NOT read "available" — WardynFeaturesBackend selects on
			// exactly this cell, and the sibling tool:claude-code cell right
			// above already reports the identical gap.
			name: "subscription: managed lane, no blob connected",
			in:   types.Integration{Kind: "anthropic_subscription", Config: map[string]any{"lane": "managed"}},
			env:  capEnv{ManagedBlobPresent: func(string) bool { return false }},
			want: []Capability{
				{ID: "model_api", State: CapImpossible, Reason: reasonXSubDirect},
				{ID: "tool:claude-code", State: CapNeedsSetup, Reason: "no managed Claude subscription connected", Residency: "proxy_injected"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXSubCodex},
				{ID: "wardyn_features", State: CapNeedsSetup, Reason: "no managed Claude subscription connected", Residency: "proxy_injected"},
			},
		},
		{
			name: "subscription: unset lane defaults to managed",
			in:   types.Integration{Kind: "anthropic_subscription"},
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
			in:   types.Integration{Kind: "anthropic_subscription", Config: map[string]any{"lane": "resident_host"}},
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
			in:   types.Integration{Kind: "anthropic_subscription", Config: map[string]any{"lane": "resident_host"}},
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
			in:   types.Integration{Kind: "anthropic_subscription", Config: map[string]any{"lane": "resident_host"}},
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
			name: "bedrock: bearer lane, region+model set, credential present",
			in:   types.Integration{Kind: "bedrock", Config: map[string]any{"auth_lane": "bearer"}},
			env:  capEnv{BedrockRegionSet: true, BedrockModelSet: true, BedrockCredentialPresent: true},
			want: []Capability{
				{ID: "model_api", State: CapAvailable, Residency: "proxy_injected"},
				{ID: "tool:claude-code", State: CapAvailable, Residency: "proxy_injected"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXBedrockCodex},
				{ID: "wardyn_features", State: CapAvailable, Residency: "proxy_injected", Reason: reasonBedrockFeatures},
			},
		},
		{
			name: "bedrock: auto lane (resident, not bearer), region+model set, credential present",
			in:   types.Integration{Kind: "bedrock", Config: map[string]any{"auth_lane": "auto"}},
			env:  capEnv{BedrockRegionSet: true, BedrockModelSet: true, BedrockCredentialPresent: true},
			want: []Capability{
				{ID: "model_api", State: CapAvailable, Residency: "resident_env"},
				{ID: "tool:claude-code", State: CapAvailable, Residency: "resident_env"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXBedrockCodex},
				{ID: "wardyn_features", State: CapAvailable, Residency: "resident_env", Reason: reasonBedrockFeatures},
			},
		},
		{
			// THE INTEGRATION IS READ FIRST (the wizard-completes-but-needs_setup
			// bug): resolveBedrockAuth resolves the row's own region/model over
			// the boot config, so a deployment that never set WARDYN_BEDROCK_*
			// must NOT report needs_setup for a row that carries both.
			name: "bedrock: region+model on the INTEGRATION, boot flags unset, credential present",
			in: types.Integration{Kind: "bedrock", Config: map[string]any{
				"auth_lane": "bearer", "region": "us-east-1", "model": "us.anthropic.claude-x"}},
			env: capEnv{BedrockRegionSet: false, BedrockModelSet: false, BedrockCredentialPresent: true},
			want: []Capability{
				{ID: "model_api", State: CapAvailable, Residency: "proxy_injected"},
				{ID: "tool:claude-code", State: CapAvailable, Residency: "proxy_injected"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXBedrockCodex},
				{ID: "wardyn_features", State: CapAvailable, Residency: "proxy_injected", Reason: reasonBedrockFeatures},
			},
		},
		{
			// The other half of the same precedence: the boot config is the
			// FALLBACK for whatever the row leaves unset ("a selection wins only
			// the fields it sets" — runs_bedrock.go).
			name: "bedrock: region on the integration, model from the boot fallback, credential present",
			in: types.Integration{Kind: "bedrock", Config: map[string]any{
				"auth_lane": "auto", "region": "eu-west-1"}},
			env: capEnv{BedrockRegionSet: false, BedrockModelSet: true, BedrockCredentialPresent: true},
			want: []Capability{
				{ID: "model_api", State: CapAvailable, Residency: "resident_env"},
				{ID: "tool:claude-code", State: CapAvailable, Residency: "resident_env"},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXBedrockCodex},
				{ID: "wardyn_features", State: CapAvailable, Residency: "resident_env", Reason: reasonBedrockFeatures},
			},
		},
		{
			name: "bedrock: region unset — every cell needs setup (resolveBedrockAuth is unready)",
			in:   types.Integration{Kind: "bedrock", Config: map[string]any{"auth_lane": "sso"}},
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
			in:   types.Integration{Kind: "bedrock", Config: map[string]any{"auth_lane": "static"}},
			env:  capEnv{BedrockRegionSet: true, BedrockModelSet: false},
			want: []Capability{
				{ID: "model_api", State: CapNeedsSetup, Reason: reasonBedrockUnset},
				{ID: "tool:claude-code", State: CapNeedsSetup, Reason: reasonBedrockUnset},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXBedrockCodex},
				{ID: "wardyn_features", State: CapNeedsSetup, Reason: reasonBedrockUnset},
			},
		},
		{
			// W5-S1-1 regression: region+model set but NO credential anywhere in
			// resolveBedrockAuth's ladder (bearer / SSO / mount / resident keys) —
			// the matrix must not read "available" off region+model alone. On the
			// pre-fix code this returned CapAvailable for every cell (the bug: three
			// readiness surfaces believing a run that would silently get no Bedrock
			// transport at all).
			name: "bedrock: region+model set, ZERO credentials — needs setup, not available",
			in:   types.Integration{Kind: "bedrock", Config: map[string]any{"auth_lane": "static"}},
			env:  capEnv{BedrockRegionSet: true, BedrockModelSet: true, BedrockCredentialPresent: false},
			want: []Capability{
				{ID: "model_api", State: CapNeedsSetup, Reason: reasonBedrockNoCreds},
				{ID: "tool:claude-code", State: CapNeedsSetup, Reason: reasonBedrockNoCreds},
				{ID: "tool:codex-cli", State: CapImpossible, Reason: reasonXBedrockCodex},
				{ID: "wardyn_features", State: CapNeedsSetup, Reason: reasonBedrockNoCreds},
			},
		},

		// azure_openai was the fifth AI kind. Its ONLY capability was
		// wardyn_features (the AI Run Composer, deleted) — it could never drive
		// an agent tool and had no sandbox lane at all. With the composer gone
		// it was a connectable credential wired to nothing, so the kind was
		// removed; capabilitiesFor now falls through to the generic branch for
		// it like any other unrecognized kind.

		// ---------------- scm: github_app ----------------
		{
			name: "github_app: both credentials stored",
			in:   types.Integration{Kind: "github_app", Secrets: []types.IntegrationSecret{secretRow("app_id", "github-app-id"), secretRow("app_key", "github-app-key")}},
			env:  capEnv{SecretPresent: secretSet("github-app-id", "github-app-key")},
			want: []Capability{
				{ID: "clone:app", State: CapAvailable, Residency: "brokered"},
				{ID: "egress_host", State: CapAvailable},
			},
		},
		{
			name: "github_app: app_id missing",
			in:   types.Integration{Kind: "github_app", Secrets: []types.IntegrationSecret{secretRow("app_key", "github-app-key")}},
			env:  capEnv{SecretPresent: secretSet("github-app-key")},
			want: []Capability{
				{ID: "clone:app", State: CapNeedsSetup, Reason: "needs both app_id and app_key credentials"},
				{ID: "egress_host", State: CapAvailable},
			},
		},
		{
			name: "github_app: neither credential stored",
			in:   types.Integration{Kind: "github_app"},
			env:  capEnv{SecretPresent: secretSet()},
			want: []Capability{
				{ID: "clone:app", State: CapNeedsSetup, Reason: "needs both app_id and app_key credentials"},
				{ID: "egress_host", State: CapAvailable},
			},
		},

		// ---------------- scm: git_host ----------------
		{
			name: "git_host: pat only",
			in:   types.Integration{Kind: "git_host", Secrets: []types.IntegrationSecret{secretRow("pat", "git-pat-github-com")}},
			env:  capEnv{SecretPresent: secretSet("git-pat-github-com")},
			want: []Capability{
				{ID: "clone:pat", State: CapAvailable, Residency: "resident_env"},
				{ID: "clone:ssh", State: CapNeedsSetup, Reason: "no credential configured"},
				{ID: "egress_host", State: CapAvailable},
			},
		},
		{
			name: "git_host: ssh only",
			in:   types.Integration{Kind: "git_host", Secrets: []types.IntegrationSecret{secretRow("ssh_key", "ssh-key-github-com")}},
			env:  capEnv{SecretPresent: secretSet("ssh-key-github-com")},
			want: []Capability{
				{ID: "clone:pat", State: CapNeedsSetup, Reason: "no credential configured"},
				{ID: "clone:ssh", State: CapAvailable, Residency: "resident_env"},
				{ID: "egress_host", State: CapAvailable},
			},
		},
		{
			name: "git_host: both lanes present",
			in:   types.Integration{Kind: "git_host", Secrets: []types.IntegrationSecret{secretRow("pat", "git-pat-github-com"), secretRow("ssh_key", "ssh-key-github-com")}},
			env:  capEnv{SecretPresent: secretSet("git-pat-github-com", "ssh-key-github-com")},
			want: []Capability{
				{ID: "clone:pat", State: CapAvailable, Residency: "resident_env"},
				{ID: "clone:ssh", State: CapAvailable, Residency: "resident_env"},
				{ID: "egress_host", State: CapAvailable},
			},
		},
		{
			name: "git_host: neither lane configured",
			in:   types.Integration{Kind: "git_host"},
			env:  capEnv{SecretPresent: secretSet()},
			want: []Capability{
				{ID: "clone:pat", State: CapNeedsSetup, Reason: "no credential configured"},
				{ID: "clone:ssh", State: CapNeedsSetup, Reason: "no credential configured"},
				{ID: "egress_host", State: CapAvailable},
			},
		},

		// ---------------- generic kinds ----------------
		{
			name: "generic kind: header + stored secret but NO hosts — credential still needs_setup",
			in: types.Integration{Kind: "acme_feed",
				Secrets: []types.IntegrationSecret{headerSecretRow(types.IntegrationCredentialToken, "feed-token", "x-api-key")},
			},
			env: capEnv{SecretPresent: secretSet("feed-token")},
			want: []Capability{
				{ID: "egress_host", State: CapNeedsSetup, Reason: "No hosts named yet — nothing becomes reachable."},
				{ID: "credential", State: CapNeedsSetup, Reason: "No hosts named yet — nothing becomes reachable."},
			},
		},
		{
			name: "generic kind: hosts present — credential gates on the stored secret as before",
			in: types.Integration{Kind: "acme_feed",
				Egress:  []string{"feed.corp.example"},
				Secrets: []types.IntegrationSecret{headerSecretRow(types.IntegrationCredentialToken, "feed-token", "x-api-key")},
			},
			env: capEnv{SecretPresent: secretSet("feed-token")},
			want: []Capability{
				{ID: "egress_host", State: CapAvailable},
				{ID: "credential", State: CapAvailable, Residency: "proxy_injected"},
			},
		},
		{
			// Kind alone routes: a generic slug is anything outside the closed
			// set — including "host_proxy"/"artifact_mirror" as a NEW-shape kind,
			// which no longer gets a bespoke matrix. This pins the generic route
			// for an ordinary open slug with no credential lane.
			name: "generic kind with no header: egress-only, credential honestly impossible",
			in: types.Integration{Kind: "corp_tool",
				Egress: []string{"tool.internal"},
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
			in: types.Integration{Kind: "anthropic_api_key",
				Secrets:              []types.IntegrationSecret{secretRow("api_key", "anthropic-api-key")},
				DisabledCapabilities: []string{"wardyn_features"},
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
			in: types.Integration{Kind: "anthropic_api_key",
				Secrets:  []types.IntegrationSecret{secretRow("api_key", "anthropic-api-key")},
				Disabled: true,
			},
			env: capEnv{SecretPresent: secretSet("anthropic-api-key")},
			want: []Capability{
				{ID: "model_api", State: CapOff, Reason: "integration disabled"},
				{ID: "tool:claude-code", State: CapOff, Reason: "integration disabled"},
				{ID: "tool:codex-cli", State: CapOff, Reason: "integration disabled"},
				{ID: "wardyn_features", State: CapOff, Reason: "integration disabled"},
			},
		},

		// ---------------- unknown kind ----------------
		{
			// Pre-fold, an unrecognized type in a TYPED category read as "no
			// capabilities". With the category gone there is no such state: any
			// slug outside the closed set IS a generic connection, and gets the
			// generic row's two honest cells.
			name: "unrecognized kind is a generic connection",
			in:   types.Integration{Kind: "something-a-future-wave-invented"},
			env:  capEnv{},
			want: []Capability{
				{ID: "egress_host", State: CapNeedsSetup, Reason: "No hosts named yet — nothing becomes reachable."},
				{ID: "credential", State: CapImpossible, Reason: reasonNoDeliveryLane},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := capabilitiesFor(tc.in, tc.env)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("capabilitiesFor(%+v, env) =\n  %#v\nwant\n  %#v", tc.in, got, tc.want)
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

// effectiveIntegrationsFor is effectiveIntegrations' test-call convenience:
// derives present/bedrock the same way the zero-arg production caller
// (integrationsWithCapabilities) does, so PLATFORM-API-7's threaded signature
// (present/bedrock as parameters, not recomputed internally) doesn't need
// touching at every one of this file's call sites.
func effectiveIntegrationsFor(s *Server, ctx context.Context) []integrationRow {
	present := s.presentSecretNames(ctx)
	return s.effectiveIntegrations(ctx, present, s.setupBedrock(ctx, present))
}

func findRow(rows []integrationRow, id string) (integrationRow, bool) {
	for _, r := range rows {
		if r.ID == id {
			return r, true
		}
	}
	return integrationRow{}, false
}

func TestEffectiveIntegrations_AnthropicAPIKey(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{}, map[string][]byte{"anthropic-api-key": []byte("sk-ant-x")}))
	row, ok := findRow(effectiveIntegrationsFor(srv, context.Background()), "anthropic_api_key")
	if !ok {
		t.Fatal("expected an anthropic_api_key row")
	}
	if row.Source != "legacy" || row.Kind != types.IntegrationKindAnthropicAPIKey {
		t.Errorf("row = %+v", row)
	}
	if row.RoleSecret("api_key") != "anthropic-api-key" {
		t.Errorf("secrets = %+v, want api_key=anthropic-api-key", row.Secrets)
	}
	if d := row.Secrets[0].Delivery; d == nil || d.Mode != types.DeliveryProxyHeader || d.Header != "x-api-key" {
		t.Errorf("delivery = %+v, want the anthropic proxy-header convention", row.Secrets[0].Delivery)
	}
}

func TestEffectiveIntegrations_OpenAIAPIKey(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{}, map[string][]byte{"openai-api-key": []byte("sk-oai-x")}))
	row, ok := findRow(effectiveIntegrationsFor(srv, context.Background()), "openai_api_key")
	if !ok {
		t.Fatal("expected an openai_api_key row")
	}
	if row.Source != "legacy" || row.Kind != types.IntegrationKindOpenAIAPIKey {
		t.Errorf("row = %+v", row)
	}
	if row.RoleSecret("api_key") != "openai-api-key" {
		t.Errorf("secrets = %+v, want api_key=openai-api-key", row.Secrets)
	}
}

func TestEffectiveIntegrations_ResidentSubscriptionLive(t *testing.T) {
	cfg := integrationsTestConfig(t, types.SiteConfig{}, nil)
	cfg.SubscriptionToken = fakeSubProvider{tok: subscription.Token{Value: "live-token"}}
	srv := New(cfg)
	row, ok := findRow(effectiveIntegrationsFor(srv, context.Background()), "anthropic_subscription:resident_host")
	if !ok {
		t.Fatal("expected an anthropic_subscription:resident_host row")
	}
	if row.Kind != types.IntegrationKindAnthropicSubscription {
		t.Errorf("row = %+v", row)
	}
	if got := row.Config; got["lane"] != "resident_host" {
		t.Errorf("config = %+v, want lane=resident_host", got)
	}
}

// A wired SubscriptionToken with no LIVE token (Peek errors, or returns an
// empty value) must not synthesize a row — "wired" alone is not "live".
func TestEffectiveIntegrations_ResidentSubscriptionNotLive(t *testing.T) {
	cfg := integrationsTestConfig(t, types.SiteConfig{}, nil)
	cfg.SubscriptionToken = fakeSubProvider{tok: subscription.Token{}}
	srv := New(cfg)
	if _, ok := findRow(effectiveIntegrationsFor(srv, context.Background()), "anthropic_subscription:resident_host"); ok {
		t.Error("expected no row when the wired provider has no live token")
	}
}

func TestEffectiveIntegrations_ManagedSubscription(t *testing.T) {
	cfg := integrationsTestConfig(t, types.SiteConfig{}, nil)
	cfg.ManagedToken = fakeSubProvider{tok: subscription.Token{Value: "sk-ant-oat01-managed"}}
	srv := New(cfg)
	row, ok := findRow(effectiveIntegrationsFor(srv, context.Background()), "anthropic_subscription:managed")
	if !ok {
		t.Fatal("expected an anthropic_subscription:managed row")
	}
	if row.Kind != types.IntegrationKindAnthropicSubscription {
		t.Errorf("row = %+v", row)
	}
	if got := row.Config; got["lane"] != "managed" {
		t.Errorf("config = %+v, want lane=managed", got)
	}
}

func TestEffectiveIntegrations_Bedrock(t *testing.T) {
	cfg := integrationsTestConfig(t, types.SiteConfig{}, nil)
	cfg.BedrockRegion = "us-east-1"
	cfg.BedrockModel = "us.anthropic.claude-x"
	srv := New(cfg)
	row, ok := findRow(effectiveIntegrationsFor(srv, context.Background()), "bedrock")
	if !ok {
		t.Fatal("expected a bedrock row")
	}
	if row.Kind != types.IntegrationKindBedrock {
		t.Errorf("row = %+v", row)
	}
	got := row.Config
	if got["auth_lane"] != "auto" || got["region"] != "us-east-1" || got["model"] != "us.anthropic.claude-x" {
		t.Errorf("config = %+v", got)
	}
}

func TestEffectiveIntegrations_BedrockNotConfigured(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{}, nil))
	if _, ok := findRow(effectiveIntegrationsFor(srv, context.Background()), "bedrock"); ok {
		t.Error("expected no bedrock row when nothing is configured")
	}
}

func TestEffectiveIntegrations_GitHubApp(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{}, map[string][]byte{
		secretGitHubAppID: []byte("123"), secretGitHubAppKey: []byte("key"),
	}))
	row, ok := findRow(effectiveIntegrationsFor(srv, context.Background()), "github_app")
	if !ok {
		t.Fatal("expected a github_app row")
	}
	if row.Kind != types.IntegrationKindGitHubApp {
		t.Errorf("row = %+v", row)
	}
	if row.RoleSecret("app_id") != secretGitHubAppID || row.RoleSecret("app_key") != secretGitHubAppKey {
		t.Errorf("secrets = %+v", row.Secrets)
	}
	if got := row.Config; got["host"] != "github.com" {
		t.Errorf("config = %+v", got)
	}
}

func TestEffectiveIntegrations_GitHubApp_OnlyOneSecret(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{}, map[string][]byte{secretGitHubAppID: []byte("123")}))
	if _, ok := findRow(effectiveIntegrationsFor(srv, context.Background()), "github_app"); ok {
		t.Error("expected no github_app row with only one of the two required secrets present")
	}
}

func TestEffectiveIntegrations_GitPatAndSSHKeySecrets(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{}, map[string][]byte{
		"git-pat-github-com":    []byte("pat"),
		"ssh-key-github-com":    []byte("key"),
		"git-pat-dev-azure-com": []byte("pat2"),
	}))
	rows := effectiveIntegrationsFor(srv, context.Background())

	gh, ok := findRow(rows, "git_host:github.com")
	if !ok {
		t.Fatal("expected a git_host:github.com row")
	}
	if gh.Kind != types.IntegrationKindGitHost || gh.Name != "github.com" {
		t.Errorf("row = %+v", gh)
	}
	if gh.RoleSecret("pat") != "git-pat-github-com" || gh.RoleSecret("ssh_key") != "ssh-key-github-com" {
		t.Errorf("secrets = %+v, want both lanes merged onto one row", gh.Secrets)
	}

	ado, ok := findRow(rows, "git_host:dev.azure.com")
	if !ok {
		t.Fatal("expected a git_host:dev.azure.com row")
	}
	if ado.RoleSecret("pat") != "git-pat-dev-azure-com" || ado.RoleSecret("ssh_key") != "" {
		t.Errorf("secrets = %+v", ado.Secrets)
	}
}

func TestEffectiveIntegrations_ScmHostsWithNoCredential(t *testing.T) {
	srv := New(integrationsTestConfig(t, types.SiteConfig{ScmHosts: []string{"ghes.corp.example"}}, nil))
	row, ok := findRow(effectiveIntegrationsFor(srv, context.Background()), "git_host:ghes.corp.example")
	if !ok {
		t.Fatal("expected a git_host row for a declared ScmHosts entry with no credential (egress_host only)")
	}
	if row.Secrets != nil {
		t.Errorf("secrets = %+v, want nil", row.Secrets)
	}
}

// A host that is BOTH a declared ScmHosts entry AND has a git-pat-<slug>
// secret must produce exactly ONE row (merged), not two.
func TestEffectiveIntegrations_ScmHostsMergesWithCredential(t *testing.T) {
	srv := New(integrationsTestConfig(t,
		types.SiteConfig{ScmHosts: []string{"github.com"}},
		map[string][]byte{"git-pat-github-com": []byte("pat")}))
	rows := effectiveIntegrationsFor(srv, context.Background())
	var matches []integrationRow
	for _, r := range rows {
		if r.ID == "git_host:github.com" {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly ONE git_host:github.com row (merged), got %d: %+v", len(matches), matches)
	}
	if matches[0].RoleSecret("pat") != "git-pat-github-com" {
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
	rows := effectiveIntegrationsFor(srv, context.Background())
	var matches []integrationRow
	for _, r := range rows {
		if strings.HasPrefix(r.ID, "git_host:") {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 || matches[0].ID != "git_host:ghe-prod.corp.com" {
		t.Fatalf("expected exactly ONE git_host row named git_host:ghe-prod.corp.com, got %+v", matches)
	}
	if matches[0].RoleSecret("pat") != "git-pat-ghe-prod-corp-com" {
		t.Errorf("merged row lost its credential: %+v", matches[0])
	}
}

// A stored integration under an id a legacy rule would also synthesize wins
// (the legacy duplicate is suppressed); a stored row under any OTHER id
// coexists alongside the unrelated legacy rows untouched.
func TestEffectiveIntegrations_StoredRowWinsAndCoexists(t *testing.T) {
	stored := types.Integration{
		ID: "anthropic_api_key", Name: "Prod Anthropic key",
		Kind: types.IntegrationKindAnthropicAPIKey,
		Secrets: []types.IntegrationSecret{{Role: "api_key", SecretName: "anthropic-api-key",
			Delivery: types.AIKeyDelivery(types.IntegrationKindAnthropicAPIKey)}},
	}
	sc := types.SiteConfig{Integrations: []types.Integration{stored}, ScmHosts: []string{"ghes.corp.example"}}
	srv := New(integrationsTestConfig(t, sc, map[string][]byte{"anthropic-api-key": []byte("sk-ant-x")}))
	rows := effectiveIntegrationsFor(srv, context.Background())

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

	if _, ok := findRow(rows, "git_host:ghes.corp.example"); !ok {
		t.Error("expected the unrelated git_host legacy row to coexist alongside the stored row")
	}
}

// The response must be deterministically ordered (derived group from kind,
// then id) across repeated calls, regardless of map iteration order upstream.
func TestEffectiveIntegrations_DeterministicOrdering(t *testing.T) {
	sc := types.SiteConfig{
		Integrations: []types.Integration{{ID: "acme-feed", Kind: "artifactory", Egress: []string{"feed.corp.example"}}},
		ScmHosts:     []string{"github.com"},
	}
	srv := New(integrationsTestConfig(t, sc, map[string][]byte{
		"openai-api-key":    []byte("x"),
		"anthropic-api-key": []byte("x"),
	}))
	ctx := context.Background()
	first := effectiveIntegrationsFor(srv, ctx)
	if len(first) < 3 {
		t.Fatalf("expected several rows to actually exercise ordering, got %d", len(first))
	}
	for i := 0; i < 5; i++ {
		got := effectiveIntegrationsFor(srv, ctx)
		if len(got) != len(first) {
			t.Fatalf("row count changed across calls: %d vs %d", len(got), len(first))
		}
		for j := range got {
			if got[j].ID != first[j].ID {
				t.Fatalf("run %d: order not stable at index %d: got %q, want %q", i, j, got[j].ID, first[j].ID)
			}
		}
	}
	// AI providers, then source control, then the flat Connections set — the
	// three groups the screen renders, derived from kind alone.
	for i := 1; i < len(first); i++ {
		if integrationGroup(first[i-1].Kind) > integrationGroup(first[i].Kind) {
			t.Errorf("not sorted by derived group: %q before %q", first[i-1].Kind, first[i].Kind)
		}
	}
	if integrationGroup(first[len(first)-1].Kind) != 2 {
		t.Errorf("the generic connection must sort last, got %q", first[len(first)-1].Kind)
	}
}
