// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// rawCONNECT sends one CONNECT for hostport to the proxy at addr and returns the client leg (closed
// at the test's end) and the proxy's answer. Nothing else is sent on it.
func rawCONNECT(t *testing.T, addr, hostport string) (net.Conn, *http.Response) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := io.WriteString(c, "CONNECT "+hostport+" HTTP/1.1\r\nHost: "+hostport+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT answer for %s: %v", hostport, err)
	}
	_ = c.SetReadDeadline(time.Time{})
	return c, resp
}

// shortenHandshakeTimeout lowers mitmHandshakeTimeout for one test. Its tests do not run in parallel.
func shortenHandshakeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	old := mitmHandshakeTimeout
	mitmHandshakeTimeout = d
	t.Cleanup(func() { mitmHandshakeTimeout = old })
}

// tunnelCapRows decodes every row the buffer holds.
func tunnelCapRows(t *testing.T, buf *bytes.Buffer) []egress.DecisionLog {
	t.Helper()
	var rows []egress.DecisionLog
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if l == "" {
			continue
		}
		var d egress.DecisionLog
		if err := json.Unmarshal([]byte(l), &d); err != nil {
			t.Fatalf("decode decision: %v (line %q)", err, l)
		}
		rows = append(rows, d)
	}
	return rows
}

// wantTunnelLimit fails unless resp is the temporary refusal an opaque tunnel past the cap gets.
func wantTunnelLimit(t *testing.T, resp *http.Response, host string) {
	t.Helper()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get(egressHeaderReason) != ruleSourceTunnelLimit ||
		resp.Header.Get("Retry-After") == "" || resp.Header.Get("X-Should-Retry") != "true" || resp.Header.Get(egressHeaderHost) != host {
		t.Fatalf("answer = %d %v, want a 503 naming %s and %s with a retry hint", resp.StatusCode, resp.Header, ruleSourceTunnelLimit, host)
	}
	if got := resp.Header.Get(egressHeaderStatus); got != "" {
		t.Errorf("%s = %q on a temporary refusal, want none", egressHeaderStatus, got)
	}
}

// wantClosedWithin fails unless the proxy closes c (a read ending in EOF or a reset, never a read
// timeout) within limit of start.
func wantClosedWithin(t *testing.T, c net.Conn, start time.Time, limit time.Duration) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err := io.ReadAll(c)
	var ne net.Error
	if err != nil && errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("the proxy never closed the connection: %v", err)
	}
	if d := time.Since(start); d > limit {
		t.Fatalf("closed after %v, want within %v", d, limit)
	}
}

// TestTunnelCap_TerminatedTunnelTakesASlot: a CONNECT the proxy terminates TLS for counts toward
// maxTunnels like an opaque one. Past the cap it gets the same temporary 503 and a deny row, and
// no allow row; once a tunnel closes, the next is admitted.
func TestTunnelCap_TerminatedTunnelTakesASlot(t *testing.T) {
	cu := captureUpstream(t, true, "ok")
	p, buf, caPEM := mitmProxy(t, "alert", cu.srv)
	p.maxTunnels = 1
	proxySrv := httptest.NewServer(p)
	t.Cleanup(proxySrv.Close)

	first := agentMITMConn(t, proxySrv.URL, caPEM)
	waitTunnels(t, p, 1)

	_, resp := rawCONNECT(t, proxySrv.Listener.Addr().String(), anthropicHost+":443")
	wantTunnelLimit(t, resp, anthropicHost)

	rows := tunnelCapRows(t, buf)
	allows := 0
	for _, d := range rows {
		if d.Decision == egress.Allow && d.Request.Method == http.MethodConnect && d.Request.Host == anthropicHost {
			allows++
		}
	}
	if allows != 1 {
		t.Errorf("%d CONNECT allow rows for %s, want 1 (the first tunnel's): %+v", allows, anthropicHost, rows)
	}
	if last := rows[len(rows)-1]; last.Decision != egress.Deny || last.RuleSource != ruleSourceTunnelLimit || last.Request.Host != anthropicHost {
		t.Errorf("last row = %+v, want a %s deny for %s", last, ruleSourceTunnelLimit, anthropicHost)
	}

	_ = first.Close()
	waitTunnels(t, p, 0)
	third := agentMITMConn(t, proxySrv.URL, caPEM)
	_ = third.Close()
}

// TestTunnelCap_TerminatedAndOpaqueShareOneCap: opaque and terminated tunnels draw on one cap, in
// either order.
func TestTunnelCap_TerminatedAndOpaqueShareOneCap(t *testing.T) {
	cu := captureUpstream(t, true, "ok")
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	caPEM := certPEM
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{anthropicHost, "example.com"}}),
		Sink:            &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 32)},
		Scanner:         scanEngine(t, "alert", scanTestSecret),
		CA:              ca,
		MITMLLM:         true,
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(cu.srv)),
		TLSClientConfig: testInsecureTLSConfig,
	})
	p.maxTunnels = 1
	proxySrv := httptest.NewServer(p)
	t.Cleanup(proxySrv.Close)
	addr := proxySrv.Listener.Addr().String()

	opaque, resp := openCONNECT(t, addr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("opaque tunnel = %d, want 200", resp.StatusCode)
	}
	_, resp = rawCONNECT(t, addr, anthropicHost+":443")
	wantTunnelLimit(t, resp, anthropicHost)

	_ = opaque.Close()
	waitTunnels(t, p, 0)
	terminated := agentMITMConn(t, proxySrv.URL, caPEM)
	defer func() { _ = terminated.Close() }()
	waitTunnels(t, p, 1)
	_, resp = openCONNECT(t, addr)
	wantTunnelLimit(t, resp, "example.com")
}

