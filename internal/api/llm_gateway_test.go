// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestValidateLLMGateways exercises the seven boot-time rejection rules a
// WARDYN_ANTHROPIC_BASE_URL / WARDYN_OPENAI_BASE_URL value must pass, plus the
// unset -> nil map baseline (byte-identical to today).
func TestValidateLLMGateways(t *testing.T) {
	cases := []struct {
		name        string
		anthropic   string
		openai      string
		ok          bool
		wantHost    string // if ok, the parsed gateway host (via gatewayHost)
		checkVendor string
	}{
		{"both unset -> nil map", "", "", true, "", ""},
		{"good anthropic gateway", "https://llm-gateway.corp.internal", "", true, "llm-gateway.corp.internal", "api.anthropic.com"},
		{"good anthropic gateway, RFC1918 literal", "https://10.40.1.5:8443/v1", "", true, "10.40.1.5", "api.anthropic.com"},
		{"good openai gateway", "", "https://oai-gateway.corp.internal", true, "oai-gateway.corp.internal", "api.openai.com"},
		{"rule 1: http refused", "http://llm-gateway.corp.internal", "", false, "", ""},
		{"rule 2: userinfo refused", "https://user:pass@llm-gateway.corp.internal", "", false, "", ""},
		{"rule 3: empty host refused", "https:///path", "", false, "", ""},
		{"rule 4: loopback literal refused", "https://127.0.0.1", "", false, "", ""},
		{"rule 4: link-local literal refused", "https://169.254.1.1", "", false, "", ""},
		{"rule 4: metadata literal refused", "https://169.254.169.254", "", false, "", ""},
		{"rule 4: unspecified literal refused", "https://0.0.0.0", "", false, "", ""},
		{"rule 4: multicast literal refused", "https://224.0.0.1", "", false, "", ""},
		{"rule 4: nat64-embedded refused", "https://[64:ff9b::a9fe:a9fe]", "", false, "", ""},
		{"rule 4 exception: RFC1918 literal allowed", "https://10.0.0.5", "", true, "10.0.0.5", "api.anthropic.com"},
		{"rule 4 exception: CGNAT literal allowed", "https://100.64.0.5", "", true, "100.64.0.5", "api.anthropic.com"},
		{"rule 5: equals the public host refused", "https://api.anthropic.com", "", false, "", ""},
		{"rule 5 case-insensitive: equals the public host refused", "https://API.ANTHROPIC.COM", "", false, "", ""},
		{"rule 5 trailing-dot: equals the public host refused", "https://api.anthropic.com.", "", false, "", ""},
		{"rule 6: query refused", "https://llm-gateway.corp.internal?x=1", "", false, "", ""},
		{"rule 7: fragment refused", "https://llm-gateway.corp.internal#x", "", false, "", ""},
		{"malformed URL refused", "https://[::", "", false, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, _, err := ValidateLLMGateways(LLMGatewayRaw{BaseURL: c.anthropic}, LLMGatewayRaw{BaseURL: c.openai})
			if c.ok && err != nil {
				t.Fatalf("expected valid, got error: %v", err)
			}
			if !c.ok && err == nil {
				t.Fatalf("expected an error, got nil (out=%v)", out)
			}
			if c.name == "both unset -> nil map" && out != nil {
				t.Fatalf("unset must yield a nil map, got %v", out)
			}
			if c.wantHost != "" {
				base, has := out[c.checkVendor]
				if !has {
					t.Fatalf("expected an entry for %q, got %v", c.checkVendor, out)
				}
				if h := gatewayHost(base); h != c.wantHost {
					t.Fatalf("gatewayHost(%q) = %q, want %q", base, h, c.wantHost)
				}
			}
		})
	}
}

