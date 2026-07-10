// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newLocalRouteProxy builds a Proxy wired for the brokered local routes: its
// dialer always lands on upstreamAddr (the fake control plane / LLM upstream),
// the resolver maps every host to a public IP (so the SSRF guard passes), and
// the control-plane URL + run token are set so the run token is injected. A
// non-nil tlsCfg trusts an httptest TLS server standing in for the HTTPS LLM
// upstream.
func newLocalRouteProxy(t *testing.T, controlPlaneURL, runToken, upstreamAddr string, inj *injector, tlsCfg *tls.Config) (*Proxy, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Injector:        inj,
		Sink:            sink,
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr),
		ControlPlaneURL: controlPlaneURL,
		RunToken:        runToken,
		TLSClientConfig: tlsCfg,
	})
	return p, buf
}

// mustLocalReq builds an origin-form (path-only) request to the proxy listener
// itself — exactly what the sandbox sends for a brokered local route.
func mustLocalReq(t *testing.T, method, path string, body io.Reader) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	req.URL.Host = "" // origin-form: no authority
	req.URL.Scheme = ""
	req.RequestURI = path
	return req
}

// TestLocalRouteForwardsRunTokenAndBody covers the three brokered-forward
// shapes (mint, approval lookup, recording upload): each strips the
// sandbox's smuggled Authorization, injects the run token, forwards the
// request path/body verbatim to the control plane, and passes its response
// straight back.
func TestLocalRouteForwardsRunTokenAndBody(t *testing.T) {
	apID := uuid.New()
	runID := uuid.New()

	cases := []struct {
		name           string
		method         string
		route          string
		body           string
		wantCPPath     string
		wantStatus     int
		respBody       string
		wantRuleSource string
	}{
		{
			name:           "mint",
			method:         http.MethodPost,
			route:          routeMint,
			body:           `{"grant_id":"` + uuid.New().String() + `"}`,
			wantCPPath:     "/api/v1/internal/credentials/mint",
			wantStatus:     http.StatusOK,
			respBody:       `{"kind":"github_token","token":"minted","jti":"j"}`,
			wantRuleSource: ruleSourceMint,
		},
		{
			name:           "approval",
			method:         http.MethodGet,
			route:          routeApprovals + apID.String(),
			wantCPPath:     "/api/v1/internal/approvals/" + apID.String(),
			wantStatus:     http.StatusOK,
			respBody:       `{"id":"` + apID.String() + `"}`,
			wantRuleSource: ruleSourceApprovals,
		},
		{
			name:           "recording",
			method:         http.MethodPut,
			route:          routeRecordings + runID.String(),
			body:           `{"version":2}`,
			wantCPPath:     "/api/v1/internal/recordings/" + runID.String(),
			wantStatus:     http.StatusNoContent,
			wantRuleSource: ruleSourceRecordings,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth, gotPath, gotBody string
			cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAuth = r.Header.Get("Authorization")
				gotPath = r.URL.Path
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
				w.WriteHeader(tc.wantStatus)
				if tc.respBody != "" {
					_, _ = io.WriteString(w, tc.respBody)
				}
			}))
			defer cp.Close()

			p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)

			var reqBody io.Reader
			if tc.body != "" {
				reqBody = strings.NewReader(tc.body)
			}
			rec := httptest.NewRecorder()
			req := mustLocalReq(t, tc.method, tc.route, reqBody)
			// Sandbox tries to smuggle its own Authorization: it MUST be stripped.
			req.Header.Set("Authorization", "Bearer SANDBOX-SMUGGLED")
			p.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
			}
			if tc.respBody != "" && rec.Body.String() != tc.respBody {
				t.Fatalf("body not passed through verbatim: %q", rec.Body.String())
			}
			if gotAuth != "Bearer RUNTOK" {
				t.Fatalf("control plane Authorization = %q, want Bearer RUNTOK (run token injected, sandbox stripped)", gotAuth)
			}
			if gotPath != tc.wantCPPath {
				t.Fatalf("forwarded path = %q, want %q", gotPath, tc.wantCPPath)
			}
			if tc.body != "" && gotBody != tc.body {
				t.Fatalf("forwarded body = %q, want %q", gotBody, tc.body)
			}
			if d := lastDecision(t, buf); d.RuleSource != tc.wantRuleSource || d.Decision != egress.Allow {
				t.Fatalf("decision = %+v, want %s allow", d, tc.wantRuleSource)
			}
		})
	}
}

