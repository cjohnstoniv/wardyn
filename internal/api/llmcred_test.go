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
