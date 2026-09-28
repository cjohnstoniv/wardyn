// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestReconcileLLMAccess_SubHintSurvivesGateway: the "launch this proposal
// from the wizard with your Claude subscription mounted" hint must still
// appear for an Anthropic no-model-access verdict once a gateway is
// configured — it is keyed on the PROVIDER (p.secret ==
// "anthropic-api-key"), not on p.host, which under a gateway is the
// gateway's host, not "api.anthropic.com".
func TestReconcileLLMAccess_SubHintSurvivesGateway(t *testing.T) {
	const wantHint = "launch this proposal from the wizard with your Claude subscription mounted"

	// Baseline: no gateway configured, no secret present -> the hint appears.
	s := &Server{}
	spec := &types.RunPolicySpec{}
	note, provisioned := s.reconcileLLMAccess(spec, "claude-code", map[string]bool{}, false, false)
	if provisioned {
		t.Fatalf("expected no model access, got provisioned")
	}
	if !strings.Contains(note, wantHint) {
		t.Fatalf("expected the subscription hint, got %q", note)
	}

	// Gateway configured: the hint must still appear (this is what the fix
	// covers — keying on p.host == "api.anthropic.com" would silently drop it).
	gw := &Server{cfg: Config{LLMGateways: map[string]string{"api.anthropic.com": "https://llm-gateway.corp.internal"}}}
	spec2 := &types.RunPolicySpec{}
	note2, provisioned2 := gw.reconcileLLMAccess(spec2, "claude-code", map[string]bool{}, false, false)
	if provisioned2 {
		t.Fatalf("expected no model access, got provisioned")
	}
	if !strings.Contains(note2, wantHint) {
		t.Fatalf("gateway must not suppress the subscription hint, got %q", note2)
	}

	// Negative control: OpenAI never carries the Claude-only hint, gateway or not.
	noteOpenAI, _ := s.reconcileLLMAccess(&types.RunPolicySpec{}, "codex-cli", map[string]bool{}, false, false)
	if strings.Contains(noteOpenAI, wantHint) {
		t.Fatalf("OpenAI's verdict must never carry the Claude subscription hint, got %q", noteOpenAI)
	}
}

// TestApplyLLMCredMount_GatewayEgressPrecondition pins the egress precondition
// applyLLMCredMount/anthropicReachable enforce: a ceiling whose egress lists
// ONLY a configured gateway host (not api.anthropic.com/*.anthropic.com) must
// still bless the subscription mount, because that gateway — not the vendor
// host — is where dispatch will actually point ANTHROPIC_BASE_URL
// (runs_dispatch_llm.go). Before this fix the check named api.anthropic.com
// only, so a ceiling correctly scoped to the gateway would silently refuse to
// mount the credential and the run fell back to a broken api-key path.
func TestApplyLLMCredMount_GatewayEgressPrecondition(t *testing.T) {
	ceiling := types.RunPolicySpec{
		AllowedDomains: []string{"llm-gateway.corp.internal"},
		WorkspaceMounts: []types.WorkspaceMount{
			{Source: "/host/.claude", Target: claudeCredTarget},
			{Source: "/host/.claude.json", Target: claudeCredJSONTarget},
		},
	}
	spec := &types.RunPolicySpec{AllowedDomains: []string{"llm-gateway.corp.internal"}}

	// No gateway threaded through: the check only knows api.anthropic.com, so
	// this ceiling's egress does not satisfy it and the mount is refused.
	if injected, warns := applyLLMCredMount(spec, ceiling, "claude-code", true, ""); injected {
		t.Fatalf("expected refusal with no gatewayHost threaded, got injected=true warns=%v", warns)
	} else if len(warns) == 0 || !strings.Contains(warns[0], "api.anthropic.com") {
		t.Fatalf("expected a refusal naming api.anthropic.com, got %v", warns)
	}

	// Gateway threaded through: the same ceiling now satisfies the precondition.
	spec2 := &types.RunPolicySpec{AllowedDomains: []string{"llm-gateway.corp.internal"}}
	injected, warns := applyLLMCredMount(spec2, ceiling, "claude-code", true, "llm-gateway.corp.internal:443")
	if !injected {
		t.Fatalf("expected the gateway-reachable ceiling to bless the mount, got warns=%v", warns)
	}
	if !specHasMountTarget(spec2, claudeCredTarget) {
		t.Fatal("expected the Claude credential mount to be injected")
	}

	// A gateway is configured but this run's OWN spec does not reach it: the
	// refusal text must name the gateway too, not just api.anthropic.com.
	if injected, warns := applyLLMCredMount(&types.RunPolicySpec{}, ceiling, "claude-code", true, "llm-gateway.corp.internal:443"); injected {
		t.Fatalf("expected refusal, got injected=true warns=%v", warns)
	} else if len(warns) == 0 || !strings.Contains(warns[0], "llm-gateway.corp.internal") {
		t.Fatalf("expected the refusal to name the configured gateway, got %v", warns)
	}
}

