// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// rawUpstream records the request line exactly as the upstream received it.
type rawUpstream struct {
	srv        *httptest.Server
	reached    bool
	method     string
	requestURI string
	header     http.Header
	body       string
}

func newRawUpstream(t *testing.T, respBody string) *rawUpstream {
	t.Helper()
	u := &rawUpstream{}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.reached = true
		u.method = r.Method
		u.requestURI = r.RequestURI
		u.header = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		u.body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

const llmTestGatewayHost = "llm-gateway.corp.internal"

// llmChecksProxy builds a proxy with both brokered vendor credentials, the
// given policy, and (when gatewayBase is set) an Anthropic gateway.
func llmChecksProxy(t *testing.T, spec types.RunPolicySpec, gatewayBase string, up *rawUpstream) (*Proxy, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	opts := Options{
		RunID:  uuid.New(),
		Policy: CompilePolicy(spec),
		Sink:   &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)},
		Resolver: fakeResolver{m: map[string][]net.IP{
			anthropicHost: ips("93.184.216.34"), openaiHost: ips("93.184.216.35"), llmTestGatewayHost: ips("10.40.1.5"),
		}},
		Dial: redirectDial(upstreamAddr(up.srv)),
		Injector: staticInj(map[string]injectedHeader{
			anthropicHost:      {name: "X-Api-Key", value: "BROKERED-KEY"},
			openaiHost:         {name: "Authorization", value: "Bearer BROKERED-OPENAI"},
			llmTestGatewayHost: {name: "X-Api-Key", value: "GATEWAY-KEY"},
		}),
		TLSClientConfig: testInsecureTLSConfig,
	}
	if gatewayBase != "" {
		opts.LLMUpstreams = map[string]string{anthropicHost: gatewayBase}
	}
	return newProxy(opts), buf
}

// TestLLMRouteForwardsModelCallsUnchanged pins the brokered model calls the
// harnesses make: the upstream receives the same method, request target and
// body, with the brokered credential, and the response comes back as sent.
// Each call is made with no method restriction and under ["GET","POST"].
func TestLLMRouteForwardsModelCallsUnchanged(t *testing.T) {
	const gateway = "https://" + llmTestGatewayHost + "/llm/anthropic"
	cases := []struct {
		name, gateway, method, target, body string
		wantURI, hdr, hdrValue              string
	}{
		{"anthropic_messages", "", http.MethodPost, llmAnthropicPrefix + "v1/messages", `{"model":"m","max_tokens":1}`,
			"/v1/messages", "X-Api-Key", "BROKERED-KEY"},
		{"anthropic_models", "", http.MethodGet, llmAnthropicPrefix + "v1/models?limit=5", "",
			"/v1/models?limit=5", "X-Api-Key", "BROKERED-KEY"},
		{"openai_responses", "", http.MethodPost, llmOpenAIPrefix + "v1/responses", `{"model":"m","input":"x"}`,
			"/v1/responses", "Authorization", "Bearer BROKERED-OPENAI"},
		{"openai_models", "", http.MethodGet, llmOpenAIPrefix + "v1/models", "",
			"/v1/models", "Authorization", "Bearer BROKERED-OPENAI"},
		{"gateway_messages", gateway, http.MethodPost, llmAnthropicPrefix + "v1/messages", `{"model":"m"}`,
			"/llm/anthropic/v1/messages", "X-Api-Key", "GATEWAY-KEY"},
		{"gateway_models", gateway, http.MethodGet, llmAnthropicPrefix + "v1/models", "",
			"/llm/anthropic/v1/models", "X-Api-Key", "GATEWAY-KEY"},
		// Dots inside a segment are ordinary names, not dot-segments.
		{"dotted_model_id", "", http.MethodGet, llmAnthropicPrefix + "v1/models/claude-3.5-sonnet", "",
			"/v1/models/claude-3.5-sonnet", "X-Api-Key", "BROKERED-KEY"},
		{"dotted_names", gateway, http.MethodGet, llmAnthropicPrefix + "v1/.well-known/a..b/...", "",
			"/llm/anthropic/v1/.well-known/a..b/...", "X-Api-Key", "GATEWAY-KEY"},
		{"semicolon_names", "", http.MethodGet, llmAnthropicPrefix + "v1/files/a;b/...;x", "",
			"/v1/files/a;b/...;x", "X-Api-Key", "BROKERED-KEY"},
	}
	for _, methods := range [][]string{nil, {"GET", "POST"}} {
		for _, tc := range cases {
			t.Run(strings.Join(methods, "_")+"/"+tc.name, func(t *testing.T) {
				up := newRawUpstream(t, `{"id":"resp-1"}`)
				p, buf := llmChecksProxy(t, types.RunPolicySpec{AllowedMethods: methods}, tc.gateway, up)
				rec := httptest.NewRecorder()
				req := mustLocalReq(t, tc.method, tc.target, strings.NewReader(tc.body))
				req.Header.Set("Content-Type", "application/json")
				p.ServeHTTP(rec, req)

				if rec.Code != http.StatusOK || rec.Body.String() != `{"id":"resp-1"}` {
					t.Fatalf("status=%d body=%q, want 200 and the upstream body", rec.Code, rec.Body.String())
				}
				if up.method != tc.method || up.requestURI != tc.wantURI || up.body != tc.body {
					t.Fatalf("upstream got %s %q body=%q, want %s %q body=%q",
						up.method, up.requestURI, up.body, tc.method, tc.wantURI, tc.body)
				}
				if got := up.header.Get(tc.hdr); got != tc.hdrValue {
					t.Fatalf("upstream %s = %q, want %q", tc.hdr, got, tc.hdrValue)
				}
				if d := lastDecision(t, buf); d.Decision != egress.Allow || d.RuleSource != ruleSourceLLM {
					t.Fatalf("decision = %s/%s, want allow/%s", d.Decision, d.RuleSource, ruleSourceLLM)
				}
			})
		}
	}
}

