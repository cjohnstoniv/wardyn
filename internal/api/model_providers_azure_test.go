// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func azureProvider() types.ModelProvider {
	return types.ModelProvider{
		ID: "foundry", Kind: types.ModelProviderAzureFoundry,
		Azure:     &types.AzureSettings{Endpoint: "https://res.services.ai.azure.com", Route: types.AzureRouteAnthropic},
		Harnesses: []types.ProviderHarness{{Harness: "claude-code", Model: "claude-sonnet-4-5", FastModel: "claude-haiku-4-5"}},
	}
}

func azureOpenAIProvider() types.ModelProvider {
	return types.ModelProvider{
		ID: "foundry-openai", Kind: types.ModelProviderAzureFoundry,
		Azure:     &types.AzureSettings{Endpoint: "https://res.openai.azure.com", Route: types.AzureRouteOpenAIV1},
		Harnesses: []types.ProviderHarness{{Harness: "codex-cli", Model: "gpt-5-codex"}},
	}
}

// setAzureGate sets azureFoundryGateReady for one test and restores it.
func setAzureGate(t *testing.T, ready bool) {
	t.Helper()
	old := azureFoundryGateReady
	azureFoundryGateReady = ready
	t.Cleanup(func() { azureFoundryGateReady = old })
}

func publicResolver(string) ([]net.IP, error) { return []net.IP{net.ParseIP("20.50.1.1")}, nil }

func azureEnv() providerWriteEnv {
	return providerWriteEnv{EntraLoginConfigured: true, Resolve: publicResolver}
}

// azureDoorServer is a Server over a fake site-config store with the Entra console login configured and the
// resolver injected, so a door's own env is the one under test.
func azureDoorServer(t *testing.T, fake *fakeSiteConfigStore, resolve func(string) ([]net.IP, error)) *Server {
	t.Helper()
	cfg := baseTestConfig(newHarness(t), fake)
	cfg.ADOLoginFacts = func() (string, string, bool) { return "client-id", "tenant-id", true }
	cfg.HostResolver = resolve
	return New(cfg)
}

// TestAzureFoundryRefusedWhileGateClosed: with azureFoundryGateReady false, the validator and every site-config
// write door refuse an azure_foundry row, whatever else about it is valid, and store nothing.
func TestAzureFoundryRefusedWhileGateClosed(t *testing.T) {
	if azureFoundryGateReady {
		t.Fatal("azureFoundryGateReady is true in this tree; az-a5 owns flipping it")
	}
	disabled := azureProvider()
	disabled.Disabled = true
	for _, p := range []types.ModelProvider{azureProvider(), azureOpenAIProvider(), disabled} {
		block := providerBlock(p)
		if _, err := validateModelProviders(normalizeModelProviders(block), azureEnv()); err == nil || !strings.Contains(err.Error(), "not available in this release") {
			t.Errorf("validateModelProviders(%s) = %v, want the gate refusal", p.ID, err)
		}
		raw, _ := json.Marshal(block)
		siteRaw, _ := json.Marshal(types.SiteConfig{ModelProviders: block})
		for _, door := range []struct{ name, path, body string }{
			{"PUT /model-providers", "/api/v1/model-providers", string(raw)},
			{"PUT /site-config", "/api/v1/site-config", string(siteRaw)},
		} {
			fake := &fakeSiteConfigStore{}
			srv := azureDoorServer(t, fake, publicResolver)
			w := do(t, srv, http.MethodPut, door.path, adminToken, door.body)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not available in this release") {
				t.Errorf("%s with %s = %d %s, want a 400 naming the gate", door.name, p.ID, w.Code, w.Body.String())
			}
			if fake.putSeen != nil {
				t.Errorf("%s with %s stored a document despite the refusal", door.name, p.ID)
			}
		}
	}
}