// TestValidateLLMGateways_PathPrefixAndPortPreserved: the normalized base URL
// keeps a non-default port and a path prefix — the proxy's LLMUpstreams
// wiring depends on both surviving validation intact.
func TestValidateLLMGateways_PathPrefixAndPortPreserved(t *testing.T) {
	out, _, err := ValidateLLMGateways(LLMGatewayRaw{BaseURL: "https://llm-gateway.corp.internal:8443/v1/"}, LLMGatewayRaw{})
	if err != nil {
		t.Fatalf("ValidateLLMGateways: %v", err)
	}
	got := out["api.anthropic.com"]
	want := "https://llm-gateway.corp.internal:8443/v1"
	if got != want {
		t.Fatalf("got %q, want %q (one trailing slash trimmed, port+prefix preserved)", got, want)
	}
}

// TestValidateLLMGateways_GatewayAuth pins the two new operator knobs
// (WARDYN_ANTHROPIC_GATEWAY_HEADER / _FORMAT, and the OpenAI pair): unset
// stays a nil map (byte-identical to today, vendor defaults untouched), a
// valid header/format pair is accepted and carried through keyed by the
// public host, and either field may be set alone — the other stays empty
// (meaning "keep the vendor convention"), never defaulted to something else.
func TestValidateLLMGateways_GatewayAuth(t *testing.T) {
	gateways, auth, err := ValidateLLMGateways(LLMGatewayRaw{}, LLMGatewayRaw{})
	if err != nil {
		t.Fatalf("both unset: %v", err)
	}
	if gateways != nil || auth != nil {
		t.Fatalf("both unset must yield nil maps, got gateways=%v auth=%v", gateways, auth)
	}

	_, auth, err = ValidateLLMGateways(
		LLMGatewayRaw{Header: "x-gw-key", Format: "Token %s"},
		LLMGatewayRaw{Header: "x-oai-key"},
	)
	if err != nil {
		t.Fatalf("valid header/format: %v", err)
	}
	if got, want := auth["api.anthropic.com"], (LLMGatewayAuth{Header: "x-gw-key", Format: "Token %s"}); got != want {
		t.Fatalf("anthropic auth = %+v, want %+v", got, want)
	}
	if got, want := auth["api.openai.com"], (LLMGatewayAuth{Header: "x-oai-key"}); got != want {
		t.Fatalf("openai auth (header only, format left empty) = %+v, want %+v", got, want)
	}
}

// TestValidateLLMGateways_GatewayAuthRefusals is the CHECK spec's boot-refusal
// requirement: a malformed WARDYN_*_GATEWAY_HEADER/_FORMAT value refuses boot,
// naming the setting, rather than surfacing later as a confusing upstream
// dial error. Three shapes: a format with no substitution point, one with
// two, and a header that is not a valid HTTP token.
func TestValidateLLMGateways_GatewayAuthRefusals(t *testing.T) {
	cases := []struct {
		name           string
		raw            LLMGatewayRaw
		wantErrContain string
	}{
		{
			name:           "format: no substitution point",
			raw:            LLMGatewayRaw{Format: "Token"},
			wantErrContain: "WARDYN_ANTHROPIC_GATEWAY_FORMAT",
		},
		{
			name:           "format: two substitution points",
			raw:            LLMGatewayRaw{Format: "%s %s"},
			wantErrContain: "WARDYN_ANTHROPIC_GATEWAY_FORMAT",
		},
		{
			name:           "header: not a valid HTTP token",
			raw:            LLMGatewayRaw{Header: "x gw key"},
			wantErrContain: "WARDYN_ANTHROPIC_GATEWAY_HEADER",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gateways, auth, err := ValidateLLMGateways(c.raw, LLMGatewayRaw{})
			if err == nil {
				t.Fatalf("expected a boot refusal, got nil (gateways=%v auth=%v)", gateways, auth)
			}
			if !strings.Contains(err.Error(), c.wantErrContain) {
				t.Errorf("refusal %q does not name %s — the operator cannot tell which knob to fix", err, c.wantErrContain)
			}
			if gateways != nil || auth != nil {
				t.Errorf("a refused boot must return nil maps, got gateways=%v auth=%v", gateways, auth)
			}
		})
	}
}

