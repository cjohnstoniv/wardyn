// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// redirectDial returns a dial seam that connects to addr regardless of the
// requested (vetted) target. This lets tests resolve a public IP (passing the
// SSRF guard) while the real upstream is a local httptest server.
func redirectDial(addr string) func(ctx context.Context, network, _ string) (net.Conn, error) {
	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	}
}

// newTestProxy builds a Proxy whose dialer always connects to upstreamAddr and
// whose resolver maps every host to a public IP (so the SSRF guard passes for
// allowed hosts). The sink writes JSON lines to buf.
func newTestProxy(t *testing.T, spec types.RunPolicySpec, upstreamAddr string, ap *approvalClient, inj *injector) (*Proxy, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
	// No background poster started; emit() only needs the buffer + chan.
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(spec),
		Approval: ap,
		Injector: inj,
		Sink:     sink,
		Resolver: publicResolver{},
		Dial:     redirectDial(upstreamAddr),
	})
	return p, buf
}

// publicResolver maps any host to a public-looking address.
type publicResolver struct{}

func (publicResolver) LookupIP(host string) ([]net.IP, error) {
	return []net.IP{net.ParseIP("93.184.216.34")}, nil
}

// drainSink reads emitted decision logs from a buffer (one JSON object/line).
func lastDecision(t *testing.T, buf *bytes.Buffer) egress.DecisionLog {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("no decision logged")
	}
	var d egress.DecisionLog
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &d); err != nil {
		t.Fatalf("decode decision: %v (line %q)", err, lines[len(lines)-1])
	}
	return d
}

// findDecision returns the last logged decision whose RuleSource == ruleSource.
// Use it when a specific decision (not merely the final one) must be asserted —
// e.g. a coverage signal emitted before the trailing allow.
func findDecision(t *testing.T, buf *bytes.Buffer, ruleSource string) egress.DecisionLog {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var d egress.DecisionLog
		if err := json.Unmarshal([]byte(lines[i]), &d); err != nil {
			continue
		}
		if d.RuleSource == ruleSource {
			return d
		}
	}
	t.Fatalf("no decision with rule_source %q in %q", ruleSource, buf.String())
	return egress.DecisionLog{}
}

func TestPlainHTTPAllow(t *testing.T) {
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("X-Upstream", "ok")
		_, _ = io.WriteString(w, "hello")
	}))
	defer upstream.Close()

	p, buf := newTestProxy(t, types.RunPolicySpec{
		AllowedDomains: []string{"allowed.test"},
	}, upstreamAddr(upstream), nil, nil)

	rec := httptest.NewRecorder()
	req := mustProxyReq(t, http.MethodGet, "http://allowed.test/path")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "hello" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	if rec.Header().Get("X-Upstream") != "ok" {
		t.Fatalf("upstream header not propagated")
	}
	if gotAuth != "" {
		t.Fatalf("no injection configured, but Authorization was set to %q", gotAuth)
	}
	if d := lastDecision(t, buf); d.Decision != egress.Allow {
		t.Fatalf("decision = %q, want allow", d.Decision)
	}
}

func TestPlainHTTPDeny(t *testing.T) {
	cu := captureUpstream(t, false, "")

	p, buf := newTestProxy(t, types.RunPolicySpec{
		AllowedDomains: []string{"allowed.test"},
	}, upstreamAddr(cu.srv), nil, nil)

	rec := httptest.NewRecorder()
	req := mustProxyReq(t, http.MethodGet, "http://denied.test/")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if cu.reached {
		t.Fatal("upstream must not be reached on deny")
	}
	if d := lastDecision(t, buf); d.Decision != egress.Deny {
		t.Fatalf("decision = %q, want deny", d.Decision)
	}
}

func TestPlainHTTPMethodDeny(t *testing.T) {
	cu := captureUpstream(t, false, "")

	p, _ := newTestProxy(t, types.RunPolicySpec{
		AllowedDomains: []string{"allowed.test"},
		AllowedMethods: []string{"GET"},
	}, upstreamAddr(cu.srv), nil, nil)

	rec := httptest.NewRecorder()
	req := mustProxyReq(t, http.MethodPost, "http://allowed.test/")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST should be denied: status = %d", rec.Code)
	}
	if cu.reached {
		t.Fatal("upstream must not be reached when method denied")
	}
}

