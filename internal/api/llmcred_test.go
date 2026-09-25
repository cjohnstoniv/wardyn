// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"reflect"
	"slices"
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

// TestEnsureLLMGrant_GrantEgressCoupling pins every input class the composed-run
// author handles, and above all the SPINE-4 coupling: an api_key grant and its
// EXACT-host allowlist entry are one unit (addAPIKeyGrant), and the entry is
// added even under allow-all. buildInjector's AllowedExactHost deliberately does
// not honor allow-all, so dropping the entry there fails the proxy CLOSED at
// startup and the sandbox gets zero egress. The subscription lane, by contrast,
// proposes egress ONLY, and adds nothing under allow-all.
func TestEnsureLLMGrant_GrantEgressCoupling(t *testing.T) {
	const host = "api.anthropic.com"
	present := map[string]bool{"anthropic-api-key": true}
	// A grant another author already proposed for the same host, with a DIFFERENT
	// secret: it must win untouched (never double-grant a host).
	existing := types.GrantSpec{Kind: types.GrantAPIKey, Scope: mustJSON(map[string]string{
		"host": host, "header": "x-api-key", "format": "%s", "secret_name": "someone-elses",
	})}

	tests := []struct {
		name        string
		spec        types.RunPolicySpec
		secrets     map[string]bool
		subscribed  bool
		wantGrants  int
		wantDomains []string
	}{
		{"subscription proposes both egress entries and no grant", types.RunPolicySpec{}, present, true, 0, []string{"*.anthropic.com", host}},
		{"subscription adds nothing under allow-all", types.RunPolicySpec{AllowAllEgress: true}, present, true, 0, nil},
		{"subscription does not duplicate", types.RunPolicySpec{AllowedDomains: []string{"*.anthropic.com", host}}, present, true, 0, []string{"*.anthropic.com", host}},
		{"api key couples the grant to its exact host", types.RunPolicySpec{}, present, false, 1, []string{host}},
		{"api key couples the exact host EVEN under allow-all", types.RunPolicySpec{AllowAllEgress: true}, present, false, 1, []string{host}},
		{"an unstored secret grants nothing", types.RunPolicySpec{}, map[string]bool{}, false, 0, nil},
		{"an existing grant for the host wins", types.RunPolicySpec{EligibleGrants: []types.GrantSpec{existing}}, present, false, 1, nil},
		{"the exact host is not duplicated", types.RunPolicySpec{AllowedDomains: []string{host}}, present, false, 1, []string{host}},
	}
	s := &Server{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := tc.spec
			s.ensureLLMGrant(&spec, "claude-code", tc.secrets, tc.subscribed)
			if len(spec.EligibleGrants) != tc.wantGrants {
				t.Fatalf("grants = %d, want %d (%+v)", len(spec.EligibleGrants), tc.wantGrants, spec.EligibleGrants)
			}
			if !slices.Equal(spec.AllowedDomains, tc.wantDomains) {
				t.Fatalf("domains = %v, want %v", spec.AllowedDomains, tc.wantDomains)
			}
		})
	}

	// The grant the api-key lane authors, field by field: the scope the proxy's
	// injector resolves, and the 1h TTL the broker/clamp ceiling mirrors.
	spec := &types.RunPolicySpec{}
	s.ensureLLMGrant(spec, "claude-code", present, false)
	g, ok := apiKeyGrantForHost(spec, host)
	if !ok {
		t.Fatal("no api_key grant for the provider host")
	}
	if g.TTLSeconds != 3600 || g.RequiresApproval {
		t.Fatalf("grant = {TTL %d, RequiresApproval %v}, want {3600, false}", g.TTLSeconds, g.RequiresApproval)
	}
	if want := string(mustJSON(map[string]string{
		"host": host, "header": "x-api-key", "format": "%s", "secret_name": "anthropic-api-key",
	})); string(g.Scope) != want {
		t.Fatalf("scope = %s, want %s", g.Scope, want)
	}
}

// TestAPIKeyGrant_BothAuthorsEmitTheSameUnit pins that the two authors of an
// api_key grant — the composed-run lane (ensureLLMGrant) and the integration
// uniform fold (applyIntegrationCreds' default arm) — emit the IDENTICAL grant
// and the identical coupled egress entry for the same provider host, because
// they share one addAPIKeyGrant. They were byte-near copies that had already
// been re-derived once; re-inlining either is how they drift apart again.
func TestAPIKeyGrant_BothAuthorsEmitTheSameUnit(t *testing.T) {
	integ := apiKeyIntegration("acme-anthropic", "anthropic-api-key")
	s := integrationTestServer(t, []types.Integration{integ}, "anthropic-api-key")

	// allow-all on BOTH: it isolates the coupled exact-host entry (the row's own
	// egress is skipped under allow-all) and pins that neither author excuses it.
	viaIntegration := &types.RunPolicySpec{AllowAllEgress: true}
	if kind, ref := s.applyIntegrationCreds(context.Background(), "", viaIntegration, integ, "claude-code"); kind != types.IntegrationKindAnthropicAPIKey || ref != nil {
		t.Fatalf("applyIntegrationCreds = (%q, %v), want (%q, nil)", kind, ref, types.IntegrationKindAnthropicAPIKey)
	}
	viaCompose := &types.RunPolicySpec{AllowAllEgress: true}
	s.ensureLLMGrant(viaCompose, "claude-code", map[string]bool{"anthropic-api-key": true}, false)

	if !reflect.DeepEqual(viaIntegration.EligibleGrants, viaCompose.EligibleGrants) {
		t.Fatalf("grants differ:\n integration %+v\n compose     %+v", viaIntegration.EligibleGrants, viaCompose.EligibleGrants)
	}
	if !slices.Equal(viaIntegration.AllowedDomains, viaCompose.AllowedDomains) {
		t.Fatalf("domains differ: integration %v vs compose %v", viaIntegration.AllowedDomains, viaCompose.AllowedDomains)
	}
	if !slices.Contains(viaCompose.AllowedDomains, "api.anthropic.com") {
		t.Fatalf("the coupled exact-host entry was dropped under allow-all (SPINE-4): %v", viaCompose.AllowedDomains)
	}
}