// TestAnthropicBaseURL pins the subscription/managed lanes' dispatch target
// (runs_dispatch_llm.go): unset must stay byte-identical to today, and a
// configured gateway must be dialed verbatim instead.
func TestAnthropicBaseURL(t *testing.T) {
	s := &Server{}
	if got := s.anthropicBaseURL(); got != "https://api.anthropic.com" {
		t.Fatalf("unset: anthropicBaseURL() = %q, want the vendor default", got)
	}
	s.cfg.LLMGateways = map[string]string{"api.anthropic.com": "https://llm-gateway.corp.internal:8443/v1"}
	if got, want := s.anthropicBaseURL(), "https://llm-gateway.corp.internal:8443/v1"; got != want {
		t.Fatalf("configured: anthropicBaseURL() = %q, want %q", got, want)
	}
}

// TestAnthropicGatewayHostAndHostPort pins the bare-host and host:port forms
// authorSubscriptionInjection and the injection-host allowlist consume: unset
// is empty (no widening), a configured gateway with no explicit port defaults
// to 443, and an explicit port is preserved.
func TestAnthropicGatewayHostAndHostPort(t *testing.T) {
	s := &Server{}
	if h := s.anthropicGatewayHost(); h != "" {
		t.Fatalf("unset: anthropicGatewayHost() = %q, want \"\"", h)
	}
	if hp := s.anthropicGatewayHostPort(); hp != "" {
		t.Fatalf("unset: anthropicGatewayHostPort() = %q, want \"\"", hp)
	}

	s.cfg.LLMGateways = map[string]string{"api.anthropic.com": "https://llm-gateway.corp.internal"}
	if h, want := s.anthropicGatewayHost(), "llm-gateway.corp.internal"; h != want {
		t.Fatalf("anthropicGatewayHost() = %q, want %q", h, want)
	}
	if hp, want := s.anthropicGatewayHostPort(), "llm-gateway.corp.internal:443"; hp != want {
		t.Fatalf("no explicit port: anthropicGatewayHostPort() = %q, want %q (default 443)", hp, want)
	}

	s.cfg.LLMGateways = map[string]string{"api.anthropic.com": "https://llm-gateway.corp.internal:8443/v1"}
	if hp, want := s.anthropicGatewayHostPort(), "llm-gateway.corp.internal:8443"; hp != want {
		t.Fatalf("explicit port: anthropicGatewayHostPort() = %q, want %q", hp, want)
	}
}

// TestSubscriptionInjectionHostAllowed pins the security property: the
// subscription/managed OAuth sentinel may target the vendor host always, the
// configured gateway ONLY when one is set (from s.cfg, never from the host
// argument itself), and nothing else — an unrelated host stays refused
// regardless of gateway config.
func TestSubscriptionInjectionHostAllowed(t *testing.T) {
	s := &Server{}
	if !s.subscriptionInjectionHostAllowed("api.anthropic.com") {
		t.Fatal("the vendor host must always be allowed")
	}
	if s.subscriptionInjectionHostAllowed("llm-gateway.corp.internal") {
		t.Fatal("an unconfigured gateway host must be refused")
	}
	if s.subscriptionInjectionHostAllowed("evil.example.com") {
		t.Fatal("an arbitrary host must always be refused")
	}

	s.cfg.LLMGateways = map[string]string{"api.anthropic.com": "https://llm-gateway.corp.internal:8443"}
	if !s.subscriptionInjectionHostAllowed("api.anthropic.com") {
		t.Fatal("the vendor host must stay allowed once a gateway is configured")
	}
	if !s.subscriptionInjectionHostAllowed("llm-gateway.corp.internal") {
		t.Fatal("the configured gateway host must now be allowed")
	}
	if !s.subscriptionInjectionHostAllowed("LLM-Gateway.corp.internal.") {
		t.Fatal("the comparison must be case-insensitive and trailing-dot-insensitive, like hostEqual everywhere else")
	}
	if s.subscriptionInjectionHostAllowed("evil.example.com") {
		t.Fatal("an arbitrary host must stay refused even with a gateway configured")
	}
}

