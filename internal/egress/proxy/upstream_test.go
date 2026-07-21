// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeUpstream is a minimal HTTP CONNECT proxy on 127.0.0.1 (a loopback, i.e.
// blocked, address — which is the whole point: a corp proxy is frequently
// private). It records the first CONNECT request line + Proxy-Authorization,
// replies 200, then echoes the tunnel so tests can prove end-to-end byte flow.
type fakeUpstream struct {
	ln      net.Listener
	mu      sync.Mutex
	connect string
	auth    string
	accepts int32
}

func startFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeUpstream{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&f.accepts, 1)
			go f.serve(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeUpstream) serve(c net.Conn) {
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		_ = c.Close()
		return
	}
	f.mu.Lock()
	f.connect = req.Method + " " + req.Host // req.Host == "host:port" for CONNECT
	f.auth = req.Header.Get("Proxy-Authorization")
	f.mu.Unlock()
	if req.Method != http.MethodConnect {
		_, _ = c.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\n\r\n"))
		_ = c.Close()
		return
	}
	if _, err := c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = c.Close()
		return
	}
	_, _ = io.Copy(c, br) // echo whatever the client sends post-CONNECT
	_ = c.Close()
}

func (f *fakeUpstream) addr() string { return f.ln.Addr().String() }

func (f *fakeUpstream) snapshot() (connect, auth string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connect, f.auth
}

// newUpstreamProxy builds a Proxy with a REAL dialer (so it can reach the
// loopback fake corp proxy) and the given upstream configured.
func newUpstreamProxy(t *testing.T, up *upstreamProxy) *Proxy {
	t.Helper()
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
	return newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"tls.test"}}),
		Sink:     sink,
		Resolver: publicResolver{},
		Upstream: up,
	})
}

// TestUpstreamCONNECTEndToEnd drives a full CONNECT through ServeHTTP and asserts
// the sidecar chained it through the corp proxy with CONNECT <real-host> + the
// Proxy-Authorization credential, and that bytes flow end-to-end.
func TestUpstreamCONNECTEndToEnd(t *testing.T) {
	f := startFakeUpstream(t)
	up, err := parseUpstreamProxy("http://user:hunter2pass@" + f.addr())
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	p := newUpstreamProxy(t, up)

	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	conn, status := connectThrough(t, proxySrv.URL, "tls.test:443")
	defer conn.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT response = %q, want 200", status)
	}
	// Tunnel is up: the fake corp proxy echoes.
	_, _ = io.WriteString(conn, "ping")
	buf := make([]byte, 4096)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf[:n]) != "ping" {
		t.Fatalf("tunnel echo = %q, want ping", string(buf[:n]))
	}

	gotConnect, gotAuth := f.snapshot()
	if gotConnect != "CONNECT tls.test:443" {
		t.Fatalf("upstream saw %q, want CONNECT tls.test:443 (REAL hostname, not vetted IP)", gotConnect)
	}
	wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:hunter2pass"))
	if gotAuth != wantAuth {
		t.Fatalf("Proxy-Authorization = %q, want %q", gotAuth, wantAuth)
	}
}

// TestUpstreamPrivateIPException proves the deliberate guard relaxation: the
// general VetHost guard DENIES the loopback address of the corp proxy, yet the
// upstream hop dials it successfully because it is operator-configured.
func TestUpstreamPrivateIPException(t *testing.T) {
	f := startFakeUpstream(t) // listens on 127.0.0.1 (loopback == blocked range)

	// Sanity: the general SSRF guard WOULD deny this address.
	host, _ := splitHostPort(f.addr(), 0)
	if g := VetHost(host, netResolver{}); !g.Denied {
		t.Fatalf("expected VetHost to deny loopback %q, got %+v", host, g)
	}

	up, err := parseUpstreamProxy("http://" + f.addr())
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	p := newUpstreamProxy(t, up)

	// The exception: dialing the operator-configured proxy at a loopback addr
	// succeeds despite the guard that just denied the same address.
	conn, err := p.dialThroughUpstream(context.Background(), "tls.test", 443)
	if err != nil {
		t.Fatalf("upstream dial to private proxy addr should be allowed (audited exception), got: %v", err)
	}
	_ = conn.Close()
	if gotConnect, _ := f.snapshot(); gotConnect != "CONNECT tls.test:443" {
		t.Fatalf("upstream saw %q, want CONNECT tls.test:443", gotConnect)
	}
}

// TestUpstreamDoesNotWeakenLiteralIPGuard is the regression that the private-IP
// exception is scoped to the CONFIGURED proxy address ONLY: with an upstream
// proxy set, an AGENT-chosen egress target that is a literal private/loopback/
// metadata IP is STILL denied by the step-0 guard (no SSRF-via-corp-proxy).
func TestUpstreamDoesNotWeakenLiteralIPGuard(t *testing.T) {
	f := startFakeUpstream(t)
	up, err := parseUpstreamProxy("http://" + f.addr())
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	p := newUpstreamProxy(t, up) // upstream != nil
	for _, target := range []string{"169.254.169.254", "127.0.0.1", "10.0.0.5"} {
		dec, _, _ := p.evaluate(context.Background(), target, 443, "CONNECT", "")
		if dec != egress.Deny {
			t.Errorf("agent target %s must be DENIED even with an upstream proxy set (got %v)", target, dec)
		}
	}
}