func TestPlainHTTPInjection(t *testing.T) {
	cu := captureUpstream(t, false, "ok")

	spec := types.RunPolicySpec{AllowedDomains: []string{"api.test", "*.wild.test"}}
	inj := staticInj(map[string]injectedHeader{
		"api.test": {name: "Authorization", value: "Bearer SEKRET"},
	})

	p, _ := newTestProxy(t, spec, upstreamAddr(cu.srv), nil, inj)

	rec := httptest.NewRecorder()
	req := mustProxyReq(t, http.MethodGet, "http://api.test/v1")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := cu.header.Get("Authorization"); got != "Bearer SEKRET" {
		t.Fatalf("injected header = %q, want Bearer SEKRET", got)
	}
}

func TestPlainHTTPRequiresAbsoluteURI(t *testing.T) {
	p, _ := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"allowed.test"}}, "127.0.0.1:1", nil, nil)
	rec := httptest.NewRecorder()
	// Origin-form request (no absolute URI) — invalid for a forward proxy.
	req := httptest.NewRequest(http.MethodGet, "/relative", nil)
	req.URL.Host = ""
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestPrivateIPDeniedEndToEnd(t *testing.T) {
	// Resolver returns a private IP; even an allowed host must be denied.
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)}
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"internal.test"}}),
		Sink:     sink,
		Resolver: fakeResolver{m: map[string][]net.IP{"internal.test": ips("169.254.169.254")}},
	})
	rec := httptest.NewRecorder()
	req := mustProxyReq(t, http.MethodGet, "http://internal.test/")
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("metadata IP must be denied: status = %d", rec.Code)
	}
	if d := lastDecision(t, buf); d.RuleSource != "builtin:private-ip" {
		t.Fatalf("rule_source = %q, want builtin:private-ip", d.RuleSource)
	}
}

