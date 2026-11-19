// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A custom_endpoint provider can carry non-secret headers the run proxy sets on every request to the
// provider's gateway (Portkey's x-portkey-provider, or any gateway that routes on a header). The rows
// below pin the write boundary, the address rule (a header edit removes people's stored credentials, as a
// base_url edit does), the audit shape and what dispatch hands the proxy.

func headerRow(hs ...types.ProviderHeader) types.ModelProvider {
	p := endpointProvider()
	p.UID = "uid-headers"
	p.ExtraHeaders = hs
	return p
}

func provHdr(name, value string) types.ProviderHeader {
	return types.ProviderHeader{Name: name, Value: value}
}

func TestExtraHeaders_ValidationTable(t *testing.T) {
	with := func(edit func(*types.ModelProvider)) *types.ModelProviders {
		p := headerRow(provHdr("X-Portkey-Provider", "@openai-prod"))
		edit(&p)
		return providerBlock(p)
	}
	eight := make([]types.ProviderHeader, 8)
	for i := range eight {
		eight[i] = provHdr(fmt.Sprintf("X-Team-%d", i), "v")
	}
	for _, tc := range []struct {
		name  string
		block *types.ModelProviders
		want  string // "" accepted; else a substring of the refusal
	}{
		{"one header", with(func(p *types.ModelProvider) {}), ""},
		{"none", providerBlock(endpointProvider()), ""},
		{"eight headers", with(func(p *types.ModelProvider) { p.ExtraHeaders = eight }), ""},
		{"a value of 2048 bytes", with(func(p *types.ModelProvider) { p.ExtraHeaders[0].Value = strings.Repeat("a", 2048) }), ""},
		{"a name in another case", with(func(p *types.ModelProvider) { p.ExtraHeaders[0].Name = "x-portkey-provider" }), ""},
		{"on a key kind", providerBlock(func() types.ModelProvider {
			p := keyProvider("a", "claude-code")
			p.ExtraHeaders = []types.ProviderHeader{provHdr("X-Team", "t")}
			return p
		}()), "extra_headers applies only to a custom_endpoint"},
		{"on a local model", providerBlock(func() types.ModelProvider {
			p := localModelProvider()
			p.ExtraHeaders = []types.ProviderHeader{provHdr("X-Team", "t")}
			return p
		}()), "extra_headers applies only to a custom_endpoint"},
		{"nine headers", with(func(p *types.ModelProvider) {
			p.ExtraHeaders = append(slices.Clone(eight), provHdr("X-Ninth", "v"))
		}), "at most 8"},
		{"a name that is not a header name", with(func(p *types.ModelProvider) { p.ExtraHeaders[0].Name = "X Team" }), "not a valid HTTP header name"},
		{"an empty name", with(func(p *types.ModelProvider) { p.ExtraHeaders[0].Name = "" }), "not a valid HTTP header name"},
		{"the same name twice", with(func(p *types.ModelProvider) {
			p.ExtraHeaders = []types.ProviderHeader{provHdr("X-Team", "a"), provHdr("x-team", "b")}
		}), "listed twice"},
		{"Authorization", with(func(p *types.ModelProvider) { p.ExtraHeaders[0].Name = "Authorization" }), "Authorization"},
		{"a credential-looking name", with(func(p *types.ModelProvider) { p.ExtraHeaders[0].Name = "X-Portkey-Api-Key" }), "X-Portkey-Api-Key"},
		{"a Wardyn header", with(func(p *types.ModelProvider) { p.ExtraHeaders[0].Name = "X-Wardyn-Run" }), "X-Wardyn-Run"},
		{"a hop-by-hop header", with(func(p *types.ModelProvider) { p.ExtraHeaders[0].Name = "Upgrade" }), "Upgrade"},
		{"Content-Type", with(func(p *types.ModelProvider) { p.ExtraHeaders[0].Name = "Content-Type" }), "Content-Type"},
		{"the provider's own auth header", with(func(p *types.ModelProvider) {
			p.Auth = &types.ProviderAuth{Header: "X-Gateway-Pass", Format: "%s"}
			p.ExtraHeaders[0].Name = "x-gateway-pass"
		}), "X-Gateway-Pass"},
		{"a harness's auth header", with(func(p *types.ModelProvider) {
			p.Harnesses[1].AuthHeader = "X-Second-Pass"
			p.ExtraHeaders[0].Name = "X-Second-Pass"
		}), "X-Second-Pass"},
		{"an empty value", with(func(p *types.ModelProvider) { p.ExtraHeaders[0].Value = "" }), "value"},
		{"a value over 2048 bytes", with(func(p *types.ModelProvider) { p.ExtraHeaders[0].Value = strings.Repeat("a", 2049) }), "2048"},
		{"a value with a line break", with(func(p *types.ModelProvider) { p.ExtraHeaders[0].Value = "a\r\nX-Injected: 1" }), "value"},
		{"a value shaped like a token", with(func(p *types.ModelProvider) { p.ExtraHeaders[0].Value = "ghp_" + strings.Repeat("a", 36) }), "value"},
		{"a value shaped like an sk- key", with(func(p *types.ModelProvider) { p.ExtraHeaders[0].Value = "sk-" + strings.Repeat("a", 24) }), "value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateModelProviders(normalizeModelProviders(tc.block), providerWriteEnv{})
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused a valid block: %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("accepted a block it must refuse (want %q)", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("refusal = %q, want it to carry %q", err, tc.want)
			}
			// The refusal names the header, never the value: a pasted secret must not come back in a response.
			if err != nil && (strings.Contains(err.Error(), "ghp_") || strings.Contains(err.Error(), "sk-aaaa")) {
				t.Errorf("the refusal carries the value: %q", err)
			}
		})
	}
}