// TestValidateProviderAzure: each rule of the kind has an accepting and a refusing case, with the gate open.
func TestValidateProviderAzure(t *testing.T) {
	setAzureGate(t, true)
	with := func(p types.ModelProvider, edit func(*types.ModelProvider)) types.ModelProvider {
		edit(&p)
		return p
	}
	endpoint := func(ep string) types.ModelProvider {
		return with(azureProvider(), func(p *types.ModelProvider) { p.Azure.Endpoint = ep })
	}
	for _, tc := range []struct {
		name    string
		p       types.ModelProvider
		noLogin bool
		raw     bool // validate as written, skipping normalizeModelProviders
		want    string
	}{
		{name: "the Messages row", p: azureProvider()},
		{name: "the Responses row", p: azureOpenAIProvider()},
		{name: "a row with no harness yet", p: with(azureProvider(), func(p *types.ModelProvider) { p.Harnesses = nil })},

		// suffix
		{name: "the openai suffix itself", p: endpoint("https://openai.azure.com")},
		{name: "the cognitive services suffix", p: endpoint("https://res.cognitiveservices.azure.com")},
		{name: "a dot-anchoring bypass", p: endpoint("https://evilopenai.azure.com"), want: "not an Azure Foundry host"},
		{name: "a suffix as a prefix", p: endpoint("https://res.openai.azure.com.attacker.example"), want: "not an Azure Foundry host"},
		{name: "a sovereign cloud", p: endpoint("https://res.openai.azure.us"), want: "not an Azure Foundry host"},
		{name: "an arbitrary host", p: endpoint("https://gateway.corp.example"), want: "not an Azure Foundry host"},

		// endpoint shape
		{name: "a trailing slash", p: endpoint("https://res.services.ai.azure.com/")},
		{name: "an uppercase host", p: endpoint("https://RES.Services.AI.Azure.com")},
		{name: "an uppercase host, unnormalized", p: endpoint("https://RES.services.ai.azure.com"), raw: true, want: "lower-case host"},
		{name: "http", p: endpoint("http://res.services.ai.azure.com"), want: "must be https://<host>"},
		{name: "a sub-path", p: endpoint("https://res.services.ai.azure.com/anthropic"), want: "no path"},
		{name: "a port", p: endpoint("https://res.services.ai.azure.com:443"), want: "no path, port"},
		{name: "userinfo", p: endpoint("https://u:p@res.services.ai.azure.com"), want: "no path, port"},
		{name: "a query", p: endpoint("https://res.services.ai.azure.com?x=1"), want: "no path, port"},
		{name: "a fragment", p: endpoint("https://res.services.ai.azure.com#x"), want: "no path, port"},
		{name: "a trailing dot", p: endpoint("https://res.services.ai.azure.com."), want: "no trailing dot"},
		{name: "a host that smuggles a suffix in a fragment", p: endpoint("https://evil.example#.openai.azure.com"), want: "no path, port"},
		{name: "no endpoint", p: with(azureProvider(), func(p *types.ModelProvider) { p.Azure = nil }), want: "needs azure.endpoint and azure.route"},

		// route and harness pairing
		{name: "an unknown route", p: with(azureProvider(), func(p *types.ModelProvider) { p.Azure.Route = "responses" }), want: "azure.route"},
		{name: "the Messages harness on openai_v1", p: with(azureProvider(), func(p *types.ModelProvider) { p.Azure.Route = types.AzureRouteOpenAIV1 }),
			want: `set azure.route to "anthropic"`},
		{name: "codex on the anthropic route", p: with(azureOpenAIProvider(), func(p *types.ModelProvider) { p.Azure.Route = types.AzureRouteAnthropic }),
			want: `set azure.route to "openai_v1"`},
		{name: "a harness neither route serves", p: with(azureProvider(), func(p *types.ModelProvider) { p.Harnesses[0].Harness = "none" }),
			want: "cannot use an azure_foundry provider"},

		// deployment and fast model
		{name: "no deployment", p: with(azureProvider(), func(p *types.ModelProvider) { p.Harnesses[0].Model = "" }), want: "deployment name"},
		{name: "a fast model on the Messages harness", p: with(azureProvider(), func(p *types.ModelProvider) { p.Harnesses[0].FastModel = "haiku-deployment" })},
		{name: "a fast model on codex", p: with(azureOpenAIProvider(), func(p *types.ModelProvider) { p.Harnesses[0].FastModel = "gpt-5-mini" }),
			want: "fast_model applies only to the claude-code harness"},
		{name: "a fast model that is not a model id", p: with(azureProvider(), func(p *types.ModelProvider) { p.Harnesses[0].FastModel = `h"` }), want: "fast_model"},

		// Entra login
		{name: "no Entra console login", p: azureProvider(), noLogin: true, want: "delegated permission for this provider's audience (https://ai.azure.com/.default)"},
		{name: "no Entra console login, openai_v1", p: azureOpenAIProvider(), noLogin: true, want: "https://cognitiveservices.azure.com/.default"},

		// the other kinds' fields
		{name: "base_url", p: with(azureProvider(), func(p *types.ModelProvider) { p.BaseURL = "https://gw.example" }), want: "base_url, bedrock and auth do not apply"},
		{name: "bedrock", p: with(azureProvider(), func(p *types.ModelProvider) { p.Bedrock = &types.BedrockSettings{Region: "us-east-1"} }), want: "do not apply"},
		{name: "auth", p: with(azureProvider(), func(p *types.ModelProvider) { p.Auth = &types.ProviderAuth{Header: "api-key"} }), want: "do not apply"},
		{name: "azure on another kind", p: with(keyProvider("a", "claude-code"), func(p *types.ModelProvider) { p.Azure = azureProvider().Azure }),
			want: "azure applies only to the azure_foundry kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := azureEnv()
			env.EntraLoginConfigured = !tc.noLogin
			block := providerBlock(tc.p)
			if !tc.raw {
				block = normalizeModelProviders(block)
			}
			_, err := validateModelProviders(block, env)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused a valid row: %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("accepted a row it must refuse (want %q)", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("refusal = %q, want it to carry %q", err, tc.want)
			}
		})
	}
}