// TestValidateBedrockBaseURL pins WARDYN_BEDROCK_BASE_URL's own boot gate: it
// inherits validateOneLLMGateway's seven rules (so it cannot drift from the
// gateway knobs), with rule 5 measured against the REGIONAL Bedrock data-plane
// host rather than a vendor's public host. The CGNAT row is the load-bearing
// one — that is what an AWS PrivateLink endpoint resolves into, so a floor that
// refused it would refuse the whole feature.
func TestValidateBedrockBaseURL(t *testing.T) {
	const region = "us-east-1"
	cases := []struct {
		name, raw string
		ok        bool
		wantHost  string
	}{
		{"unset -> empty, byte-identical to today", "", true, ""},
		{"vpc endpoint hostname", "https://vpce-0abc-bedrock-runtime.us-east-1.vpce.amazonaws.com", true, "vpce-0abc-bedrock-runtime.us-east-1.vpce.amazonaws.com"},
		{"rule 1: http refused", "http://vpce-0abc.vpce.amazonaws.com", false, ""},
		{"rule 2: userinfo refused", "https://user:pass@vpce-0abc.vpce.amazonaws.com", false, ""},
		{"rule 3: empty host refused", "https:///path", false, ""},
		{"rule 4: metadata literal refused", "https://169.254.169.254", false, ""},
		{"rule 4: loopback literal refused", "https://127.0.0.1", false, ""},
		{"rule 4 exception: CGNAT literal ACCEPTED (the PrivateLink case)", "https://100.64.1.5", true, "100.64.1.5"},
		{"rule 4 exception: RFC1918 literal ACCEPTED", "https://10.40.1.5:8443", true, "10.40.1.5"},
		{"rule 5: equals the regional public host refused", "https://bedrock-runtime.us-east-1.amazonaws.com", false, ""},
		{"rule 6: query refused", "https://vpce-0abc.vpce.amazonaws.com?x=1", false, ""},
		{"rule 7: fragment refused", "https://vpce-0abc.vpce.amazonaws.com#x", false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ValidateBedrockBaseURL(c.raw, region, false)
			if c.ok && err != nil {
				t.Fatalf("expected valid, got error: %v", err)
			}
			if !c.ok {
				if err == nil {
					t.Fatalf("expected an error, got nil (out=%q)", got)
				}
				if got != "" {
					t.Fatalf("a refused value must return %q, got %q", "", got)
				}
				return
			}
			if h := gatewayHost(got); h != c.wantHost {
				t.Fatalf("gatewayHost(%q) = %q, want %q", got, h, c.wantHost)
			}
		})
	}
}

