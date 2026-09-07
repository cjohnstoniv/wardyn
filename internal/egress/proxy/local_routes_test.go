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
	"os"
	"regexp"
	"slices"
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
		RunToken:        newTokenSource(runToken),
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
			d := lastDecision(t, buf)
			if d.RuleSource != tc.wantRuleSource || d.Decision != egress.Allow {
				t.Fatalf("decision = %+v, want %s allow", d, tc.wantRuleSource)
			}
			// The brokered upstream is the control plane: audit records its REAL
			// host AND port (a zero/fabricated port would be dishonest).
			if d.Request.Host != "wardynd.test" || d.Request.Port != 8080 {
				t.Fatalf("decision target = %s:%d, want wardynd.test:8080", d.Request.Host, d.Request.Port)
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

// TestLocalLLMInjectsAPIKeyAndStripsSandboxAuth covers both brokered LLM
// providers: the proxy always strips the sandbox's smuggled credential and
// injects the broker-minted one, on whichever header the provider uses.
func TestLocalLLMInjectsAPIKeyAndStripsSandboxAuth(t *testing.T) {
	cases := []struct {
		name      string
		prefix    string
		host      string
		injHeader string
		injValue  string
		reqPath   string
		reqBody   string
	}{
		{
			// Startup-minted Anthropic injection credential (same mechanism the
			// forward-proxy path uses) — an x-api-key header for api.anthropic.com.
			name:      "anthropic",
			prefix:    llmAnthropicPrefix,
			host:      anthropicHost,
			injHeader: "X-Api-Key",
			injValue:  "BROKERED-KEY",
			reqPath:   "v1/messages",
			reqBody:   `{"hi":1}`,
		},
		{
			name:      "openai",
			prefix:    llmOpenAIPrefix,
			host:      openaiHost,
			injHeader: "Authorization",
			injValue:  "Bearer BROKERED-OPENAI",
			reqPath:   "v1/chat/completions",
			reqBody:   `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// HTTPS upstream — the LLM route always dials the provider's real host.
			cu := captureUpstream(t, true, "llm-ok")

			inj := staticInj(map[string]injectedHeader{
				tc.host: {name: tc.injHeader, value: tc.injValue},
			})
			p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cu.srv), inj, testInsecureTLSConfig)

			rec := httptest.NewRecorder()
			req := mustLocalReq(t, http.MethodPost, tc.prefix+tc.reqPath, strings.NewReader(tc.reqBody))
			req.Header.Set("Authorization", "Bearer SANDBOX-SMUGGLED")
			req.Header.Set("Content-Type", "application/json")
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
			}
			if rec.Body.String() != "llm-ok" {
				t.Fatalf("body = %q", rec.Body.String())
			}
			if got := cu.header.Get(tc.injHeader); got != tc.injValue {
				t.Fatalf("upstream %s = %q, want %q (injected)", tc.injHeader, got, tc.injValue)
			}
			// If the injected credential lands on its own header, the sandbox's
			// smuggled Authorization must be stripped outright; if the injected
			// credential IS Authorization, it must overwrite the smuggled one
			// (already asserted above).
			if tc.injHeader != "Authorization" {
				if got := cu.header.Get("Authorization"); got != "" {
					t.Fatalf("sandbox Authorization must be stripped, upstream saw %q", got)
				}
			}
			if cu.host != tc.host {
				t.Fatalf("upstream Host = %q, want %q", cu.host, tc.host)
			}
			if want := "/" + tc.reqPath; cu.path != want {
				t.Fatalf("upstream path = %q, want %q", cu.path, want)
			}
			if cu.body != tc.reqBody {
				t.Fatalf("upstream body = %q, want streamed verbatim", cu.body)
			}
			if d := lastDecision(t, buf); d.RuleSource != ruleSourceLLM || d.Decision != egress.Allow {
				t.Fatalf("decision = %+v, want brokered:llm allow", d)
			}
		})
	}
}

// TestLocalLLMDialFailEmitsDenyNotAllow pins the E3 fix extended to the
// brokered LLM tail (forwardInspectedLLM, bug-egress-1): a request to an
// allowed LLM route whose upstream RoundTrip FAILS must emit exactly one
// dial-failed Deny and NO allow decision — the allow is emitted only after a
// successful round-trip, so a failed dial can never over-report an allow.
func TestLocalLLMDialFailEmitsDenyNotAllow(t *testing.T) {
	inj := staticInj(map[string]injectedHeader{
		anthropicHost: {name: "X-Api-Key", value: "BROKERED-KEY"},
	})
	// "127.0.0.1:1" is the repo's dead-port convention: nothing listens there,
	// so the real RoundTrip fails.
	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", "127.0.0.1:1", inj, testInsecureTLSConfig)

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages", strings.NewReader(`{}`))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 on dial failure", rec.Code)
	}
	if d := findDecision(t, buf, "builtin:dial-failed"); d.Decision != egress.Deny {
		t.Fatalf("dial-failed decision = %q, want deny", d.Decision)
	}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var dl egress.DecisionLog
		if json.Unmarshal([]byte(line), &dl) == nil && dl.Decision == egress.Allow {
			t.Fatalf("a failed dial must NOT emit an allow decision; got %q", line)
		}
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

// TestLocalToolApprovalCreateForwardsSanitizedScope covers the sandbox-facing
// tool-hold raise: the run token is injected (the sandbox is tokenless), the
// smuggled Authorization is stripped, and the persisted requested_scope is the
// {tool, cmd, env} shape the approvals screen renders — with cmd clamped and
// env reduced to NAMES.
func TestLocalToolApprovalCreateForwardsSanitizedScope(t *testing.T) {
	apID := uuid.New()
	longCmd := strings.Repeat("x", maxToolCmd+500)

	var gotAuth, gotPath, gotBody string
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"`+apID.String()+`","state":"PENDING"}`)
	}))
	defer cp.Close()

	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)

	body, _ := json.Marshal(map[string]any{
		"kind": "tool_call",
		"payload": map[string]any{
			"tool": "Bash",
			"cmd":  longCmd,
			"env":  []string{"AWS_PROFILE", "HOME"},
		},
	})
	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, routeApprovalsCreate, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer SANDBOX-SMUGGLED")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if gotAuth != "Bearer RUNTOK" {
		t.Fatalf("control plane Authorization = %q, want the injected run token", gotAuth)
	}
	if gotPath != "/api/v1/internal/approvals" {
		t.Fatalf("forwarded path = %q", gotPath)
	}
	var fwd struct {
		Kind           string `json:"kind"`
		RequestedScope struct {
			Tool string `json:"tool"`
			Cmd  string `json:"cmd"`
			Env  string `json:"env"`
		} `json:"requested_scope"`
	}
	if err := json.Unmarshal([]byte(gotBody), &fwd); err != nil {
		t.Fatalf("decode forwarded body: %v (%q)", err, gotBody)
	}
	if fwd.Kind != "tool_call" || fwd.RequestedScope.Tool != "Bash" {
		t.Fatalf("forwarded = %+v", fwd)
	}
	// Clamped, and the truncation is VISIBLE — a human must never decide on a
	// silently shortened command.
	if want := strings.Repeat("x", maxToolCmd) + "… (truncated)"; fwd.RequestedScope.Cmd != want {
		t.Fatalf("cmd = %d bytes (%q…), want the clamped+marked form", len(fwd.RequestedScope.Cmd), fwd.RequestedScope.Cmd[:20])
	}
	if fwd.RequestedScope.Env != "AWS_PROFILE, HOME" {
		t.Fatalf("env = %q, want the joined NAME list", fwd.RequestedScope.Env)
	}
	// The created id rides the decision log so audit can join raise -> approval.
	d := lastDecision(t, buf)
	if d.RuleSource != ruleSourceApprovals || d.Decision != egress.Allow {
		t.Fatalf("decision = %+v", d)
	}
	if d.ApprovalID == nil || *d.ApprovalID != apID {
		t.Fatalf("decision approval_id = %v, want %v", d.ApprovalID, apID)
	}
}