// TestAnthropicReachable_GatewayGoverns pins issue #508 F4: once a gateway is
// configured, subscription mode dials the GATEWAY (runs_dispatch_llm.go sets
// ANTHROPIC_BASE_URL to it), so a vendor-host entry proves nothing — passing
// on *.anthropic.com mounted the resident credential into a run whose one model
// dial the proxy refuses. The gateway's reachability is judged exactly as the
// proxy will judge the CONNECT.
func TestAnthropicReachable_GatewayGoverns(t *testing.T) {
	const gw = "llm-gateway.corp.internal:443"
	tests := []struct {
		name    string
		spec    types.RunPolicySpec
		gateway string
		want    bool
	}{
		{"no gateway: exact vendor host", types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}, "", true},
		{"no gateway: vendor wildcard", types.RunPolicySpec{AllowedDomains: []string{"*.anthropic.com"}}, "", true},
		{"no gateway: allow-all", types.RunPolicySpec{AllowAllEgress: true}, "", true},
		{"no gateway: nothing listed", types.RunPolicySpec{}, "", false},
		{"gateway: only *.anthropic.com listed", types.RunPolicySpec{AllowedDomains: []string{"*.anthropic.com"}}, gw, false},
		{"gateway: only api.anthropic.com listed", types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}}, gw, false},
		{"gateway: exact entry", types.RunPolicySpec{AllowedDomains: []string{"llm-gateway.corp.internal"}}, gw, true},
		{"gateway: exact entry, upper case", types.RunPolicySpec{AllowedDomains: []string{"LLM-Gateway.corp.internal"}}, gw, true},
		{"gateway: covering wildcard", types.RunPolicySpec{AllowedDomains: []string{"*.corp.internal"}}, gw, true},
		{"gateway: entry on its port", types.RunPolicySpec{AllowedDomains: []string{"llm-gateway.corp.internal:443"}}, gw, true},
		{"gateway: entry on another port", types.RunPolicySpec{AllowedDomains: []string{"llm-gateway.corp.internal:8443"}}, gw, false},
		{"gateway: a sibling host", types.RunPolicySpec{AllowedDomains: []string{"other.corp.internal"}}, gw, false},
		{"gateway: allow-all", types.RunPolicySpec{AllowAllEgress: true}, gw, true},
		{"gateway: allow-all but the gateway denied", types.RunPolicySpec{AllowAllEgress: true,
			DeniedDomains: []string{"llm-gateway.corp.internal"}}, gw, false},
		{"gateway: listed but denied by wildcard", types.RunPolicySpec{AllowedDomains: []string{"llm-gateway.corp.internal"},
			DeniedDomains: []string{"*.corp.internal"}}, gw, false},
		{"gateway: malformed host:port", types.RunPolicySpec{AllowAllEgress: true}, "llm-gateway.corp.internal", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.spec
			if got := anthropicReachable(&spec, tc.gateway); got != tc.want {
				t.Fatalf("anthropicReachable(%+v, %q) = %v, want %v", tc.spec, tc.gateway, got, tc.want)
			}
		})
	}

	// End to end through the mount gate: a vendor-only ceiling with a gateway
	// configured must NOT mount the resident credential, and must say why.
	ceiling := types.RunPolicySpec{
		AllowedDomains:  []string{"*.anthropic.com"},
		WorkspaceMounts: []types.WorkspaceMount{{Source: "/host/.claude", Target: claudeCredTarget}},
	}
	spec := &types.RunPolicySpec{AllowedDomains: []string{"*.anthropic.com"}}
	injected, warns := applyLLMCredMount(spec, ceiling, "claude-code", true, gw)
	if injected || specHasMountTarget(spec, claudeCredTarget) {
		t.Fatalf("resident credential mounted into a run that cannot reach the gateway (warns=%v)", warns)
	}
	if len(warns) == 0 || !strings.Contains(warns[0], gw) {
		t.Fatalf("refusal must name the gateway, got %v", warns)
	}
}
