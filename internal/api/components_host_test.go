// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func mustDestination(t *testing.T, entry string) types.Destination {
	t.Helper()
	d, err := types.ParseDestination(entry)
	if err != nil {
		t.Fatalf("ParseDestination(%q): %v", entry, err)
	}
	return d
}

// TestDestinationOverlaps is the gate's two host vetoes over every spelling a
// destination can take: both compare parsed destinations with
// OverlapsAtAnyPort, so case, a trailing dot, an explicit or default port and a
// wildcard in either direction all answer the same.
func TestDestinationOverlaps(t *testing.T) {
	site := types.SiteConfig{
		ModelProviders: &types.ModelProviders{Providers: []types.ModelProvider{
			{ID: "gw", Kind: types.ModelProviderCustomEndpoint, BaseURL: "https://llm.corp.example:8443/v1"},
			{ID: "br", Kind: types.ModelProviderBedrockBearer, Bedrock: &types.BedrockSettings{Region: "eu-west-1"}},
		}},
		EgressRedirects: []types.EgressRedirect{
			{To: "https://mirror.corp.example:8443/npm/"},
			{Ecosystem: "pip", To: "https://pypi.corp.example/simple/"},
			{From: "https://public.vendor.example/api", To: "relay.corp.example"},
		},
	}
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{
		{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"Api.Vendor.Example","secret_name":"k"}`)},
		{Kind: types.GrantEnvSecret, Scope: json.RawMessage(`{"name":"X","secret_name":"k"}`)},
	}}
	srv := &Server{}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(operatorCtx(capSub, capEmail, oidc.RoleUser))
	b := srv.newComponentHostBounds(r, site, spec, governanceCeiling{}, nil)

	for entry, want := range map[string]bool{
		// The vendors' hosts.
		"api.openai.com": true, "api.openai.com:443": true, "api.openai.com:8443": true, "API.OpenAI.com.": true,
		"*.openai.com": true, "*.com": true, "openai.com": false, "chat.openai.com": false, "*.api.openai.com": false,
		"api.anthropic.com": true, "anthropic.com": true, "ANTHROPIC.com:9": true, "*.anthropic.com": true, "*.console.anthropic.com": true,
		// A provider row's own address, on a port it does not use, and under a
		// wildcard that covers it.
		"llm.corp.example": true, "llm.corp.example:443": true, "LLM.Corp.Example.": true, "*.corp.example": true, "*.example": true,
		"corp.example": false, "other.corp.example": false,
		// A Bedrock row's runtime and control hosts for its region.
		"bedrock-runtime.eu-west-1.amazonaws.com": true, "bedrock.eu-west-1.amazonaws.com:8443": true, "*.eu-west-1.amazonaws.com": true,
		"bedrock-runtime.us-east-1.amazonaws.com": false, "s3.eu-west-1.amazonaws.com": false,
		// Not a model's.
		"svc.example": false, "*.svc.example": false, "10.0.0.1": false,
	} {
		if got := b.servesModel(mustDestination(t, entry)); got != want {
			t.Errorf("servesModel(%q) = %v, want %v", entry, got, want)
		}
	}
	// The predicate the rest of the deployment asks is never looser than the
	// gate: a single host it calls a model's, the gate refuses.
	serving := srv.modelServingHosts(site)
	for _, h := range []string{"api.openai.com", "api.anthropic.com", "evilanthropic.com", "llm.corp.example", "bedrock.eu-west-1.amazonaws.com"} {
		if !serving(h) || !b.servesModel(mustDestination(t, h)) {
			t.Errorf("%q: modelServingHosts = %v, servesModel = %v, want both true", h, serving(h), b.servesModel(mustDestination(t, h)))
		}
	}

	for entry, want := range map[string]bool{
		// A grant already on the run, whatever the spelling or port.
		"api.vendor.example": true, "API.Vendor.Example.": true, "api.vendor.example:8443": true, "*.vendor.example": true,
		"vendor.example": false, "other.vendor.example": false,
		// A corporate redirect's target, which the redirect reaches on :8443.
		"mirror.corp.example": true, "mirror.corp.example:443": true,
		// The public hosts a redirect stands in for: an ecosystem's registry
		// table, a network-only row's From host.
		"pypi.org": true, "files.pythonhosted.org:8443": true, "pypi.corp.example": true, "*.pythonhosted.org": true,
		"public.vendor.example": true, "PUBLIC.vendor.example.": true, "relay.corp.example": true,
		"registry.npmjs.org": false, // no npm redirect is configured here
		"svc.example":        false,
	} {
		if got := b.collides(mustDestination(t, entry)); got != want {
			t.Errorf("collides(%q) = %v, want %v", entry, got, want)
		}
	}
}

// The per-person Azure DevOps lane's hosts carry that person's credential, so
// a component may not put a second one on any of them — for the person the
// lane resolves for, and only for them.
func TestComponentHostBounds_AzureDevOpsLane(t *testing.T) {
	site := adoSite(adoEntraTestRow())
	spec := types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: adoTestRepo}}}
	srv := &Server{}
	as := func(sub string) componentHostBounds {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(operatorCtx(sub, sub+"@corp.example", oidc.RoleUser))
		return srv.newComponentHostBounds(r, site, spec, governanceCeiling{}, nil)
	}
	owner := as(adoTestOwner)
	for _, h := range adoContosoHosts {
		if !owner.collides(mustDestination(t, h)) || !owner.collides(mustDestination(t, h+":8443")) {
			t.Errorf("%q does not collide with the Azure DevOps lane", h)
		}
	}
	if owner.collides(mustDestination(t, "fabrikam.visualstudio.com")) {
		t.Error("another organisation's host collides: the lane is pinned to one")
	}
	// A run that declares no Azure DevOps repository has no lane.
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(operatorCtx(adoTestOwner, "a@corp.example", oidc.RoleUser))
	if b := srv.newComponentHostBounds(r, site, types.RunPolicySpec{}, governanceCeiling{}, nil); b.collides(mustDestination(t, "dev.azure.com")) {
		t.Error("dev.azure.com collides on a run with no Azure DevOps repository")
	}
}

// The deny set is the caller's ceiling, the run's own policy and every
// referenced workspace's permanent denies, matched as dispatch re-asserts them.
func TestComponentHostBounds_DenySet(t *testing.T) {
	b := (&Server{}).newComponentHostBounds(httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil), types.SiteConfig{},
		types.RunPolicySpec{DeniedDomains: []string{"policy-denied.example"}},
		governanceCeiling{Spec: types.RunPolicySpec{DeniedDomains: []string{"*.ceiling-denied.example", "ported.example:8443"}}},
		[]types.Workspace{{DeniedEgress: []string{"ws-denied.example"}}, {DeniedEgress: []string{"ws2-denied.example"}}})
	for entry, want := range map[string]bool{
		"policy-denied.example": true, "policy-denied.example:8443": true,
		"api.ceiling-denied.example": true, "*.eu.ceiling-denied.example": true, "ceiling-denied.example": false,
		// A deny on one port walls the host off for a component on any port.
		"ported.example": true, "ported.example:443": true,
		"ws-denied.example": true, "ws2-denied.example": true,
		"allowed.example": false,
	} {
		if got := b.denied(mustDestination(t, entry)); got != want {
			t.Errorf("denied(%q) = %v, want %v", entry, got, want)
		}
	}
}