// TestControlPlaneBypassesUpstream proves the transport split: with an upstream
// configured, a control-plane forward dials the control plane DIRECTLY (never
// via the corp proxy), while an egress dial DOES go through the corp proxy.
func TestControlPlaneBypassesUpstream(t *testing.T) {
	var cpHits int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&cpHits, 1)
		if got := r.Header.Get("Authorization"); got != "Bearer run-tok" {
			t.Errorf("control-plane Authorization = %q, want Bearer run-tok", got)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer cp.Close()

	f := startFakeUpstream(t)
	up, err := parseUpstreamProxy("http://" + f.addr())
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"tls.test"}}),
		Sink:            sink,
		Resolver:        publicResolver{},
		Upstream:        up,
		ControlPlaneURL: cp.URL,
		RunToken:        newTokenSource("run-tok"),
	})

	// Control-plane forward: must reach the CP directly, corp proxy untouched.
	resp, err := p.forwardToControlPlane(context.Background(), http.MethodGet, "/api/v1/ping", nil, "")
	if err != nil {
		t.Fatalf("forwardToControlPlane: %v", err)
	}
	_ = resp.Body.Close()
	if got := atomic.LoadInt32(&cpHits); got != 1 {
		t.Fatalf("control plane hit %d times, want 1", got)
	}
	if got := atomic.LoadInt32(&f.accepts); got != 0 {
		t.Fatalf("corp proxy was used for a control-plane call (accepts=%d) — the split leaked the run token", got)
	}

	// Egress dial from the SAME proxy DOES traverse the corp proxy.
	ec, err := p.dialThroughUpstream(context.Background(), "tls.test", 443)
	if err != nil {
		t.Fatalf("egress dial through upstream: %v", err)
	}
	_ = ec.Close()
	if got := atomic.LoadInt32(&f.accepts); got != 1 {
		t.Fatalf("egress dial should use the corp proxy (accepts=%d, want 1)", got)
	}
}

// TestUpstreamCredentialMasked verifies the corp-proxy credential is masked from
// decision-log/stdout output via the process mask registry.
func TestUpstreamCredentialMasked(t *testing.T) {
	up, err := parseUpstreamProxy("http://alice:s3cr3t-longpass@proxy.corp:8080")
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	vals := up.maskValues()
	if len(vals) == 0 {
		t.Fatal("expected credential mask values")
	}
	for _, v := range vals {
		procRegistry.AddGlobal(v)
	}
	b64 := base64.StdEncoding.EncodeToString([]byte("alice:s3cr3t-longpass"))
	line := []byte(`{"proxy_authorization":"Basic ` + b64 + `","pw":"s3cr3t-longpass"}`)
	masked := maskDecisionBytes(line)
	if bytes.Contains(masked, []byte("s3cr3t-longpass")) {
		t.Fatalf("password leaked through mask: %s", masked)
	}
	if bytes.Contains(masked, []byte(b64)) {
		t.Fatalf("wire credential leaked through mask: %s", masked)
	}
}

// TestUpstreamBasicAuthRawCredential guards against percent-encoding the Basic
// credential: a proxy password with reserved characters must be sent decoded
// (user:p@ss/w0rd), not url.Userinfo.String()'s encoded form (user:p%40ss%2Fw0rd).
func TestUpstreamBasicAuthRawCredential(t *testing.T) {
	up, err := parseUpstreamProxy("http://alice:p%40ss%2Fw0rd@proxy.corp:8080")
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	// The raw password is "p@ss/w0rd" (%40 -> '@', %2F -> '/').
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:p@ss/w0rd"))
	if up.authHeader != want {
		t.Fatalf("authHeader = %q, want %q (must use decoded credential, not percent-encoded)", up.authHeader, want)
	}
}

// TestUpstreamParseRejectsBadScheme guards the config-validation path.
func TestUpstreamParseRejectsBadScheme(t *testing.T) {
	for _, raw := range []string{"ftp://proxy:1080", "socks5://proxy:1080", "http://", "://nohost"} {
		if _, err := parseUpstreamProxy(raw); err == nil {
			t.Fatalf("parseUpstreamProxy(%q) = nil error, want rejection", raw)
		}
	}
	if up, err := parseUpstreamProxy(""); err != nil || up != nil {
		t.Fatalf("empty url: got (%v,%v), want (nil,nil)", up, err)
	}
}

// TestUpstreamCONNECTStallFailsFast pins the fix for the reported "approved
// request hangs forever" symptom: a corporate parent proxy that ACCEPTS the TCP
// connection and then never answers the CONNECT must surface as an error, not as
// an unbounded block. The MITM path tells the agent "200 Connection Established"
// before this dial happens, so without a read deadline the operator sees an
// approved request stall indefinitely with no failure to act on.
func TestUpstreamCONNECTStallFailsFast(t *testing.T) {
	// A listener that accepts and then goes silent forever.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold the connection open, never write a reply.
			go func(c net.Conn) { <-done; _ = c.Close() }(c)
		}
	}()

	up, err := parseUpstreamProxy("http://" + ln.Addr().String())
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	p := newUpstreamProxy(t, up)

	// Shrink the wait: the production budget is upstreamConnectTimeout, but the
	// property under test is "bounded", not the exact value.
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := p.dialThroughUpstream(context.Background(), "tls.test", 443)
		ch <- result{c, err}
	}()

	select {
	case got := <-ch:
		if got.err == nil {
			_ = got.conn.Close()
			t.Fatal("dialThroughUpstream returned success against a silent upstream; want an error")
		}
		if !strings.Contains(got.err.Error(), "read upstream CONNECT response") {
			t.Errorf("error = %v, want the CONNECT-response read to fail", got.err)
		}
	case <-time.After(upstreamConnectTimeout + 10*time.Second):
		t.Fatal("dialThroughUpstream HUNG against a silent upstream — the read deadline is missing")
	}
}
