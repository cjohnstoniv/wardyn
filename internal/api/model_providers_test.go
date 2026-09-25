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

func keyProvider(id string, harnesses ...string) types.ModelProvider {
	p := types.ModelProvider{ID: id, Kind: types.ModelProviderAnthropicAPIKey}
	for _, h := range harnesses {
		p.Harnesses = append(p.Harnesses, types.ProviderHarness{Harness: h})
	}
	return p
}

func providerBlock(ps ...types.ModelProvider) *types.ModelProviders {
	return &types.ModelProviders{Providers: ps}
}

func ssoProvider() types.ModelProvider {
	return types.ModelProvider{
		ID: "bedrock-prod", Kind: types.ModelProviderBedrockSSO,
		Bedrock: &types.BedrockSettings{
			Region: "us-east-1", SSOStartURL: "https://acme.awsapps.com/start",
			SSOAccountID: "123456789012", SSORoleName: "BedrockUser",
		},
		Harnesses: []types.ProviderHarness{{Harness: "claude-code", Model: "us.anthropic.claude-sonnet-4-5-20250929-v1:0"}},
	}
}

func endpointProvider() types.ModelProvider {
	return types.ModelProvider{
		ID: "corp-gateway", Name: "Corp gateway", Kind: types.ModelProviderCustomEndpoint,
		BaseURL: "https://gateway.corp.example",
		Harnesses: []types.ProviderHarness{
			{Harness: "claude-code", Path: "/anthropic"},
			{Harness: "codex-cli", Path: "/v1", Model: "gpt-5-codex"},
		},
	}
}