func TestLocalMintPassesThrough409WithApprovalID(t *testing.T) {
	apID := uuid.New()
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{"approval_id": apID.String()})
	}))
	defer cp.Close()

	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, routeMint, strings.NewReader(`{"grant_id":"`+uuid.New().String()+`"}`))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 passed through verbatim", rec.Code)
	}
	var m struct {
		ApprovalID string `json:"approval_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode 409 body: %v (%q)", err, rec.Body.String())
	}
	if m.ApprovalID != apID.String() {
		t.Fatalf("approval_id = %q, want %q", m.ApprovalID, apID)
	}
	d := lastDecision(t, buf)
	if d.RuleSource != ruleSourceMint {
		t.Fatalf("rule_source = %q", d.RuleSource)
	}
	if d.ApprovalID == nil || *d.ApprovalID != apID {
		t.Fatalf("decision log approval_id = %v, want %v", d.ApprovalID, apID)
	}
}

func TestAbsoluteURIWritPathDoesNotReachLocalRoutes(t *testing.T) {
	// An absolute-URI forward request for the proxy's own /wardyn/... path must
	// go through normal policy evaluation. wardyn-proxy is not allowlisted, so
	// it is denied — the control plane is NEVER contacted with the run token.
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("control plane must NOT be reached via absolute-URI /wardyn path")
	}))
	defer cp.Close()

	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)

	rec := httptest.NewRecorder()
	// Absolute-URI request addressed to the proxy host itself.
	req := mustProxyReq(t, http.MethodPost, "http://wardyn-proxy:3128"+routeMint)
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("absolute-URI /wardyn path status = %d, want 403 (policy deny)", rec.Code)
	}
	if d := lastDecision(t, buf); d.Decision != egress.Deny {
		t.Fatalf("decision = %q, want deny (policy default-deny on wardyn-proxy)", d.Decision)
	}
	if strings.HasPrefix(lastDecision(t, buf).RuleSource, "brokered:") {
		t.Fatalf("absolute-URI request must NOT emit a brokered decision")
	}
}

func TestLocalLLMNoRuleReturns404(t *testing.T) {
	// No injector configured -> no Anthropic credential brokered -> 404.
	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", "127.0.0.1:1", nil, nil)

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages", strings.NewReader(`{}`))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when no LLM credential", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "no_llm_credential") {
		t.Fatalf("body = %q, want JSON explaining no LLM credential", rec.Body.String())
	}
	if d := lastDecision(t, buf); d.RuleSource != ruleSourceLLM || d.Decision != egress.Deny {
		t.Fatalf("decision = %+v, want brokered:llm deny", d)
	}
}

func TestLocalLLMInjectsAPIKeyAndStripsSandboxAuth(t *testing.T) {
	var gotAuthz, gotXKey, gotHost, gotPath, gotBody string
	// HTTPS upstream — the LLM route always dials https://api.anthropic.com.
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthz = r.Header.Get("Authorization")
		gotXKey = r.Header.Get("X-Api-Key")
		gotHost = r.Host
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, "llm-ok")
	}))
	defer upstream.Close()

	// Startup-minted Anthropic injection credential (same mechanism the
	// forward-proxy path uses) — an x-api-key header for api.anthropic.com.
	inj := staticInj(map[string]injectedHeader{
		anthropicHost: {name: "X-Api-Key", value: "BROKERED-KEY"},
	})
	// Trust the httptest TLS cert (it is not issued for api.anthropic.com).
	tlsCfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only seam
	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(upstream), inj, tlsCfg)

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages", strings.NewReader(`{"hi":1}`))
	req.Header.Set("Authorization", "Bearer SANDBOX-SMUGGLED")
	req.Header.Set("Content-Type", "application/json")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "llm-ok" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if gotXKey != "BROKERED-KEY" {
		t.Fatalf("upstream X-Api-Key = %q, want BROKERED-KEY (injected)", gotXKey)
	}
	if gotAuthz != "" {
		t.Fatalf("sandbox Authorization must be stripped, upstream saw %q", gotAuthz)
	}
	if gotHost != anthropicHost {
		t.Fatalf("upstream Host = %q, want %q", gotHost, anthropicHost)
	}
	if gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %q, want /v1/messages", gotPath)
	}
	if gotBody != `{"hi":1}` {
		t.Fatalf("upstream body = %q, want streamed verbatim", gotBody)
	}
	if d := lastDecision(t, buf); d.RuleSource != ruleSourceLLM || d.Decision != egress.Allow {
		t.Fatalf("decision = %+v, want brokered:llm allow", d)
	}
}

func TestLocalUnknownWritPath404(t *testing.T) {
	p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", "127.0.0.1:1", nil, nil)
	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodGet, "/wardyn/v1/bogus", nil)
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown brokered route status = %d, want 404", rec.Code)
	}
}

// TestLocalRouteRejectsBadPathSegment covers non-UUID and traversal-shaped
// path segments for both the recording and approval brokered routes: none
// of them may reach the forward to the control plane.
func TestLocalRouteRejectsBadPathSegment(t *testing.T) {
	cases := []struct {
		name   string
		method string
		route  string
	}{
		{"recording-dotdot", http.MethodPut, routeRecordings + ".."},
		{"recording-nonuuid", http.MethodPut, routeRecordings + "x"},
		{"recording-traversal", http.MethodPut, routeRecordings + "../decisions"},
		{"recording-nested", http.MethodPut, routeRecordings + uuid.New().String() + "/extra"},
		{"approval-nested", http.MethodGet, routeApprovals + "abc/extra"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cpCalled := false
			cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cpCalled = true }))
			defer cp.Close()
			p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)

			var body io.Reader
			if tc.method == http.MethodPut {
				body = bytes.NewReader([]byte("x"))
			}
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, tc.method, tc.route, body))
			if rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", rec.Code)
			}
			if cpCalled {
				t.Fatal("control plane must never be contacted for an invalid path segment")
			}
		})
	}
}