// TestNormalizeAzureEndpoint: the host is lower-cased and one trailing slash trimmed in storage.
func TestNormalizeAzureEndpoint(t *testing.T) {
	p := azureProvider()
	p.Azure.Endpoint = "  HTTPS://Res.Services.AI.Azure.com/ "
	got := normalizeModelProviders(providerBlock(p)).Providers[0].Azure.Endpoint
	if got != "https://res.services.ai.azure.com" {
		t.Errorf("endpoint = %q, want the lower-case host with no trailing slash", got)
	}
}

// TestAzureEndpointWarnings: the advisory is present for a private resolution no InternalHosts entry covers, and
// absent when an entry covers it, when the address is public, when the lookup fails, and for a disabled row.
func TestAzureEndpointWarnings(t *testing.T) {
	private := func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.1.2.3")}, nil }
	for _, tc := range []struct {
		name     string
		resolve  func(string) ([]net.IP, error)
		internal []types.InternalHost
		disabled bool
		want     bool
	}{
		{name: "private and uncovered", resolve: private, want: true},
		{name: "covered by the host suffix", resolve: private, internal: []types.InternalHost{{HostSuffix: "services.ai.azure.com"}}},
		{name: "covered by a CIDR holding the address", resolve: private,
			internal: []types.InternalHost{{HostSuffix: "res.services.ai.azure.com", CIDRs: []string{"10.0.0.0/8"}}}},
		{name: "an entry whose CIDR misses the address", resolve: private, want: true,
			internal: []types.InternalHost{{HostSuffix: "services.ai.azure.com", CIDRs: []string{"192.168.0.0/16"}}}},
		{name: "an entry for another host", resolve: private, want: true, internal: []types.InternalHost{{HostSuffix: "corp.example"}}},
		{name: "a suffix that is not dot-anchored", resolve: private, want: true, internal: []types.InternalHost{{HostSuffix: "ervices.ai.azure.com"}}},
		{name: "a public address", resolve: publicResolver},
		{name: "loopback is never liftable", want: true,
			resolve:  func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("127.0.0.1")}, nil },
			internal: []types.InternalHost{{HostSuffix: "services.ai.azure.com"}}},
		{name: "a lookup that fails", resolve: func(string) ([]net.IP, error) { return nil, errors.New("nxdomain") }},
		{name: "a disabled row", resolve: private, disabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := azureProvider()
			p.Disabled = tc.disabled
			env := azureEnv()
			env.Resolve, env.InternalHosts = tc.resolve, tc.internal
			got := azureEndpointWarnings(providerBlock(p), env)
			if (len(got) == 1) != tc.want || len(got) > 1 {
				t.Fatalf("warnings = %q, want present=%v", got, tc.want)
			}
			if tc.want && (!strings.Contains(got[0], "internal_hosts") || !strings.Contains(got[0], "upstream_proxy_no_proxy")) {
				t.Errorf("advisory = %q, want both knobs named", got[0])
			}
		})
	}
}

