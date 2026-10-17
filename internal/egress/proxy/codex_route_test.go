// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCodexBrokeredRoute_FinalUpstreamPath: codex is configured with a base URL
// (WARDYN_CODEX_BASE_URL, composed by codexBaseURL in internal/api) and appends
// `responses`. A decision-log row is allow-shaped whatever the upstream
// answers, so the claim worth pinning is the host and PATH the final upstream
// receives, with the brokered key on it and the sandbox's own stripped. The
// request paths below are the base URLs
// TestProviderDispatch_EndpointAndRouteThrough pins, plus `responses`.
func TestCodexBrokeredRoute_FinalUpstreamPath(t *testing.T) {
	const gw = "llm-gateway.corp.internal"
	for _, tc := range []struct {
		name     string
		upstream string // configured gateway base URL, "" for the public host
		reqPath  string
		wantPath string
	}{
		{"public host, /v1 on the base URL", "", llmOpenAIPrefix + "v1/responses", "/v1/responses"},
		{"gateway with a path prefix, no extra /v1", "https://" + gw + "/v1", llmOpenAIPrefix + "responses", "/v1/responses"},
		{"gateway with an empty path, /v1 on the base URL", "https://" + gw, llmOpenAIPrefix + "v1/responses", "/v1/responses"},
		// The 0.8.5 URL: today's base unchanged reaches the vendor as /responses.
		{"public host, the base URL 0.8.5 sent", "", llmOpenAIPrefix + "responses", "/responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := captureUpstream(t, true, `{"id":"resp_1"}`)
			host, opts := openaiHost, Options{}
			if tc.upstream != "" {
				host, opts.LLMUpstreams = gw, map[string]string{openaiHost: tc.upstream}
			}
			buf := &bytes.Buffer{}
			opts.RunID, opts.Policy = uuid.New(), CompilePolicy(types.RunPolicySpec{})
			opts.Sink = &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
			opts.Resolver = fakeResolver{m: map[string][]net.IP{gw: ips("10.40.1.5")}}
			if tc.upstream == "" {
				opts.Resolver = publicResolver{}
			}
			opts.Dial = redirectDial(upstreamAddr(up.srv))
			opts.Injector = staticInj(map[string]injectedHeader{host: {name: "Authorization", value: "Bearer BROKERED-KEY"}})
			opts.TLSClientConfig = testInsecureTLSConfig
			p := newProxy(opts)

			rec := httptest.NewRecorder()
			req := mustLocalReq(t, http.MethodPost, tc.reqPath, strings.NewReader(`{"model":"m","input":"hi"}`))
			req.Header.Set("Authorization", "Bearer wardyn-proxy-injected")
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK || !up.reached {
				t.Fatalf("status = %d reached = %v body=%q", rec.Code, up.reached, rec.Body.String())
			}
			if up.path != tc.wantPath {
				t.Errorf("upstream path = %q, want %q", up.path, tc.wantPath)
			}
			if up.host != host {
				t.Errorf("upstream host = %q, want %q", up.host, host)
			}
			if got := up.header.Get("Authorization"); got != "Bearer BROKERED-KEY" {
				t.Errorf("upstream Authorization = %q, want the brokered key", got)
			}
			if d := lastDecision(t, buf); d.Decision != egress.Allow || d.RuleSource != ruleSourceLLM {
				t.Errorf("decision = %+v, want an allow from %s", d, ruleSourceLLM)
			}
		})
	}
}