func TestFirstUsePendingPath(t *testing.T) {
	// Control plane stub: POST raises an approval that stays PENDING.
	var raised atomic.Int32
	apID := uuid.New()
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/internal/approvals"):
			raised.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: apID, State: types.ApprovalPending})
		default:
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	defer cp.Close()

	ap := newApprovalClient(cp.URL, "tok", uuid.New(), cp.Client())
	p, buf := newTestProxy(t, types.RunPolicySpec{
		AllowedDomains:   []string{"known.test"},
		FirstUseApproval: types.FirstUseDenyWithReview,
	}, "127.0.0.1:1", ap, nil)

	rec := httptest.NewRecorder()
	req := mustProxyReq(t, http.MethodGet, "http://unknown.test/")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("first-use should return 403, got %d", rec.Code)
	}
	var body struct {
		Wardyn     string `json:"wardyn"`
		ApprovalID string `json:"approval_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode pending body: %v (%q)", err, rec.Body.String())
	}
	if body.Wardyn != "approval_pending" {
		t.Fatalf("wardyn field = %q", body.Wardyn)
	}
	if body.ApprovalID != apID.String() {
		t.Fatalf("approval_id = %q, want %q", body.ApprovalID, apID)
	}
	if d := lastDecision(t, buf); d.Decision != egress.Pending {
		t.Fatalf("decision = %q, want pending", d.Decision)
	}
	if raised.Load() != 1 {
		t.Fatalf("approval raised %d times, want 1", raised.Load())
	}

	// Second request to same host: must NOT raise again (cached pending,
	// throttled poll).
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, mustProxyReq(t, http.MethodGet, "http://unknown.test/again"))
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("second pending request status = %d", rec2.Code)
	}
	if raised.Load() != 1 {
		t.Fatalf("approval must not be re-raised; raised=%d", raised.Load())
	}
}

func TestFirstUseApprovedThenAllowed(t *testing.T) {
	apID := uuid.New()
	var state atomic.Value
	state.Store(types.ApprovalPending)
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/internal/approvals"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: apID, State: types.ApprovalPending})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/internal/approvals/"):
			st := state.Load().(types.ApprovalState)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: apID, State: st})
		default:
			http.Error(w, "unexpected", http.StatusTeapot)
		}
	}))
	defer cp.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "reached")
	}))
	defer upstream.Close()

	ap := newApprovalClient(cp.URL, "tok", uuid.New(), cp.Client())
	// Drive pollInterval to zero so the second request polls immediately.
	p, _ := newTestProxy(t, types.RunPolicySpec{
		AllowedDomains:   []string{"known.test"},
		FirstUseApproval: types.FirstUseDenyWithReview,
	}, upstreamAddr(upstream), ap, nil)

	// 1st request: pending.
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://newhost.test/"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("first request want 403 got %d", rec.Code)
	}

	// Approver approves; force the cache to allow poll now.
	state.Store(types.ApprovalApproved)
	forcePollNow(ap, "newhost.test")

	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, mustProxyReq(t, http.MethodGet, "http://newhost.test/v"))
	if rec2.Code != http.StatusOK {
		t.Fatalf("after approval want 200 got %d body=%q", rec2.Code, rec2.Body.String())
	}
	if rec2.Body.String() != "reached" {
		t.Fatalf("body = %q", rec2.Body.String())
	}
}

func TestFirstUseDeniedCached(t *testing.T) {
	apID := uuid.New()
	var getCount atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/internal/approvals"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: apID, State: types.ApprovalPending})
		case r.Method == http.MethodGet:
			getCount.Add(1)
			_ = json.NewEncoder(w).Encode(types.ApprovalRequest{ID: apID, State: types.ApprovalDenied})
		}
	}))
	defer cp.Close()

	ap := newApprovalClient(cp.URL, "tok", uuid.New(), cp.Client())
	p, _ := newTestProxy(t, types.RunPolicySpec{FirstUseApproval: types.FirstUseDenyWithReview}, "127.0.0.1:1", ap, nil)

	// raise
	p.ServeHTTP(httptest.NewRecorder(), mustProxyReq(t, http.MethodGet, "http://nope.test/"))
	// poll -> denied
	forcePollNow(ap, "nope.test")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://nope.test/"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("denied host status = %d", rec.Code)
	}
	// Further requests must be served from the cached deny (no extra GETs).
	before := getCount.Load()
	p.ServeHTTP(httptest.NewRecorder(), mustProxyReq(t, http.MethodGet, "http://nope.test/"))
	if getCount.Load() != before {
		t.Fatalf("denied decision must be cached; extra polls happened")
	}
}

func TestConnectTunnelAllow(t *testing.T) {
	// Build a proxy whose dialer connects to an echo server (stand-in for the
	// TLS upstream — CONNECT is opaque bytes) regardless of IP.
	p, _ := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"tls.test"}}, startEcho(t), nil, nil)

	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	conn, status := connectThrough(t, proxySrv.URL, "tls.test:443")
	defer conn.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT response = %q, want 200", status)
	}
	// Now the tunnel echoes.
	_, _ = io.WriteString(conn, "ping")
	br := make([]byte, 4096)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := conn.Read(br)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(br[:n]) != "ping" {
		t.Fatalf("tunnel echo = %q", string(br[:n]))
	}
}

func TestConnectTunnelDeny(t *testing.T) {
	// Deny is decided before any dial, so the upstream address is unused.
	p, buf := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"tls.test"}}, "127.0.0.1:1", nil, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	conn, status := connectThrough(t, proxySrv.URL, "blocked.test:443")
	defer conn.Close()
	if !strings.Contains(status, "403") {
		t.Fatalf("CONNECT to denied host = %q, want 403", status)
	}
	if d := lastDecision(t, buf); d.Decision != egress.Deny {
		t.Fatalf("decision = %q, want deny", d.Decision)
	}
	if d := lastDecision(t, buf); d.Request.Method != "CONNECT" {
		t.Fatalf("method = %q, want CONNECT", d.Request.Method)
	}
}

// TestConnectTunnelDialFailEmitsDenyNotAllow pins the E3 fix: a raw-tunnel
// CONNECT to an ALLOWED host whose upstream dial FAILS must emit exactly one
// dial-failed Deny and NO allow decision — the allow is emitted only after a
// successful dial, so a failed dial can never over-report an allow.
func TestConnectTunnelDialFailEmitsDenyNotAllow(t *testing.T) {
	// "127.0.0.1:1" is the repo's dead-port convention: nothing listens there,
	// so redirectDial's real dial fails and emits builtin:dial-failed itself.
	p, buf := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"tls.test"}}, "127.0.0.1:1", nil, nil)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	conn, _ := connectThrough(t, proxySrv.URL, "tls.test:443") // the handler ran; a failed dial returns a non-200
	defer conn.Close()

	// The dial-failed deny is present...
	if d := findDecision(t, buf, "builtin:dial-failed"); d.Decision != egress.Deny {
		t.Fatalf("dial-failed decision = %q, want deny", d.Decision)
	}
	// ...and NO allow decision was logged for the failed dial.
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var dl egress.DecisionLog
		if json.Unmarshal([]byte(line), &dl) == nil && dl.Decision == egress.Allow {
			t.Fatalf("a failed dial must NOT emit an allow decision; got %q", line)
		}
	}
}

// --- helpers ---

// testInsecureTLSConfig trusts any httptest TLS server's self-signed leaf.
// Shared client-side TLSClientConfig for tests standing in an HTTPS upstream
// or MITM CA store — never used against a real upstream.
var testInsecureTLSConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only

// capturedUpstream is a fake upstream server that records the one request it
// receives (host/path/body/headers) and optionally flags that it was reached.
type capturedUpstream struct {
	srv     *httptest.Server
	reached bool
	header  http.Header
	host    string
	path    string
	body    string
}

// captureUpstream starts a fake upstream (TLS if useTLS) that records the
// request it receives and writes respBody back verbatim (if non-empty).
func captureUpstream(t *testing.T, useTLS bool, respBody string) *capturedUpstream {
	t.Helper()
	cu := &capturedUpstream{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cu.reached = true
		cu.header = r.Header
		cu.host = r.Host
		cu.path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		cu.body = string(b)
		if respBody != "" {
			_, _ = io.WriteString(w, respBody)
		}
	})
	if useTLS {
		cu.srv = httptest.NewTLSServer(h)
	} else {
		cu.srv = httptest.NewServer(h)
	}
	t.Cleanup(cu.srv.Close)
	return cu
}

// startEcho starts a TCP echo server (stand-in for an opaque TLS upstream —
// CONNECT tunnels carry opaque bytes) and returns its address.
func startEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				_, _ = io.Copy(conn, conn)
				_ = conn.Close()
			}(c)
		}
	}()
	return ln.Addr().String()
}

// connectThrough dials the proxy at proxyURL and issues a raw CONNECT for
// hostport, returning the established connection and the raw response text
// read back (status line + headers). Read errors are tolerated (some callers
// intentionally probe a connection that never completes the CONNECT).
func connectThrough(t *testing.T, proxyURL, hostport string) (net.Conn, string) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "CONNECT "+hostport+" HTTP/1.1\r\nHost: "+hostport+"\r\n\r\n")
	br := make([]byte, 4096)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _ := conn.Read(br)
	return conn, string(br[:n])
}

func upstreamAddr(s *httptest.Server) string {
	u, _ := url.Parse(s.URL)
	return u.Host
}

func mustProxyReq(t *testing.T, method, rawurl string) *http.Request {
	t.Helper()
	u, err := url.Parse(rawurl)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, rawurl, nil)
	req.URL = u // absolute-form
	req.RequestURI = rawurl
	return req
}

// forcePollNow resets a host's lastPoll so the next Resolve polls immediately.
func forcePollNow(ap *approvalClient, host string) {
	ap.mu.Lock()
	defer ap.mu.Unlock()
	if st, ok := ap.hosts[host]; ok {
		st.lastPoll = time.Time{}
	}
}