// TestAzureFoundryWritesCarryWarnings: with the gate open, both write doors accept a valid row and return the
// advisory as model_provider_warnings; a covering internal_hosts entry removes it.
func TestAzureFoundryWritesCarryWarnings(t *testing.T) {
	setAzureGate(t, true)
	private := func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("10.1.2.3")}, nil }
	block := providerBlock(azureProvider())
	provRaw, _ := json.Marshal(block)
	read := func(t *testing.T, w interface{ Bytes() []byte }) []string {
		var body struct {
			Warnings []string `json:"model_provider_warnings"`
		}
		if err := json.Unmarshal(w.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Warnings
	}
	for _, withEntry := range []bool{false, true} {
		internal := []types.InternalHost(nil)
		if withEntry {
			internal = []types.InternalHost{{HostSuffix: "services.ai.azure.com"}}
		}
		siteRaw, _ := json.Marshal(types.SiteConfig{ModelProviders: block, InternalHosts: internal})

		srv := azureDoorServer(t, &fakeSiteConfigStore{cfg: types.SiteConfig{InternalHosts: internal}}, private)
		w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, string(provRaw))
		if w.Code != http.StatusOK {
			t.Fatalf("PUT /model-providers = %d %s", w.Code, w.Body.String())
		}
		if got := read(t, w.Body); (len(got) == 1) == withEntry {
			t.Errorf("PUT /model-providers (internal_hosts entry=%v) warnings = %q", withEntry, got)
		}

		srv = azureDoorServer(t, &fakeSiteConfigStore{}, private)
		w = do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, string(siteRaw))
		if w.Code != http.StatusOK {
			t.Fatalf("PUT /site-config = %d %s", w.Code, w.Body.String())
		}
		if got := read(t, w.Body); (len(got) == 1) == withEntry {
			t.Errorf("PUT /site-config (internal_hosts entry=%v) warnings = %q", withEntry, got)
		}
	}
}

// TestProviderAddressChangedAzure: an endpoint or route change is an address change, which purges every
// person's -entra blob; a deployment or fast-model change is not.
func TestProviderAddressChangedAzure(t *testing.T) {
	base := azureProvider()
	edit := func(f func(*types.ModelProvider)) types.ModelProvider {
		p := azureProvider()
		f(&p)
		return p
	}
	for _, tc := range []struct {
		name    string
		changed types.ModelProvider
		want    bool
	}{
		{"the endpoint", edit(func(p *types.ModelProvider) { p.Azure.Endpoint = "https://other.services.ai.azure.com" }), true},
		{"the route", edit(func(p *types.ModelProvider) { p.Azure.Route = types.AzureRouteOpenAIV1 }), true},
		{"the deployment", edit(func(p *types.ModelProvider) { p.Harnesses[0].Model = "another" }), false},
		{"the fast model", edit(func(p *types.ModelProvider) { p.Harnesses[0].FastModel = "another" }), false},
	} {
		if got := providerAddressChanged(base, tc.changed); got != tc.want {
			t.Errorf("providerAddressChanged after a %s change = %v, want %v", tc.name, got, tc.want)
		}
		if same := providerAddressDigest(base) == providerAddressDigest(tc.changed); same == tc.want {
			t.Errorf("providerAddressDigest after a %s change: unchanged=%v, want unchanged=%v", tc.name, same, !tc.want)
		}
	}
	if !slices.Contains(types.ClosedModelProviderKindList(), "azure_foundry") {
		t.Error("azure_foundry is not in the closed kind list")
	}
}