// TestLLMRouteAnyMethodWithoutRestriction: with no allowed_methods every
// method still reaches the upstream on the brokered route.
func TestLLMRouteAnyMethodWithoutRestriction(t *testing.T) {
	up := newRawUpstream(t, `{}`)
	p, _ := llmChecksProxy(t, types.RunPolicySpec{}, "", up)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodDelete, llmOpenAIPrefix+"v1/files/file-abc", nil))
	if rec.Code != http.StatusOK || !up.reached || up.method != http.MethodDelete {
		t.Fatalf("status=%d reached=%v method=%q, want the DELETE forwarded", rec.Code, up.reached, up.method)
	}
}

// TestLLMRouteHonoursAllowedMethods: a method outside allowed_methods is
// refused on the brokered route with the forward path's 403 and a
// policy:method decision, and the upstream is never contacted.
func TestLLMRouteHonoursAllowedMethods(t *testing.T) {
	spec := types.RunPolicySpec{AllowedDomains: []string{openaiHost, anthropicHost}, AllowedMethods: []string{"GET", "POST"}}
	for _, tc := range []struct{ name, gateway, method, target string }{
		{"openai_delete", "", http.MethodDelete, llmOpenAIPrefix + "v1/files/file-abc"},
		{"openai_patch", "", http.MethodPatch, llmOpenAIPrefix + "v1/fine_tuning/jobs/x"},
		{"anthropic_put", "", http.MethodPut, llmAnthropicPrefix + "v1/files/f"},
		{"gateway_delete", "https://" + llmTestGatewayHost + "/llm/anthropic", http.MethodDelete, llmAnthropicPrefix + "v1/files/f"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newRawUpstream(t, `{}`)
			p, buf := llmChecksProxy(t, spec, tc.gateway, up)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, tc.method, tc.target, nil))

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if up.reached {
				t.Fatal("a method outside allowed_methods reached the upstream")
			}
			if d := lastDecision(t, buf); d.Decision != egress.Deny || d.RuleSource != "policy:method" {
				t.Fatalf("decision = %s/%s, want deny/policy:method", d.Decision, d.RuleSource)
			}
			if strings.Contains(buf.String(), `"decision":"allow"`) {
				t.Fatalf("refused request was recorded as an allow:\n%s", buf.String())
			}
		})
	}

	// Same refusal as the forward path gives the same request to the same host.
	t.Run("matches_forward_path", func(t *testing.T) {
		up := newRawUpstream(t, `{}`)
		p, _ := llmChecksProxy(t, spec, "", up)
		brokered := httptest.NewRecorder()
		p.ServeHTTP(brokered, mustLocalReq(t, http.MethodDelete, llmOpenAIPrefix+"v1/files/file-abc", nil))
		forward := httptest.NewRecorder()
		p.ServeHTTP(forward, httptest.NewRequest(http.MethodDelete, "http://"+openaiHost+"/v1/files/file-abc", nil))

		if forward.Code != http.StatusForbidden || brokered.Code != forward.Code || brokered.Body.String() != forward.Body.String() {
			t.Fatalf("brokered %d %q, forward %d %q: want the same 403",
				brokered.Code, brokered.Body.String(), forward.Code, forward.Body.String())
		}
		for k := range forward.Header() {
			if got, want := brokered.Header().Get(k), forward.Header().Get(k); got != want {
				t.Errorf("header %s: brokered %q, forward %q", k, got, want)
			}
		}
	})
}