// TestValidateModelProviders is the write-boundary table: one case per rule.
func TestValidateModelProviders(t *testing.T) {
	with := func(p types.ModelProvider, edit func(*types.ModelProvider)) *types.ModelProviders {
		edit(&p)
		return providerBlock(p)
	}
	for _, tc := range []struct {
		name  string
		block *types.ModelProviders
		want  string // "" = accepted; else a substring of the refusal
	}{
		{name: "a nil block is today", block: nil},
		{name: "a key provider on claude-code", block: providerBlock(keyProvider("anthropic", "claude-code"))},
		{name: "a provider used by no harness yet", block: providerBlock(keyProvider("anthropic"))},
		{name: "bedrock sso with its pin", block: providerBlock(ssoProvider())},
		{name: "a custom endpoint on both harnesses", block: providerBlock(endpointProvider())},
		{name: "a disabled provider", block: with(keyProvider("a", "claude-code"), func(p *types.ModelProvider) { p.Disabled = true })},
		{name: "a route-through gateway on a key kind",
			block: with(keyProvider("a", "claude-code"), func(p *types.ModelProvider) { p.BaseURL = "https://llm.corp.example/anthropic" })},
		{name: "a 1M-context model id", block: with(keyProvider("a", "claude-code"), func(p *types.ModelProvider) {
			p.Harnesses[0].Model = "claude-sonnet-4-5[1m]"
		})},

		{name: "an id with capitals", block: providerBlock(keyProvider("Corp")), want: "is not a provider id"},
		// A colon would make the audit's "<harness>:<id>" pairs ambiguous.
		{name: "an id with a colon", block: providerBlock(keyProvider("corp:gw")), want: "is not a provider id"},
		{name: "an id over 64 characters", block: providerBlock(keyProvider(strings.Repeat("a", 65))), want: "is not a provider id"},
		{name: "a duplicate id", block: providerBlock(keyProvider("a"), keyProvider("a")), want: "is not unique"},
		{name: "a kind outside the closed set",
			block: with(keyProvider("a"), func(p *types.ModelProvider) { p.Kind = "bedrock_env" }), want: "want one of"},
		{name: "a name with a control character",
			block: with(keyProvider("a"), func(p *types.ModelProvider) { p.Name = "Corp\ngateway" }), want: "control characters"},
		{name: "a custom endpoint with no base url",
			block: with(endpointProvider(), func(p *types.ModelProvider) { p.BaseURL = "" }), want: "base_url is required"},
		{name: "rule 1: plain http",
			block: with(endpointProvider(), func(p *types.ModelProvider) { p.BaseURL = "http://gateway.corp.example" }), want: "https://"},
		{name: "rule 2: a credential in the url",
			block: with(endpointProvider(), func(p *types.ModelProvider) { p.BaseURL = "https://u:p@gateway.corp.example" }), want: "credential"},
		{name: "rule 4: a loopback literal",
			block: with(endpointProvider(), func(p *types.ModelProvider) { p.BaseURL = "https://127.0.0.1" }), want: "loopback"},
		{name: "rule 5: a route-through naming the vendor itself",
			block: with(keyProvider("a"), func(p *types.ModelProvider) { p.BaseURL = "https://api.anthropic.com" }), want: "public provider host"},
		{name: "rule 5 holds a custom endpoint against both vendors",
			block: with(endpointProvider(), func(p *types.ModelProvider) { p.BaseURL = "https://api.openai.com" }), want: "public provider host"},
		{name: "rule 6: a query",
			block: with(endpointProvider(), func(p *types.ModelProvider) { p.BaseURL = "https://gateway.corp.example?x=1" }), want: "query"},
		{name: "a base url on a Bedrock kind",
			block: with(ssoProvider(), func(p *types.ModelProvider) { p.BaseURL = "https://vpce.example" }), want: "bedrock.base_url"},
		{name: "auth on a key kind",
			block: with(keyProvider("a"), func(p *types.ModelProvider) { p.Auth = &types.ProviderAuth{Header: "X-Key"} }), want: "auth applies only"},
		{name: "a header that is not a header name",
			block: with(endpointProvider(), func(p *types.ModelProvider) { p.Auth = &types.ProviderAuth{Header: "X Key"} }), want: "valid HTTP header"},
		{name: "a format with no %s",
			block: with(endpointProvider(), func(p *types.ModelProvider) { p.Auth = &types.ProviderAuth{Format: "Bearer"} }), want: "exactly one %s"},
		{name: "bedrock with no region",
			block: with(ssoProvider(), func(p *types.ModelProvider) { p.Bedrock.Region = "" }), want: "needs bedrock.region"},
		{name: "a region that is a host",
			block: with(ssoProvider(), func(p *types.ModelProvider) { p.Bedrock.Region = "x.attacker.com/" }), want: "not an AWS region"},
		{name: "bedrock settings on a key kind",
			block: with(keyProvider("a"), func(p *types.ModelProvider) { p.Bedrock = &types.BedrockSettings{Region: "us-east-1"} }), want: "bedrock applies only"},
		{name: "bedrock sso with no start url",
			block: with(ssoProvider(), func(p *types.ModelProvider) { p.Bedrock.SSOStartURL = "" }), want: "sso_start_url is required"},
		{name: "a start url on a bearer provider", block: with(ssoProvider(), func(p *types.ModelProvider) {
			p.Kind = types.ModelProviderBedrockBearer
		}), want: "apply only to a bedrock_sso"},
		{name: "half a pin", block: with(ssoProvider(), func(p *types.ModelProvider) { p.Bedrock.SSORoleName = "" }), want: "set together"},
		{name: "a bedrock data plane over plain http",
			block: with(ssoProvider(), func(p *types.ModelProvider) { p.Bedrock.BaseURL = "http://vpce.example" }), want: "https://"},
		{name: "a bedrock harness with no model",
			block: with(ssoProvider(), func(p *types.ModelProvider) { p.Harnesses[0].Model = "" }), want: "model is required"},
		{name: "a model id with a quote", block: with(keyProvider("a", "claude-code"), func(p *types.ModelProvider) {
			p.Harnesses[0].Model = `claude"`
		}), want: "is not a model id"},
		{name: "a harness the catalog does not know", block: providerBlock(keyProvider("a", "ghost")), want: "not an agent Wardyn can wire"},
		{name: "the bring-your-own row takes no provider", block: providerBlock(keyProvider("a", "none")), want: "not an agent Wardyn can wire"},
		{name: "a harness listed twice", block: providerBlock(keyProvider("a", "claude-code", "claude-code")), want: "listed twice"},
		// The catalog's own verbatim protocol fact, never a second opinion.
		{name: "an anthropic key on codex", block: providerBlock(keyProvider("a", "codex-cli")),
			want: `model_providers: "a": ` + reasonXKeyCodex},
		{name: "bedrock on codex", block: with(ssoProvider(), func(p *types.ModelProvider) {
			p.Harnesses[0].Harness = "codex-cli"
		}), want: reasonXBedrockCodex},
		{name: "a path on a key kind", block: with(keyProvider("a", "claude-code"), func(p *types.ModelProvider) {
			p.Harnesses[0].Path = "/v1"
		}), want: "apply only to a custom_endpoint"},
		{name: "a path that changes the host", block: with(endpointProvider(), func(p *types.ModelProvider) {
			p.Harnesses[0].Path = "//evil.example/anthropic"
		}), want: "must start with a single /"},
		{name: "a path with a dot-dot segment", block: with(endpointProvider(), func(p *types.ModelProvider) {
			p.Harnesses[0].Path = "/v1/../admin"
		}), want: "must start with a single /"},
		{name: "a path with no leading slash", block: with(endpointProvider(), func(p *types.ModelProvider) {
			p.Harnesses[0].Path = "v1"
		}), want: "must start with a single /"},
		{name: "a per-harness header override that is not a header", block: with(endpointProvider(), func(p *types.ModelProvider) {
			p.Harnesses[1].AuthHeader = "a:b"
		}), want: "valid HTTP header"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateModelProviders(normalizeModelProviders(tc.block), false)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused a valid block: %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("accepted a block it must refuse (want %q)", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("refusal = %q, want it to carry %q", err, tc.want)
			}
		})
	}
}