// TestMITMConnect_SilentClientIsClosedAtTheHandshakeDeadline: a client that opens a terminated
// tunnel and sends nothing is closed at the handshake deadline, its slot freed, and it leaves no
// failure row (it never spoke).
func TestMITMConnect_SilentClientIsClosedAtTheHandshakeDeadline(t *testing.T) {
	shortenHandshakeTimeout(t, 200*time.Millisecond)
	cu := captureUpstream(t, true, "ok")
	p, buf, _ := mitmProxy(t, "alert", cu.srv)
	proxySrv := httptest.NewServer(p)
	t.Cleanup(proxySrv.Close)

	c, resp := rawCONNECT(t, proxySrv.Listener.Addr().String(), anthropicHost+":443")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT = %d, want 200", resp.StatusCode)
	}
	wantClosedWithin(t, c, time.Now(), 2*time.Second)
	waitTunnels(t, p, 0)
	for _, d := range tunnelCapRows(t, buf) {
		if d.RuleSource == ruleSourceTunnelFailed {
			t.Errorf("a client that sent nothing left a row: %+v", d)
		}
	}
}

// TestMITMConnect_StalledHandshakeIsClosedWithItsRow: a client that sends a TLS record header and
// stalls is closed at the deadline and leaves today's client-handshake failure row.
func TestMITMConnect_StalledHandshakeIsClosedWithItsRow(t *testing.T) {
	shortenHandshakeTimeout(t, 200*time.Millisecond)
	cu := captureUpstream(t, true, "ok")
	p, buf, _ := mitmProxy(t, "alert", cu.srv)
	proxySrv := httptest.NewServer(p)
	t.Cleanup(proxySrv.Close)

	c, resp := rawCONNECT(t, proxySrv.Listener.Addr().String(), anthropicHost+":443")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT = %d, want 200", resp.StatusCode)
	}
	if _, err := io.WriteString(c, "\x16\x03\x01\x00\x50"); err != nil {
		t.Fatal(err)
	}
	wantClosedWithin(t, c, time.Now(), 2*time.Second)
	waitTunnels(t, p, 0)
	d := lastDecision(t, buf)
	if d.Decision != egress.Deny || d.RuleSource != ruleSourceTunnelFailed ||
		!strings.HasPrefix(d.Cause, "client tls handshake (sandbox to wardyn-proxy): ") {
		t.Errorf("last row = %+v, want a %s deny whose cause starts with the client handshake stage", d, ruleSourceTunnelFailed)
	}
}

// TestMITMConnect_DeadlineEndsWithTheHandshake: the handshake deadline is cleared once the
// handshake is in. A request sent, and an answer still streaming, past the deadline are served.
func TestMITMConnect_DeadlineEndsWithTheHandshake(t *testing.T) {
	shortenHandshakeTimeout(t, 1*time.Second)
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "a")
		w.(http.Flusher).Flush()
		time.Sleep(1500 * time.Millisecond)
		_, _ = io.WriteString(w, "b")
	}))
	t.Cleanup(upstream.Close)
	p, _, caPEM := mitmProxy(t, "alert", upstream)
	proxySrv := httptest.NewServer(p)
	t.Cleanup(proxySrv.Close)

	conn := agentMITMConn(t, proxySrv.URL, caPEM)
	defer func() { _ = conn.Close() }()
	time.Sleep(1300 * time.Millisecond)
	req, err := http.NewRequest(http.MethodPost, "https://"+anthropicHost+"/v1/messages", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if err := req.Write(conn); err != nil {
		t.Fatalf("write the request past the deadline: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	got, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("read the response past the deadline: %v", err)
	}
	defer func() { _ = got.Body.Close() }()
	body, err := io.ReadAll(got.Body)
	if got.StatusCode != http.StatusOK || string(body) != "ab" || err != nil {
		t.Fatalf("response = %d %q (%v), want 200 \"ab\": the handshake deadline must not outlive the handshake", got.StatusCode, body, err)
	}
}

// TestMITMConnect_PlaintextOriginPeekIsBounded: on a plaintext origin the proxy waits for the
// client's first byte before it chooses TLS or not; a client that sends none is closed.
func TestMITMConnect_PlaintextOriginPeekIsBounded(t *testing.T) {
	shortenHandshakeTimeout(t, 200*time.Millisecond)
	origin := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(origin.Close)
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	inj := &injector{byHost: map[string]*injEntry{plainMITMHost: {
		grantID: uuid.New(),
		header:  injectedHeader{name: "x-amz-sso_bearer_token", value: "the-real-session-token"},
	}}}
	p := newProxy(Options{
		RunID:     uuid.New(),
		Policy:    CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{plainMITMHost}}),
		Injector:  inj,
		Sink:      &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
		Resolver:  publicResolver{},
		Dial:      redirectDial(upstreamAddr(origin)),
		MITMHosts: []string{"http://" + plainMITMHost + ":8090"},
		CA:        ca,
	})
	proxySrv := httptest.NewServer(p)
	t.Cleanup(proxySrv.Close)

	c, resp := rawCONNECT(t, proxySrv.Listener.Addr().String(), plainMITMHost+":8090")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT = %d, want 200", resp.StatusCode)
	}
	wantClosedWithin(t, c, time.Now(), 2*time.Second)
	waitTunnels(t, p, 0)
}