// TestLLMRouteRefusesDotSegments: a rest path with a "." or ".." segment
// (literal or percent-encoded) or a backslash is answered 400 and never
// forwarded, neither as sent nor normalised.
func TestLLMRouteRefusesDotSegments(t *testing.T) {
	const gateway = "https://" + llmTestGatewayHost + "/llm/anthropic"
	rests := []string{
		"../../admin/keys",
		"..",
		"v1/..",
		"v1/../admin",
		"./v1/messages",
		"v1/./messages",
		"%2e%2e/%2e%2e/admin/keys",
		"%2E%2E/admin",
		"%2e/v1/messages",
		".%2e/admin",
		"v1/%2E%2e/admin",
		"..%2fadmin",
		`..\admin`,
		"..%5cadmin",
		"%2e%2e%5Cadmin",
		`v1\messages`,
	}
	for _, route := range []struct{ name, gateway, prefix string }{
		{"gateway", gateway, llmAnthropicPrefix},
		{"anthropic", "", llmAnthropicPrefix},
		{"openai", "", llmOpenAIPrefix},
	} {
		for _, rest := range rests {
			for _, method := range []string{http.MethodGet, http.MethodPost} {
				t.Run(route.name+"/"+method+"/"+rest, func(t *testing.T) {
					assertLLMPathRefused(t, route.gateway, method, route.prefix+rest)
				})
			}
		}
	}

	// Over a real listener: the request line reaches the proxy as sent.
	t.Run("listener", func(t *testing.T) {
		up := newRawUpstream(t, `{}`)
		p, _ := llmChecksProxy(t, types.RunPolicySpec{}, gateway, up)
		srv := httptest.NewServer(p)
		defer srv.Close()
		resp, err := http.Get(srv.URL + llmAnthropicPrefix + "../../admin/keys")
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if up.reached || resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status=%d, upstream got %q: want 400 and no upstream request", resp.StatusCode, up.requestURI)
		}
	})
}

// assertLLMPathRefused sends target and asserts a 400 and a deny row, with
// nothing sent upstream.
func assertLLMPathRefused(t *testing.T, gateway, method, target string) {
	t.Helper()
	up := newRawUpstream(t, `{}`)
	p, buf := llmChecksProxy(t, types.RunPolicySpec{}, gateway, up)
	rec := httptest.NewRecorder()
	req := mustLocalReq(t, method, target, strings.NewReader(`{}`))
	p.ServeHTTP(rec, req)

	if up.reached {
		t.Fatalf("upstream received %s %q (key %q)", up.method, up.requestURI,
			up.header.Get("X-Api-Key")+up.header.Get("Authorization"))
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (decoded path %q)", rec.Code, req.URL.Path)
	}
	if d := lastDecision(t, buf); d.Decision != egress.Deny {
		t.Fatalf("decision = %s/%s, want deny", d.Decision, d.RuleSource)
	}
}

// TestLLMRouteRefusesParameterDotSegments: a segment that is "." or ".." up to
// its first ";" is refused like a plain dot-segment.
func TestLLMRouteRefusesParameterDotSegments(t *testing.T) {
	const gateway = "https://" + llmTestGatewayHost + "/llm/anthropic"
	for _, tc := range []struct{ gateway, target string }{
		{gateway, llmAnthropicPrefix + "..;/..;/admin"},
		{gateway, llmAnthropicPrefix + "v1/%2e%2e;x=1/admin"},
		{"", llmAnthropicPrefix + ".;x"},
		{"", llmOpenAIPrefix + "v1/..;a/files"},
	} {
		t.Run(tc.target, func(t *testing.T) { assertLLMPathRefused(t, tc.gateway, http.MethodGet, tc.target) })
	}
}

// TestLLMRouteRefusesControlCharacters: a decoded rest holding a byte below
// 0x20 or 0x7f is refused.
func TestLLMRouteRefusesControlCharacters(t *testing.T) {
	const gateway = "https://" + llmTestGatewayHost + "/llm/anthropic"
	for _, rest := range []string{"..%00/admin", "v1/messages%00", "v1/messages%0d%0aX:%20y", "v1/%09models", "v1/%1f", "v1/models%7f"} {
		t.Run(rest, func(t *testing.T) { assertLLMPathRefused(t, gateway, http.MethodPost, llmAnthropicPrefix+rest) })
	}
}

// TestPATBrokerRefusesParameterDotSegments: the git_pat broker refuses a
// segment that is ".." up to its first ";" before any scope check or mint,
// and still forwards the plain path on the same unrestricted grant.
func TestPATBrokerRefusesParameterDotSegments(t *testing.T) {
	const info = "/info/refs?service=git-upload-pack"
	for _, tc := range []struct {
		path     string
		upstream string
	}{
		{"team/app" + info, "/team/app/info/refs"},
		{"team/..;/app" + info, ""},
		{"team/.;x/app" + info, ""},
		{"..;/..;/team/app" + info, ""},
	} {
		t.Run(tc.path, func(t *testing.T) {
			p, up, last := patScopeProxy(t, "gitlab.com", PATGrant{Forge: "generic"})
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, "/wardyn/git/gitlab.com/"+tc.path, nil))
			if tc.upstream != "" {
				if rec.Code != http.StatusOK || up.gitPath != tc.upstream {
					t.Fatalf("status=%d forwarded %q, want 200 at %q", rec.Code, up.gitPath, tc.upstream)
				}
				return
			}
			if rec.Code != http.StatusForbidden || up.gitHits != 0 || up.mintCalls != 0 {
				t.Fatalf("status=%d forge requests=%d mints=%d, want 403 and none", rec.Code, up.gitHits, up.mintCalls)
			}
			if d := last(); d.Decision != egress.Deny || d.RuleSource != ruleSourcePATDenied {
				t.Fatalf("decision = %s/%s, want deny/%s", d.Decision, d.RuleSource, ruleSourcePATDenied)
			}
		})
	}
}
