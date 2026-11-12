// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// With an allowed-models list set, every way a hosted provider kind's model request reaches the proxy is
// checked: each host the kind's dispatch intercepts is terminated (the CONNECT branch handleConnect takes)
// and its request refused for an unlisted model; a kind that goes through the brokered route is refused
// there. The proxy options are the shape each kind's dispatch writes (internal/api resolveLLMInjections:
// mitm_llm and the per-run CA with a list set, its own interception entries, channel hosts, upstreams and
// re-signed hosts). A host none of these covers is refused as an opaque tunnel
// (TestAllowedModels_OpaqueModelTunnelRefused), so no kind's request reaches a provider unchecked.
func TestAllowedModels_EveryProviderKindChecked(t *testing.T) {
	const (
		gateway   = "llm-gateway.corp.internal"
		routeThru = "claude-route.corp.internal"
		foundry   = "foundry.corp.internal"
		runtime   = "bedrock-runtime.us-west-2.amazonaws.com"
		vpce      = "vpce-1.bedrock-runtime.us-west-2.vpce.amazonaws.com"
		bedrockGW = "bedrock-gateway.corp.example"
	)
	anthropicBody, openaiBody := `{"model":"model-b","messages":[]}`, `{"model":"model-b","input":"x"}`
	type direct struct{ host, path, body string }
	for _, tc := range []struct {
		kind     string
		opts     Options
		routes   map[string]string // Config.BedrockRoutes
		brokered string            // a brokered route the kind's harness is pointed at, or ""
		body     string
		direct   []direct // hosts the kind's sandbox connects to itself
	}{
		{kind: "anthropic_api_key", opts: Options{MITMLLM: true}, brokered: llmAnthropicPrefix + "v1/messages", body: anthropicBody,
			direct: []direct{{anthropicHost, "/v1/messages", anthropicBody}}},
		{kind: "openai_api_key", opts: Options{MITMLLM: true}, brokered: llmOpenAIPrefix + "v1/responses", body: openaiBody,
			direct: []direct{{openaiHost, "/v1/responses", openaiBody}}},
		{kind: "custom_endpoint", opts: Options{MITMLLM: true, MITMHosts: []string{gateway + ":443"},
			LLMUpstreams: map[string]string{anthropicHost: "https://" + gateway + "/anthropic"}},
			brokered: llmAnthropicPrefix + "v1/messages", body: anthropicBody,
			direct: []direct{{gateway, "/anthropic/v1/messages", anthropicBody}, {anthropicHost, "/v1/messages", anthropicBody}}},
		{kind: "gateway", opts: Options{MITMLLM: true, MITMHosts: []string{gateway + ":443"},
			LLMUpstreams: map[string]string{openaiHost: "https://" + gateway + "/v1"}},
			brokered: llmOpenAIPrefix + "responses", body: openaiBody,
			direct: []direct{{gateway, "/v1/responses", openaiBody}, {openaiHost, "/v1/responses", openaiBody}}},
		{kind: "anthropic_subscription", opts: Options{MITMLLM: true, MITMHosts: []string{routeThru + ":443"},
			LLMChannelHosts: map[string]string{routeThru: anthropicHost}},
			direct: []direct{{anthropicHost, "/v1/messages", anthropicBody}, {routeThru, "/anthropic/v1/messages", anthropicBody}}},
		{kind: "bedrock_bearer", opts: Options{MITMLLM: true, MITMHosts: []string{vpce + ":443"}},
			direct: []direct{{vpce, "/model/model-b/invoke", `{}`}}},
		{kind: "bedrock_bearer at a gateway of its own name", opts: Options{MITMLLM: true, MITMHosts: []string{bedrockGW + ":443"}},
			routes: map[string]string{bedrockGW: "/bedrock"},
			direct: []direct{{bedrockGW, "/bedrock/model/model-b/invoke", `{}`}, {bedrockGW, "/bedrock/model/model-b/converse-stream", `{}`}}},
		{kind: "bedrock_sso re-signed at the proxy", opts: Options{MITMLLM: true,
			AWSSigV4: &AWSSigV4Config{GrantID: uuid.New(), Hosts: []string{runtime}}},
			direct: []direct{{runtime, "/model/model-b/converse", `{}`}}},
		{kind: "azure_foundry", opts: Options{MITMLLM: true, MITMHosts: []string{foundry + ":443"},
			LLMChannelHosts: map[string]string{foundry: anthropicHost}},
			direct: []direct{{foundry, "/anthropic/v1/messages", anthropicBody}}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			opts := tc.opts
			opts.RunID, opts.CA, opts.Resolver = uuid.New(), fakeCA(t), publicResolver{}
			opts.Policy = CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{anthropicHost, openaiHost, gateway, routeThru, foundry, runtime, vpce, bedrockGW}})
			p := newProxy(opts)
			p.allowedModels = compileAllowedModels(&[]string{"model-a"}, tc.routes)
			for _, d := range tc.direct {
				if !(p.mitmLLMHost(d.host, 443) || (p.ca != nil && p.isCorpMITMHost(d.host) && p.mitmPortAllowed(d.host, 443))) {
					t.Fatalf("%s: a connection to it is not terminated, so the gate cannot read it", d.host)
				}
				rec := httptest.NewRecorder()
				p.serveMITMRequest(rec, httptest.NewRequest(http.MethodPost, "https://"+d.host+d.path, strings.NewReader(d.body)), d.host, 443)
				if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `model \"model-b\" is not on`) {
					t.Fatalf("%s: status = %d body = %q, want model-b refused", d.host, rec.Code, rec.Body.String())
				}
			}
			if tc.brokered == "" {
				return
			}
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, tc.brokered, strings.NewReader(tc.body)))
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `model \"model-b\" is not on`) {
				t.Fatalf("brokered %s: status = %d body = %q, want model-b refused", tc.brokered, rec.Code, rec.Body.String())
			}
		})
	}
}