// TestValidateBedrockBaseURL_PlainHTTPOnlyWithTestEndpoints is R-01's pin: the
// kind SSO walk points WARDYN_BEDROCK_BASE_URL at a PLAIN-HTTP fake
// bedrock-runtime stub (test/awsssofake serves no TLS, and the bedrock_sso lane
// is SigV4 — no per-run TLS-MITM terminates for it), so rule 1's unconditional
// https:// refused the boot the walk depends on.
//
// The relaxation is gated by the SAME acknowledgement the AWS SSO endpoint hatch
// uses — two deliberate acts, never one env var — and the PRODUCTION rule is
// untouched: without WARDYN_ALLOW_TEST_ENDPOINTS, http:// still refuses boot,
// and the refusal names BOTH variables so the operator knows which one they
// meant. Every other rule applies identically in both postures.
func TestValidateBedrockBaseURL_PlainHTTPOnlyWithTestEndpoints(t *testing.T) {
	const region = "us-east-1"
	const fake = "http://wardyn-awsssofake.wardyn.svc.cluster.local:8090"

	// WITHOUT the acknowledgement: refused, fail closed, naming both vars.
	got, err := ValidateBedrockBaseURL(fake, region, false)
	if err == nil {
		t.Fatalf("plain http with allowTestEndpoints=false returned %q, nil — want a refusal", got)
	}
	if got != "" {
		t.Errorf("a refused value returned %q, want \"\" (fail closed)", got)
	}
	for _, name := range []string{"WARDYN_BEDROCK_BASE_URL", "WARDYN_ALLOW_TEST_ENDPOINTS"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("refusal %q does not name %s — the operator cannot tell which knob to change", err, name)
		}
	}

	// WITH it: accepted and normalized, scheme preserved.
	got, err = ValidateBedrockBaseURL(fake, region, true)
	if err != nil {
		t.Fatalf("plain http with allowTestEndpoints=true errored: %v", err)
	}
	if got != fake {
		t.Errorf("= %q, want %q", got, fake)
	}
	if h := gatewayHost(got); h != "wardyn-awsssofake.wardyn.svc.cluster.local" {
		t.Errorf("gatewayHost(%q) = %q", got, h)
	}

	// https is unaffected by the acknowledgement, in either direction.
	const vpce = "https://vpce-0abc.vpce.amazonaws.com"
	for _, allow := range []bool{false, true} {
		if g, e := ValidateBedrockBaseURL(vpce, region, allow); e != nil || g != vpce {
			t.Errorf("ValidateBedrockBaseURL(https, allow=%v) = %q, %v — want it unchanged", allow, g, e)
		}
	}

	// The acknowledgement relaxes rule 1 ONLY. Everything else still refuses.
	for _, raw := range []string{
		"http://u:p@host:8090",                           // rule 2: userinfo
		"http:///path",                                   // rule 3: empty host
		"http://169.254.169.254",                         // rule 4: metadata literal
		"http://bedrock-runtime.us-east-1.amazonaws.com", // rule 5: the public host itself
		"http://host:8090?x=1",                           // rule 6: query
		"http://host:8090#x",                             // rule 7: fragment
		"ftp://host",                                     // not http/https either
	} {
		if g, e := ValidateBedrockBaseURL(raw, region, true); e == nil {
			t.Errorf("ValidateBedrockBaseURL(%q, allow=true) = %q, nil — the acknowledgement must relax rule 1 only", raw, g)
		}
	}
}

// TestValidateModelProviders_BedrockHTTPNeedsTestHatch is T-13 (MP-9): a model
// provider's bedrock.base_url takes the boot knob's relaxation and no more.
// Plain http:// is refused at every door that writes one unless
// WARDYN_ALLOW_TEST_ENDPOINTS acknowledges a test deployment, and stored as
// written when it does — the kind SSO walk's fake bedrock-runtime serves no TLS.
// The MDM door is `wardyn site-config apply`: the file decoded strictly, as the
// CLI does, and sent through the SDK's PutSiteConfig.
func TestValidateModelProviders_BedrockHTTPNeedsTestHatch(t *testing.T) {
	const fakeURL = "http://wardyn-awsssofake.wardyn.svc.cluster.local:8090"
	p := ssoProvider()
	p.Bedrock.BaseURL = fakeURL
	block, err := json.Marshal(types.ModelProviders{Providers: []types.ModelProvider{p}})
	if err != nil {
		t.Fatal(err)
	}
	mdmFile := `{"model_providers":` + string(block) + `}`
	httpPut := func(path, body string) func(*Server) error {
		return func(srv *Server) error {
			if w := do(t, srv, http.MethodPut, path, adminToken, body); w.Code != http.StatusOK {
				return fmt.Errorf("PUT %s = %d: %s", path, w.Code, w.Body.String())
			}
			return nil
		}
	}
	doors := []struct {
		name string
		put  func(*Server) error
	}{
		{"PUT /site-config", httpPut("/api/v1/site-config", mdmFile)},
		{"PUT /model-providers", httpPut("/api/v1/model-providers", string(block))},
		{"MDM apply", func(srv *Server) error {
			var cfg types.SiteConfig
			dec := json.NewDecoder(strings.NewReader(mdmFile))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&cfg); err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(srv.Handler())
			defer ts.Close()
			_, _, _, err := client.New(ts.URL, adminToken).PutSiteConfig(context.Background(), cfg)
			return err
		}},
	}
	for _, door := range doors {
		for _, allow := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/allow_test_endpoints=%v", door.name, allow), func(t *testing.T) {
				store := &fakeSiteConfigStore{}
				cfg := baseTestConfig(newHarness(t), store)
				cfg.AllowTestEndpoints = allow
				err := door.put(New(cfg))
				if !allow {
					if err == nil || !strings.Contains(err.Error(), `bedrock.base_url: must be https://`) {
						t.Fatalf("plain http:// without the test hatch: err = %v, want the bedrock.base_url https refusal", err)
					}
					if store.putSeen != nil {
						t.Fatalf("a refused write stored %+v", store.putSeen.ModelProviders)
					}
					return
				}
				if err != nil {
					t.Fatalf("plain http:// under WARDYN_ALLOW_TEST_ENDPOINTS refused: %v", err)
				}
				if store.putSeen == nil || store.putSeen.ModelProviders == nil ||
					store.putSeen.ModelProviders.Providers[0].Bedrock.BaseURL != fakeURL {
					t.Fatalf("stored = %+v, want the provider with base_url %s", store.putSeen, fakeURL)
				}
			})
		}
	}

	// The hatch relaxes the scheme alone, here as at boot.
	for _, raw := range []string{"http://u:p@host:8090", "http://169.254.169.254", "http://host:8090?x=1"} {
		q := ssoProvider()
		q.Bedrock.BaseURL = raw
		if err := validateModelProviders(providerBlock(q), true); err == nil {
			t.Errorf("bedrock.base_url %q accepted under the test hatch — it must relax the scheme only", raw)
		}
	}
}