// Names are stored in their canonical case and values trimmed, so "the same header" is one spelling.
func TestExtraHeaders_Normalize(t *testing.T) {
	got := normalizeModelProviders(providerBlock(headerRow(provHdr("x-portkey-provider", "  @openai-prod  "), provHdr(" X-TEAM ", "platform"))))
	hs := got.Providers[0].ExtraHeaders
	if len(hs) != 2 || hs[0] != provHdr("X-Portkey-Provider", "@openai-prod") || hs[1] != provHdr("X-Team", "platform") {
		t.Errorf("ExtraHeaders = %+v, want canonical names and trimmed values in the order given", hs)
	}
	if none := normalizeModelProviders(providerBlock(endpointProvider())); none.Providers[0].ExtraHeaders != nil {
		t.Errorf("a provider with no headers normalized to %+v", none.Providers[0].ExtraHeaders)
	}
}

// A header edit is an address change: it purges people's stored credentials, as a base_url edit does,
// because the credential was given to a destination that carried other headers.
func TestExtraHeaders_AreAPartOfTheAddress(t *testing.T) {
	base := headerRow(provHdr("X-Portkey-Provider", "@openai-prod"), provHdr("X-Team", "platform"))
	for name, edit := range map[string]func(*types.ModelProvider){
		"a value changes": func(p *types.ModelProvider) { p.ExtraHeaders[0].Value = "@anthropic-prod" },
		"a name changes":  func(p *types.ModelProvider) { p.ExtraHeaders[1].Name = "X-Squad" },
		"one is added":    func(p *types.ModelProvider) { p.ExtraHeaders = append(p.ExtraHeaders, provHdr("X-Env", "prod")) },
		"one is removed":  func(p *types.ModelProvider) { p.ExtraHeaders = p.ExtraHeaders[:1] },
		"all are removed": func(p *types.ModelProvider) { p.ExtraHeaders = nil },
	} {
		next := headerRow(provHdr("X-Portkey-Provider", "@openai-prod"), provHdr("X-Team", "platform"))
		edit(&next)
		if !providerAddressChanged(base, next) {
			t.Errorf("%s: providerAddressChanged = false, want the credentials purged", name)
		}
		if providerAddressDigest(base) == providerAddressDigest(next) {
			t.Errorf("%s: the address digest did not move, so a sign-in started before the edit would still land", name)
		}
		if !slices.Contains(invalidatedProviderUIDs(providerBlock(base), providerBlock(next)), base.UID) {
			t.Errorf("%s: the provider's credentials are not invalidated", name)
		}
	}

	reordered := headerRow(provHdr("X-Team", "platform"), provHdr("X-Portkey-Provider", "@openai-prod"))
	if providerAddressChanged(base, reordered) || providerAddressDigest(base) == "" || providerAddressDigest(base) != providerAddressDigest(reordered) {
		t.Error("the same headers in another order purged people's credentials")
	}

	// A provider with no headers hashes exactly the inputs it always has, so no stored digest moves.
	plain := endpointProvider()
	harnesses := map[string][2]string{}
	for _, h := range plain.Harnesses {
		harnesses[h.Harness] = [2]string{h.Path, h.AuthHeader}
	}
	sum := sha256.Sum256(mustJSON([]any{plain.BaseURL, bedrockAddress(plain), authHeader(plain.Auth), harnesses}))
	if got := providerAddressDigest(plain); got != hex.EncodeToString(sum[:]) {
		t.Errorf("a provider with no extra headers hashes differently than before: %s", got)
	}
}