// A Bedrock gateway named by its own address is read by its configured route alone: under the base path
// a listed model passes and an unlisted one is refused before anything is sent; a write outside the base
// path is refused as unchecked, a read there passes. Without the route the host is no model host, which
// is why dispatch carries it.
func TestAllowedModels_BedrockGatewayRoute(t *testing.T) {
	const gw = "bedrock-gateway.corp.example"
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{gw}}), Resolver: publicResolver{}})
	p.allowedModels = compileAllowedModels(&[]string{"model-a"}, map[string]string{"Bedrock-Gateway.corp.example": "/bedrock"})
	for _, tc := range []struct {
		method, path string
		refused      bool
	}{
		{http.MethodPost, "/bedrock/model/model-a/invoke", false},
		{http.MethodPost, "/bedrock/model/model-a/invoke-with-response-stream", false},
		{http.MethodPost, "/bedrock/model/model-b/converse", true},
		{http.MethodPost, "/bedrock/model/model-b/converse-stream", true},
		{http.MethodPost, "/model/model-a/invoke", true},
		{http.MethodPost, "/bedrockx/model/model-a/invoke", true},
		{http.MethodGet, "/health", false},
	} {
		rec := httptest.NewRecorder()
		release, refused := p.allowedModelRefused(rec, httptest.NewRequest(tc.method, "https://"+gw+tc.path, strings.NewReader(`{}`)), gw, 443, tc.path)
		release()
		if refused != tc.refused || (refused && rec.Code != http.StatusForbidden) {
			t.Errorf("%s %s: refused = %v (%d %q), want %v", tc.method, tc.path, refused, rec.Code, rec.Body.String(), tc.refused)
		}
	}
	bare := newProxy(Options{RunID: uuid.New(), Resolver: publicResolver{}})
	bare.allowedModels = modelSet([]string{"model-a"})
	if bare.isLLMHost(gw) {
		t.Fatal("a gateway host with no route is a model host by DNS alone: the route test proves nothing")
	}
	for name, routes := range map[string]map[string]string{
		"a port in the key":     {gw + ":443": ""},
		"a relative base":       {gw: "bedrock"},
		"a trailing slash":      {gw: "/bedrock/"},
		"an escape in the base": {gw: "/bed%2Frock"},
		"a dot segment":         {gw: "/a/../bedrock"},
	} {
		if err := (&Config{BedrockRoutes: routes}).validateBedrockRoutes(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (&Config{BedrockRoutes: map[string]string{gw: "/bedrock", "vpce-1.bedrock-runtime.us-west-2.vpce.amazonaws.com": ""}}).validateBedrockRoutes(); err != nil {
		t.Errorf("good routes refused: %v", err)
	}
}