// TestLLMProviderFor_GatewayAuthOverride pins the seam ValidateLLMGateways'
// header/format feeds: (*Server).llmProviderFor applies Config.LLMGatewayAuth
// field-by-field onto the harness catalog's compile-time convention, keyed by
// the VENDOR host (not the gateway's, so the lookup survives a host
// substitution happening in the same call). Unset is byte-identical to
// today's vendor convention; each field is independently overridable.
func TestLLMProviderFor_GatewayAuthOverride(t *testing.T) {
	s := &Server{}
	p, ok := s.llmProviderFor("claude-code")
	if !ok {
		t.Fatal("claude-code must resolve to a provider")
	}
	if p.header != "x-api-key" || p.format != "%s" {
		t.Fatalf("unset: header=%q format=%q, want the vendor default x-api-key/%%s", p.header, p.format)
	}

	// Header alone overridden: format keeps the vendor default.
	s.cfg.LLMGatewayAuth = map[string]LLMGatewayAuth{"api.anthropic.com": {Header: "x-gw-key"}}
	p, _ = s.llmProviderFor("claude-code")
	if p.header != "x-gw-key" || p.format != "%s" {
		t.Fatalf("header-only override: header=%q format=%q, want x-gw-key/%%s", p.header, p.format)
	}

	// Both overridden, alongside a gateway host override: the auth lookup key
	// stays the ORIGINAL vendor host, not the gateway's.
	s.cfg.LLMGateways = map[string]string{"api.anthropic.com": "https://llm-gateway.corp.internal"}
	s.cfg.LLMGatewayAuth = map[string]LLMGatewayAuth{"api.anthropic.com": {Header: "x-gw-key", Format: "Token %s"}}
	p, _ = s.llmProviderFor("claude-code")
	if p.host != "llm-gateway.corp.internal" {
		t.Fatalf("host = %q, want the gateway host", p.host)
	}
	if p.header != "x-gw-key" || p.format != "Token %s" {
		t.Fatalf("both overridden: header=%q format=%q, want x-gw-key/Token %%s", p.header, p.format)
	}

	// OpenAI is unaffected by an Anthropic-only override.
	pOpenAI, _ := s.llmProviderFor("codex-cli")
	if pOpenAI.header != "Authorization" || pOpenAI.format != "Bearer %s" {
		t.Fatalf("openai must keep its own vendor default, got header=%q format=%q", pOpenAI.header, pOpenAI.format)
	}
}