// TestNormalizeModelProviders: {} clears, and a custom endpoint with no header
// scheme is given the default one, so a later explicit default is not a change.
func TestNormalizeModelProviders(t *testing.T) {
	if got := normalizeModelProviders(&types.ModelProviders{}); got != nil {
		t.Errorf("an empty block normalized to %+v, want nil — {} is the clear form", got)
	}
	got := normalizeModelProviders(providerBlock(endpointProvider()))
	if a := got.Providers[0].Auth; a == nil || a.Header != "Authorization" || a.Format != "Bearer %s" {
		t.Errorf("auth = %+v, want the default Authorization / Bearer %%s scheme", a)
	}
}

// TestAssignModelProviderUIDs pins the UID's three properties: carried by id,
// never taken from the body, never reissued after a delete.
func TestAssignModelProviderUIDs(t *testing.T) {
	stored := providerBlock(keyProvider("a"))
	assignModelProviderUIDs(stored, nil)
	uid := stored.Providers[0].UID
	if uid == "" {
		t.Fatal("no UID minted on a provider's first write")
	}

	body := providerBlock(keyProvider("a"), keyProvider("b"))
	body.Providers[0].UID = "chosen-by-the-client"
	assignModelProviderUIDs(body, stored)
	if body.Providers[0].UID != uid {
		t.Errorf("uid = %q, want the stored %q — a submitted UID must never be trusted", body.Providers[0].UID, uid)
	}
	if body.Providers[1].UID == "" || body.Providers[1].UID == uid {
		t.Errorf("a new provider got uid %q, want a fresh one", body.Providers[1].UID)
	}

	// A kind change is a new provider as far as credentials go: a key given
	// for one kind of destination must not follow the id to another.
	rekinded := providerBlock(keyProvider("a"), keyProvider("b"))
	rekinded.Providers[0].Kind = types.ModelProviderCustomEndpoint
	assignModelProviderUIDs(rekinded, body)
	if rekinded.Providers[0].UID == uid || rekinded.Providers[0].UID == "" {
		t.Errorf("a kind change kept uid %q — stored credentials would follow the id to a new kind of destination",
			rekinded.Providers[0].UID)
	}
	if rekinded.Providers[1].UID != body.Providers[1].UID {
		t.Error("an unchanged provider beside a kind change lost its UID")
	}

	// Delete "a", then add it back under the same id: a new UID, so credentials
	// given for the deleted provider can never attach to the new one.
	deleted := providerBlock(keyProvider("b"))
	assignModelProviderUIDs(deleted, body)
	readded := providerBlock(keyProvider("a"))
	assignModelProviderUIDs(readded, deleted)
	if readded.Providers[0].UID == uid {
		t.Error("a deleted provider's UID was reissued")
	}
}