// TestLocalToolApprovalCreateRejects pins the trust boundary: the sandbox may
// raise a tool hold and NOTHING else, and env values are unrepresentable on the
// wire (an object-shaped env is a 400, never a stored secret). Every rejection
// must fail BEFORE the control plane is contacted with the run token.
func TestLocalToolApprovalCreateRejects(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"egress kind", `{"kind":"egress_domain","payload":{"cmd":"curl evil.example"}}`},
		{"credential kind", `{"kind":"credential","payload":{"cmd":"x"}}`},
		{"missing kind", `{"payload":{"cmd":"x"}}`},
		{"env carries values", `{"kind":"tool_call","payload":{"cmd":"x","env":{"AWS_SECRET_ACCESS_KEY":"s3cr3t"}}}`},
		{"nothing to decide", `{"kind":"tool_call","payload":{"tool":"  ","cmd":""}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var contacted bool
			cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				contacted = true
				w.WriteHeader(http.StatusCreated)
			}))
			defer cp.Close()

			p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cp), nil, nil)
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, routeApprovalsCreate, strings.NewReader(tc.body)))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if contacted {
				t.Fatal("control plane was contacted with the run token for a rejected raise")
			}
			if strings.Contains(rec.Body.String(), "s3cr3t") {
				t.Fatal("an env value was echoed back to the sandbox")
			}
		})
	}
}

// TestRelayStripsHopByHopResponseHeaders pins the SHARED relay tail
// (local_routes.go relay), which every streamed brokered response goes through:
// the git broker, the git_pat broker and the brokered/MITM'd LLM forward all end
// in it, so one hole here is a hole on all four. The brokered LLM route is the
// cheapest way in.
//
// Hop-by-hop headers describe the UPSTREAM's connection, not this one, and
// relaying them is how a proxy leaks another connection's framing into its own.
// relay has to drop two kinds: the fixed RFC 7230 §6.1 names (Keep-Alive,
// Connection, …), and whatever the upstream NAMED in its own Connection header
// (X-Hop) — hop-by-hop only because that header says so. Everything else
// (X-Keep) and the status must arrive untouched.
//
// The two rows differ ONLY in whether that Connection header also carries the
// "close" token, and that turns out to decide whether the named-token arm can
// work at all — see the connCloseEatsTokenList row.
func TestRelayStripsHopByHopResponseHeaders(t *testing.T) {
	cases := []struct {
		name       string
		connection string
		wantXHop   string // "" == stripped
	}{
		{
			// The arm relay itself implements: Connection reaches resp.Header, so
			// removeHopByHop reads its token list and drops X-Hop by name.
			name:       "named token is stripped",
			connection: "X-Hop",
			wantXHop:   "",
		},
		{
			// KNOWN GAP, pinned deliberately rather than left as folklore. When
			// Connection carries "close", net/http's own readTransfer DELETES the
			// whole Connection header from resp.Header (folding it into
			// resp.Close) before relay ever runs. removeHopByHop therefore finds
			// no token list to read, and X-Hop is relayed to the sandbox. Nothing
			// sensitive rides it — it is a response header from an upstream this
			// run was already allowed to reach — but the strip is NOT the
			// unconditional one the code reads like. If relay ever learns to
			// consult resp.Close (or strips before the transport folds it), flip
			// this row to "" rather than deleting it.
			name:       "connection close eats the token list",
			connection: "close, X-Hop",
			wantXHop:   "1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Connection", tc.connection)
				w.Header().Set("X-Hop", "1")
				w.Header().Set("Keep-Alive", "timeout=5")
				w.Header().Set("X-Keep", "1")
				w.WriteHeader(http.StatusAccepted)
				_, _ = io.WriteString(w, "relayed")
			}))
			defer up.Close()

			inj := staticInj(map[string]injectedHeader{
				anthropicHost: {name: "X-Api-Key", value: "BROKERED-KEY"},
			})
			p, _ := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(up), inj, testInsecureTLSConfig)

			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages", strings.NewReader(`{}`)))

			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 relayed verbatim (body %q)", rec.Code, rec.Body.String())
			}
			if rec.Body.String() != "relayed" {
				t.Fatalf("body = %q, want the upstream body streamed through", rec.Body.String())
			}
			if got := rec.Header().Get("X-Keep"); got != "1" {
				t.Fatalf("X-Keep = %q, want it relayed — only hop-by-hop headers may be dropped", got)
			}
			if got := rec.Header().Get("Connection"); got != "" {
				t.Fatalf("Connection = %q, want it stripped (RFC 7230 §6.1 hop-by-hop)", got)
			}
			// A fixed-list name the transport does NOT fold away, so this arm is
			// relay's own work in both rows.
			if got := rec.Header().Get("Keep-Alive"); got != "" {
				t.Fatalf("Keep-Alive = %q, want it stripped (RFC 7230 §6.1 hop-by-hop)", got)
			}
			if got := rec.Header().Get("X-Hop"); got != tc.wantXHop {
				t.Fatalf("X-Hop = %q, want %q (Connection: %q)", got, tc.wantXHop, tc.connection)
			}
		})
	}
}

// TestBrokeredRouteControlPlaneDownDeniesAndFails502 pins relayControlPlane's
// error branch across the brokered sandbox->control-plane routes. It is the
// FAIL-CLOSED shape: an unreachable control plane is a 502 to the sandbox AND a
// Deny row under that route's own rule source — never a silent 200, and never an
// audit gap where a brokered call simply left no trace.
//
// The per-route rule source is the point of the table: these SIX share one tail,
// so a tail that emitted a single generic source would still pass a one-route
// test while making the decision log unable to say WHICH brokered call failed.
//
// F144: the table said "these four" and carried four rows while the dispatcher
// (local_routes.go) routed six calls into relayControlPlane. routeSSOToken —
// the sandbox's WRITE channel to the operator-wide AWS SSO blob, and the only
// 0%-covered handler in the file — and routeApprovalsCreate were both missing.
// The stale count was the tell, so the row set is no longer maintained by hand:
// constName is checked against the dispatcher itself by
// TestBrokeredRouteTableCoversEveryDispatchedRoute below.
// brokeredRouteCase is one row of the fail-closed table. constName names the
// dispatcher constant the row stands for, so the coverage guard below can hold
// the table to the dispatcher instead of to a comment.
type brokeredRouteCase struct {
	name       string
	constName  string
	method     string
	route      string
	body       string
	ruleSource string
}

// brokeredRouteFailClosedCases is the row set, shared by the fail-closed table
// and the dispatcher-coverage guard so neither can drift from the other.
func brokeredRouteFailClosedCases(runID uuid.UUID) []brokeredRouteCase {
	return []brokeredRouteCase{
		{"mint", "routeMint", http.MethodPost, routeMint, `{"grant_id":"` + uuid.New().String() + `"}`, ruleSourceMint},
		{"approval", "routeApprovals", http.MethodGet, routeApprovals + uuid.New().String(), "", ruleSourceApprovals},
		{"approval-create", "routeApprovalsCreate", http.MethodPost, routeApprovalsCreate,
			`{"kind":"tool_call","payload":{"tool":"Bash","cmd":"ls"}}`, ruleSourceApprovals},
		{"recording", "routeRecordings", http.MethodPut, routeRecordings + runID.String(), `{"version":2}`, ruleSourceRecordings},
		{"scan-result", "routeScanResults", http.MethodPut, routeScanResults + runID.String(), `{"facts":1}`, ruleSourceScanResults},
		{"sso-token", "routeSSOToken", http.MethodPut, routeSSOToken + runID.String(), `{"accessToken":"x"}`, ruleSourceSSOToken},
	}
}

func TestBrokeredRouteControlPlaneDownDeniesAndFails502(t *testing.T) {
	for _, tc := range brokeredRouteFailClosedCases(uuid.New()) {
		t.Run(tc.name, func(t *testing.T) {
			// "127.0.0.1:1" is the repo's dead-port convention: the control-plane
			// URL resolves and vets fine, and then nothing answers the dial.
			p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", "127.0.0.1:1", nil, nil)

			var body io.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			rec := httptest.NewRecorder()
			req := mustLocalReq(t, tc.method, tc.route, body)
			req.Header.Set("Content-Type", "application/json")
			p.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502 when the control plane is unreachable (body %q)", rec.Code, rec.Body.String())
			}
			d := lastDecision(t, buf)
			if d.RuleSource != tc.ruleSource || d.Decision != egress.Deny {
				t.Fatalf("decision = %+v, want a %s deny row", d, tc.ruleSource)
			}
		})
	}
}

// TestBrokeredRouteTableCoversEveryDispatchedRoute is the F144 root cause: the
// fail-closed table was maintained by hand and fell one (in fact two) routes
// behind the dispatcher, silently, for as long as the stale "these four" comment
// had been wrong. Read the dispatcher's own switch and require a row per
// route<Name> constant it dispatches, so a NEW brokered route cannot be added
// without one.
func TestBrokeredRouteTableCoversEveryDispatchedRoute(t *testing.T) {
	src, err := os.ReadFile("local_routes.go")
	if err != nil {
		t.Fatalf("read local_routes.go: %v", err)
	}
	var covered []string
	for _, tc := range brokeredRouteFailClosedCases(uuid.New()) {
		covered = append(covered, tc.constName)
	}
	dispatched := dispatchedRouteConsts(t, string(src))
	if len(dispatched) < 5 {
		t.Fatalf("found only %v dispatched route constants — the parser, not the dispatcher, "+
			"is what changed; fix this guard rather than deleting it", dispatched)
	}
	for _, name := range dispatched {
		if !slices.Contains(covered, name) {
			t.Errorf("%s is dispatched to a brokered handler but has no row in "+
				"TestBrokeredRouteControlPlaneDownDeniesAndFails502: every brokered "+
				"sandbox->control-plane route must be pinned to the 502 + per-route deny shape "+
				"(F144 — routeSSOToken, the sandbox's write channel to the operator SSO blob, "+
				"was the one that went missing)", name)
		}
	}
}

// dispatchedRouteConsts extracts the route<Name> constants whose dispatched
// handler is defined IN local_routes.go and reaches the shared
// relayControlPlane tail (directly or via forwardBrokeredUpload) — i.e. exactly
// the brokered sandbox->control-plane routes this table is about. The git/PAT
// broker lanes have their own files and their own tails, and the LLM prefixes
// are not route<Name> constants at all.
func dispatchedRouteConsts(t *testing.T, src string) []string {
	t.Helper()
	var out []string
	dispatch := regexp.MustCompile(`(?m)^\tcase [^\n]*\b(route[A-Z]\w*)[^\n]*:\n\t\tp\.(\w+)\(`)
	for _, m := range dispatch.FindAllStringSubmatch(src, -1) {
		routeConst, handler := m[1], m[2]
		body := funcBody(src, handler)
		if body == "" {
			continue // handler lives in another file: another lane, another tail
		}
		if !strings.Contains(body, "relayControlPlane(") && !strings.Contains(body, "forwardBrokeredUpload(") {
			continue
		}
		if !slices.Contains(out, routeConst) {
			out = append(out, routeConst)
		}
	}
	return out
}

// funcBody returns the source of `func (p *Proxy) <name>(...)` in src, or "" if
// it is not defined there.
func funcBody(src, name string) string {
	start := strings.Index(src, "func (p *Proxy) "+name+"(")
	if start < 0 {
		return ""
	}
	rest := src[start:]
	if end := strings.Index(rest, "\n}\n"); end >= 0 {
		return rest[:end]
	}
	return rest
}
