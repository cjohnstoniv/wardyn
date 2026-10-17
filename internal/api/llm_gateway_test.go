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

// TestValidateOneLLMGateway exercises the seven rules a model provider's
// base URL (validateModelProviders) must pass.
func TestValidateOneLLMGateway(t *testing.T) {
	cases := []struct {
		name       string
		publicHost string
		raw        string
		wantHost   string // "" = refused
	}{
		{"good anthropic gateway", "api.anthropic.com", "https://llm-gateway.corp.internal", "llm-gateway.corp.internal"},
		{"good anthropic gateway, RFC1918 literal", "api.anthropic.com", "https://10.40.1.5:8443/v1", "10.40.1.5"},
		{"good openai gateway", "api.openai.com", "https://oai-gateway.corp.internal", "oai-gateway.corp.internal"},
		{"rule 1: http refused", "api.anthropic.com", "http://llm-gateway.corp.internal", ""},
		{"rule 2: userinfo refused", "api.anthropic.com", "https://user:pass@llm-gateway.corp.internal", ""},
		{"rule 3: empty host refused", "api.anthropic.com", "https:///path", ""},
		{"rule 4: loopback literal refused", "api.anthropic.com", "https://127.0.0.1", ""},
		{"rule 4: link-local literal refused", "api.anthropic.com", "https://169.254.1.1", ""},
		{"rule 4: metadata literal refused", "api.anthropic.com", "https://169.254.169.254", ""},
		{"rule 4: unspecified literal refused", "api.anthropic.com", "https://0.0.0.0", ""},
		{"rule 4: multicast literal refused", "api.anthropic.com", "https://224.0.0.1", ""},
		{"rule 4: nat64-embedded refused", "api.anthropic.com", "https://[64:ff9b::a9fe:a9fe]", ""},
		{"rule 4 exception: RFC1918 literal allowed", "api.anthropic.com", "https://10.0.0.5", "10.0.0.5"},
		{"rule 4 exception: CGNAT literal allowed", "api.anthropic.com", "https://100.64.0.5", "100.64.0.5"},
		{"rule 5: equals the public host refused", "api.anthropic.com", "https://api.anthropic.com", ""},
		{"rule 5 case-insensitive: equals the public host refused", "api.anthropic.com", "https://API.ANTHROPIC.COM", ""},
		{"rule 5 trailing-dot: equals the public host refused", "api.anthropic.com", "https://api.anthropic.com.", ""},
		{"rule 6: query refused", "api.anthropic.com", "https://llm-gateway.corp.internal?x=1", ""},
		{"rule 7: fragment refused", "api.anthropic.com", "https://llm-gateway.corp.internal#x", ""},
		{"malformed URL refused", "api.anthropic.com", "https://[::", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := validateOneLLMGateway(c.publicHost, c.raw, false)
			if c.wantHost == "" {
				if err == nil {
					t.Fatalf("expected an error, got nil (out=%q)", out)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected valid, got error: %v", err)
			}
			if h := gatewayHost(out); h != c.wantHost {
				t.Fatalf("gatewayHost(%q) = %q, want %q", out, h, c.wantHost)
			}
		})
	}
}

// TestValidateOneLLMGateway_PathPrefixAndPortPreserved: the normalized base URL
// keeps a non-default port and a path prefix — the proxy's LLMUpstreams
// wiring depends on both surviving validation intact.
func TestValidateOneLLMGateway_PathPrefixAndPortPreserved(t *testing.T) {
	got, err := validateOneLLMGateway("api.anthropic.com", "https://llm-gateway.corp.internal:8443/v1/", false)
	if err != nil {
		t.Fatalf("validateOneLLMGateway: %v", err)
	}
	if want := "https://llm-gateway.corp.internal:8443/v1"; got != want {
		t.Fatalf("got %q, want %q (one trailing slash trimmed, port+prefix preserved)", got, want)
	}
}

// TestValidateModelProviders_BedrockHTTPNeedsTestHatch is T-13 (MP-9): a model
// provider's bedrock.base_url takes the test hatch's relaxation and no more.
// Plain http:// is refused at every door that writes one unless
// WARDYN_ALLOW_TEST_ENDPOINTS acknowledges a test deployment, and stored as
// written when it does — the kind SSO walk's fake bedrock-runtime serves no TLS.
// The MDM door is `wardyn site-config set`: the file decoded strictly, as the
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
			ts := httptest.NewServer(panicFails(t, srv.Handler()))
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

	// The hatch relaxes the scheme alone.
	for _, raw := range []string{"http://u:p@host:8090", "http://169.254.169.254", "http://host:8090?x=1"} {
		q := ssoProvider()
		q.Bedrock.BaseURL = raw
		if _, err := validateModelProviders(providerBlock(q), providerWriteEnv{AllowTestEndpoints: true}); err == nil {
			t.Errorf("bedrock.base_url %q accepted under the test hatch — it must relax the scheme only", raw)
		}
	}
}