// TestSiteConfigDoorCarriesModelProviders is the MDM door: silence carries the
// block forward, {} clears it, the same validator guards it, and the UID is
// server-owned there too.
func TestSiteConfigDoorCarriesModelProviders(t *testing.T) {
	stored := types.SiteConfig{ModelProviders: providerBlock(keyProvider("anthropic", "claude-code"))}
	stored.ModelProviders.Providers[0].UID = "stored-uid"

	t.Run("a body that never names the key carries it forward", func(t *testing.T) {
		fake := &fakeSiteConfigStore{cfg: stored}
		srv, _ := newSiteConfigHarness(t, fake)
		if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"scm_hosts":["github.com"]}`); w.Code != http.StatusOK {
			t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
		}
		if fake.putSeen.ModelProviders == nil {
			t.Fatal("an older client's silence deleted the org's model providers")
		}
	})

	t.Run("an explicit {} clears it", func(t *testing.T) {
		fake := &fakeSiteConfigStore{cfg: stored}
		srv, _ := newSiteConfigHarness(t, fake)
		if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"model_providers":{}}`); w.Code != http.StatusOK {
			t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
		}
		if fake.putSeen.ModelProviders != nil {
			t.Errorf("model_providers = %+v, want nil", fake.putSeen.ModelProviders)
		}
	})

	t.Run("the validator guards this door", func(t *testing.T) {
		srv, _ := newSiteConfigHarness(t, &fakeSiteConfigStore{})
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken,
			`{"model_providers":{"providers":[{"id":"a","kind":"anthropic_api_key","harnesses":[{"harness":"codex-cli"}]}]}}`)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), reasonXKeyCodex) {
			t.Fatalf("PUT = %d %s, want 400 with the catalog's reason", w.Code, w.Body.String())
		}
	})

	t.Run("a named block keeps its stored UID and ignores the body's", func(t *testing.T) {
		fake := &fakeSiteConfigStore{cfg: stored}
		srv, audit := newSiteConfigHarness(t, fake)
		w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken,
			`{"model_providers":{"providers":[{"id":"anthropic","uid":"forged","kind":"anthropic_api_key"},`+
				`{"id":"openai","kind":"openai_api_key","disabled":true}]}}`)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
		}
		got := fake.putSeen.ModelProviders.Providers
		if got[0].UID != "stored-uid" || got[1].UID == "" {
			t.Errorf("uids = %q, %q; want the stored one kept and a new one minted", got[0].UID, got[1].UID)
		}
		d := siteConfigWriteDatum(t, audit)
		if d["model_providers"] != float64(1) {
			t.Errorf("site_config.write model_providers = %v, want 1 (a disabled provider is not counted)", d["model_providers"])
		}
	})
}

// TestModelProvidersNilBlockIsToday is the golden for "additive only": with no
// block, the site-config doors answer and audit exactly what they did before
// model providers existed.
func TestModelProvidersNilBlockIsToday(t *testing.T) {
	fake := &fakeSiteConfigStore{}
	srv, audit := newSiteConfigHarness(t, fake)

	if got := strings.TrimSpace(do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "").Body.String()); got != "{}" {
		t.Errorf("unconfigured GET /site-config = %s, want {}", got)
	}
	w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"scm_hosts":["github.com"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "model_providers") || fake.putSeen.ModelProviders != nil {
		t.Errorf("a body with no provider block grew one: %s", w.Body.String())
	}
	want := []string{
		"agent_providers", "egress_redirects", "egress_redirects_count", "git_providers", "internal_hosts",
		"internal_hosts_count", "scm_hosts_count", "sign_in_help_text", "sign_in_help_url", "storage_configured",
		"upstream_proxy_configured", "upstream_proxy_secret_ref", "upstream_proxy_url",
	}
	if got := slices.Sorted(maps.Keys(siteConfigWriteDatum(t, audit))); !slices.Equal(got, want) {
		t.Errorf("site_config.write keys = %v, want exactly today's %v", got, want)
	}
}

func siteConfigWriteDatum(t *testing.T, audit *recRecorder) map[string]any {
	t.Helper()
	for _, ev := range audit.events {
		if ev.Action != "site_config.write" {
			continue
		}
		var d map[string]any
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	t.Fatal("no site_config.write event")
	return nil
}