// TestMITMConnect_ManyStalledConnectsStayBounded: the measurement the cap and the deadline exist
// for, at test scale. Stalled CONNECTs past the cap are refused; those inside it are closed at the
// deadline, and the proxy's tunnels and goroutines return to where they started.
func TestMITMConnect_ManyStalledConnectsStayBounded(t *testing.T) {
	const (
		capN  = 16
		total = 40
	)
	shortenHandshakeTimeout(t, 2*time.Second)
	cu := captureUpstream(t, true, "ok")
	p, _, _ := mitmProxy(t, "alert", cu.srv)
	p.maxTunnels = capN
	proxySrv := httptest.NewServer(p)
	t.Cleanup(proxySrv.Close)
	addr := proxySrv.Listener.Addr().String()
	base := runtime.NumGoroutine()

	var open, refused []net.Conn
	for range total {
		c, resp := rawCONNECT(t, addr, anthropicHost+":443")
		switch resp.StatusCode {
		case http.StatusOK:
			open = append(open, c)
		case http.StatusServiceUnavailable:
			refused = append(refused, c)
		default:
			t.Fatalf("CONNECT = %d, want 200 or 503", resp.StatusCode)
		}
	}
	if len(open) != capN || len(refused) != total-capN {
		t.Fatalf("%d answers 200 and %d answers 503, want %d and %d", len(open), len(refused), capN, total-capN)
	}

	closed := make(chan struct{}, len(open))
	for _, c := range open {
		go func(c net.Conn) {
			_ = c.SetReadDeadline(time.Now().Add(6 * time.Second))
			_, _ = io.Copy(io.Discard, c)
			closed <- struct{}{}
		}(c)
	}
	for range open {
		select {
		case <-closed:
		case <-time.After(6 * time.Second):
			t.Fatalf("a stalled connection was not closed by the proxy within 6s")
		}
	}
	for _, c := range open {
		_ = c.Close()
	}
	for _, c := range refused {
		_ = c.Close()
	}
	deadline := time.Now().Add(6 * time.Second)
	for p.openTunnels.Load() != 0 || runtime.NumGoroutine() > base+8 {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("open tunnels = %d, goroutines = %d (base %d), want 0 and <= %d\n%s",
				p.openTunnels.Load(), runtime.NumGoroutine(), base, base+8, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("200s = %d, 503s = %d, base goroutines = %d, after = %d", len(open), len(refused), base, runtime.NumGoroutine())
}

// TestMITMConnect_RelayedFlowTakesNoSlot: mitmConnect takes no slot for a flow the relay already
// counted (resumed from its ask); the same call, not relayed, past the cap is refused and, with
// no decision to record, leaves no row.
func TestMITMConnect_RelayedFlowTakesNoSlot(t *testing.T) {
	for _, relayed := range []bool{true, false} {
		name := "not relayed"
		if relayed {
			name = "relayed"
		}
		t.Run(name, func(t *testing.T) {
			cu := captureUpstream(t, true, "ok")
			p, buf, caPEM := mitmProxy(t, "alert", cu.srv)
			p.maxTunnels = 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				p.mitmConnect(w, r, anthropicHost, 443, nil, relayed)
			}))
			t.Cleanup(srv.Close)

			c, resp := rawCONNECT(t, srv.Listener.Addr().String(), anthropicHost+":443")
			if !relayed {
				wantTunnelLimit(t, resp, anthropicHost)
				if rows := tunnelCapRows(t, buf); len(rows) != 0 {
					t.Errorf("rows = %+v, want none: a via_org-shaped call has no decision to record", rows)
				}
				return
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("CONNECT = %d, want 200 for a relayed flow past a zero cap", resp.StatusCode)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(caPEM) {
				t.Fatal("failed to add Wardyn CA to agent trust pool")
			}
			tc := tls.Client(c, &tls.Config{ServerName: anthropicHost, RootCAs: pool})
			if err := tc.Handshake(); err != nil {
				t.Fatalf("agent TLS handshake on a relayed flow: %v", err)
			}
			_ = tc.Close()
			if got := p.openTunnels.Load(); got != 0 {
				t.Errorf("open tunnels = %d, want 0: a relayed flow is not counted", got)
			}
		})
	}
}