// model_provider.write names which providers carry headers, by header name only: the audit log is read by
// more people than the providers page is, and a value is the admin's gateway configuration.
func TestExtraHeaders_AuditCarriesNamesOnly(t *testing.T) {
	row := headerRow(provHdr("X-Portkey-Provider", "@openai-prod-SENTINEL"), provHdr("X-Team", "platform"))
	data := modelProviderAuditData(types.ModelProviders{}, types.ModelProviders{Providers: []types.ModelProvider{row}})
	got, ok := data["extra_headers"].(map[string][]string)
	if !ok || len(got) != 1 || !slices.Equal(got[row.ID], []string{"X-Portkey-Provider", "X-Team"}) {
		t.Fatalf("extra_headers = %#v, want {%q: [X-Portkey-Provider X-Team]}", data["extra_headers"], row.ID)
	}
	if body := string(mustJSON(data)); strings.Contains(body, "SENTINEL") || strings.Contains(body, "platform") {
		t.Errorf("the audit row carries a header value: %s", body)
	}
	plain := modelProviderAuditData(types.ModelProviders{}, types.ModelProviders{Providers: []types.ModelProvider{endpointProvider()}})
	if _, named := plain["extra_headers"]; named {
		t.Error("a provider with no headers names extra_headers: the row must read as it always has")
	}
	changed := modelProviderAuditData(types.ModelProviders{Providers: []types.ModelProvider{headerRow(provHdr("X-Team", "a"))}},
		types.ModelProviders{Providers: []types.ModelProvider{headerRow(provHdr("X-Team", "b"))}})
	if ids, _ := changed["base_url_changed"].([]string); !slices.Equal(ids, []string{"corp-gateway"}) {
		t.Errorf("base_url_changed = %v, want a header edit named like any address change", changed["base_url_changed"])
	}
}

func headerLane(t *testing.T, p types.ModelProvider, harness string) providerKeyLane {
	t.Helper()
	l, ok := providerKeyLaneFor(p, harness)
	if !ok {
		t.Fatalf("no key lane for %s on %s", p.ID, harness)
	}
	return l
}

// Dispatch hands the proxy the headers of a custom_endpoint provider only, keyed by the gateway's host.
func TestExtraHeaders_DispatchFillsTheMapForACustomEndpointOnly(t *testing.T) {
	p := headerRow(provHdr("X-Portkey-Provider", "@openai-prod"), provHdr("X-Team", "platform"))
	var d providerDispatch
	d.addExtraHeaders(headerLane(t, p, "claude-code"))
	d.addExtraHeaders(headerLane(t, p, "codex-cli")) // the same provider on a second harness: same host, same headers
	want := map[string]map[string]string{"gateway.corp.example": {"X-Portkey-Provider": "@openai-prod", "X-Team": "platform"}}
	if fmt.Sprint(d.extraHeaders) != fmt.Sprint(want) {
		t.Fatalf("extraHeaders = %v, want %v", d.extraHeaders, want)
	}

	// A key kind routed through a gateway never carries any, whatever its stored row says.
	key := keyProvider("a", "claude-code")
	key.BaseURL = "https://llm.corp.example/anthropic"
	key.ExtraHeaders = []types.ProviderHeader{provHdr("X-Team", "t")}
	var k providerDispatch
	k.addExtraHeaders(headerLane(t, key, "claude-code"))
	if k.extraHeaders != nil {
		t.Errorf("a key kind produced %v, want nothing", k.extraHeaders)
	}

	// A custom endpoint with none produces no entry, so the proxy config carries no key.
	var n providerDispatch
	n.addExtraHeaders(headerLane(t, endpointProvider(), "claude-code"))
	if n.extraHeaders != nil {
		t.Errorf("a provider with no headers produced %v, want nothing", n.extraHeaders)
	}
}

// Two providers cannot share one gateway host in a run, so one host never carries two header sets.
func TestExtraHeaders_TwoProvidersOnOneHostConflict(t *testing.T) {
	a, b := headerRow(provHdr("X-Team", "a")), headerRow(provHdr("X-Team", "b"))
	a.ID, a.UID, b.ID, b.UID = "gw-a", "uid-a", "gw-b", "uid-b"
	pairs := []types.RunHarness{{Harness: "claude-code", ModelProviderID: a.ID}, {Harness: "codex-cli", ModelProviderID: b.ID}}
	lanes := make([]providerLane, len(pairs))
	for i, p := range []types.ModelProvider{a, b} {
		lanes[i].chosen = &chosenProvider{provider: p}
		lanes[i].key = headerLane(t, p, pairs[i].Harness)
	}
	if j, host := laneConflict(pairs, lanes); j != 1 || host != "gateway.corp.example" {
		t.Errorf("laneConflict = (%d, %q), want the second provider refused on the shared host", j, host)
	}
	if !harnessProviderPairsConflict("claude-code", a, "codex-cli", b) {
		t.Error("the create door allows two providers with different headers on one host")
	}
}
