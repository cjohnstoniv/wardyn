// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// tunnelFailedSource is the rule_source a tunnel that died after its 200
// carries. Spelled out here, not taken from the package constant, so the row
// is pinned by its wire value.
const tunnelFailedSource = "builtin:tunnel-failed"

// fakeClientHello opens a tunnel the way a TLS client does: a handshake record
// header (0x16, then the version) and a body. clientMarker is what the cause
// assertions look for, since no client byte may reach an audit row.
var fakeClientHello = append([]byte{0x16, 0x03, 0x01, 0x00, 0x05}, clientMarker...)

const clientMarker = "q7zXk"

// plaintextAlert is a fatal handshake_failure (40) alert record, unencrypted.
var plaintextAlert = []byte{0x15, 0x03, 0x03, 0x00, 0x02, 0x02, 40}

type hostileMode int

const (
	// hostileHTTPAfterHello acknowledges the CONNECT, then answers the
	// client's hello with a plaintext HTTP 503.
	hostileHTTPAfterHello hostileMode = iota
	// hostileDirectHTTPAfterHello is the same answer from the destination
	// itself: there is no CONNECT to read, because nothing proxies the dial.
	hostileDirectHTTPAfterHello
	// hostileCloseAfterHello acknowledges the CONNECT, then closes as soon as
	// the client speaks.
	hostileCloseAfterHello
	// hostileDeclaredBody declares a body on its 200, sends it, then behaves:
	// it completes a TLS handshake and writes "pong".
	hostileDeclaredBody
	// hostileAlert acknowledges the CONNECT, then answers the hello with a
	// plaintext TLS alert and closes.
	hostileAlert
	// hostilePipelinedHTTP sends an HTTP 503 in the same flight as its 200,
	// before the client has sent a byte.
	hostilePipelinedHTTP
)

const hostile503 = "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n"

// startHostileUpstream runs a loopback hop that acknowledges a CONNECT and
// then misbehaves as mode says, returning its address.
func startHostileUpstream(t *testing.T, mode hostileMode) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cert := selfSignedCert(t)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go serveHostile(c, mode, cert)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().String()
}

func serveHostile(c net.Conn, mode hostileMode, cert tls.Certificate) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(c)
	if mode != hostileDirectHTTPAfterHello {
		if _, err := http.ReadRequest(br); err != nil {
			return
		}
		ack := "HTTP/1.1 200 Connection Established\r\n\r\n"
		switch mode {
		case hostileDeclaredBody:
			ack = "HTTP/1.1 200 Connection Established\r\nContent-Length: 5\r\n\r\nhello"
		case hostilePipelinedHTTP:
			ack += hostile503
		}
		if _, err := io.WriteString(c, ack); err != nil {
			return
		}
	}
	if mode == hostileDeclaredBody {
		tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
		if _, err := io.WriteString(tc, "pong"); err != nil {
			return
		}
		_, _ = io.Copy(io.Discard, tc) // until the client closes
		return
	}
	// Read the client's whole flight before answering: closing with it unread
	// resets the connection, and a reset can overtake the answer.
	if _, err := io.ReadFull(br, make([]byte, len(fakeClientHello))); err != nil {
		return
	}
	switch mode {
	case hostileHTTPAfterHello, hostileDirectHTTPAfterHello:
		_, _ = io.WriteString(c, hostile503)
	case hostileAlert:
		_, _ = c.Write(plaintextAlert)
	}
}

// serveTunnelProxy serves p and reports each finished request on done. A
// CONNECT handler returns only once its tunnel has ended and its rows are
// written, which httptest.Server.Close does not wait for on a hijacked
// connection.
func serveTunnelProxy(t *testing.T, p *Proxy) (proxyURL string, done <-chan struct{}) {
	t.Helper()
	ch := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.ServeHTTP(w, r)
		ch <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, ch
}

// awaitDecision waits for the CONNECT handler to finish, then returns every
// row the proxy emitted, read off the sink's own channel: these sinks are
// hand-built, so nothing else drains it.
func awaitDecision(t *testing.T, p *Proxy, done <-chan struct{}) []egress.DecisionLog {
	t.Helper()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the CONNECT handler did not return within 10s of the tunnel ending")
	}
	var rows []egress.DecisionLog
	for {
		select {
		case d := <-p.sink.ch:
			rows = append(rows, d)
		default:
			return rows
		}
	}
}

// tunnelFailedRows is the subset of rows carrying the tunnel-failed source.
func tunnelFailedRows(rows []egress.DecisionLog) []egress.DecisionLog {
	var out []egress.DecisionLog
	for _, d := range rows {
		if d.RuleSource == tunnelFailedSource {
			out = append(out, d)
		}
	}
	return out
}

// hostileProxy builds the proxy under test: chained through the hostile hop,
// or dialling it directly as the destination.
func hostileProxy(t *testing.T, mode hostileMode) *Proxy {
	t.Helper()
	addr := startHostileUpstream(t, mode)
	if mode == hostileDirectHTTPAfterHello {
		p, _ := newTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"tls.test"}}, addr, nil, nil)
		return p
	}
	up, err := parseUpstreamProxy("http://" + addr)
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	return newUpstreamProxy(t, up)
}

// openTunnel issues the CONNECT and requires the proxy's 200.
func openTunnel(t *testing.T, proxyURL string) net.Conn {
	t.Helper()
	conn, resp := connectThrough(t, proxyURL, "tls.test:443")
	t.Cleanup(func() { _ = conn.Close() })
	if !strings.HasPrefix(resp, "HTTP/1.1 200") {
		t.Fatalf("CONNECT answer = %q, want the proxy's 200", resp)
	}
	return conn
}

// readToEnd returns everything the tunnel delivers before it ends, and fails
// if the tunnel is still open after 5s.
func readToEnd(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(conn)
	if isTimeout(err) {
		t.Fatalf("the tunnel was still open 5s after the upstream misbehaved (read %q)", got)
	}
	return got
}

// TestHostileUpstream pins what a client and the audit see when a CONNECT
// tunnel is acknowledged and then dies: nothing that is not TLS is relayed
// into a tunnel the client opened with a TLS hello, and the run's audit gets
// exactly one deny row naming what happened and which hop it happened on.
func TestHostileUpstream(t *testing.T) {
	for _, c := range []struct {
		name       string
		mode       hostileMode
		wantClient []byte
		wantVia    string
		wantCause  string
		notInCause string
	}{
		{"a_http_after_hello", hostileHTTPAfterHello, nil, viaUpstreamProxy, "HTTP 503 (Service Unavailable)", ""},
		{"b_close_after_hello", hostileCloseAfterHello, nil, viaUpstreamProxy, "the upstream closed without answering", ""},
		// The destination's own reason phrase is not the operator's text.
		{"d_direct_http_after_hello", hostileDirectHTTPAfterHello, nil, viaDirect, "HTTP 503", "Service Unavailable"},
		{"e_plaintext_alert", hostileAlert, plaintextAlert, viaUpstreamProxy, "alert 40", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := hostileProxy(t, c.mode)
			proxyURL, done := serveTunnelProxy(t, p)
			conn := openTunnel(t, proxyURL)
			if _, err := conn.Write(fakeClientHello); err != nil {
				t.Fatalf("write hello: %v", err)
			}
			if got := readToEnd(t, conn); !bytes.Equal(got, c.wantClient) {
				t.Errorf("client received %q, want %q", got, c.wantClient)
			}

			rows := tunnelFailedRows(awaitDecision(t, p, done))
			if len(rows) != 1 {
				t.Fatalf("%s rows = %d, want exactly 1", tunnelFailedSource, len(rows))
			}
			d := rows[0]
			if d.Decision != egress.Deny {
				t.Errorf("decision = %q, want deny", d.Decision)
			}
			if d.Via != c.wantVia {
				t.Errorf("via = %q, want %q", d.Via, c.wantVia)
			}
			if !strings.Contains(d.Cause, c.wantCause) {
				t.Errorf("cause = %q, want it to contain %q", d.Cause, c.wantCause)
			}
			if c.notInCause != "" && strings.Contains(d.Cause, c.notInCause) {
				t.Errorf("cause = %q, must not contain %q", d.Cause, c.notInCause)
			}
			if strings.Contains(d.Cause, clientMarker) {
				t.Errorf("cause = %q carries a client byte", d.Cause)
			}
		})
	}
}

// TestHostileUpstreamDeclaredBodyNeverReachesClient is case c: a hop that
// declares a body on its 200 is not hostile. The body is discarded with the
// response, the tunnel carries a real TLS session, and a healthy close writes
// no tunnel-failed row.
func TestHostileUpstreamDeclaredBodyNeverReachesClient(t *testing.T) {
	p := hostileProxy(t, hostileDeclaredBody)
	proxyURL, done := serveTunnelProxy(t, p)
	conn := openTunnel(t, proxyURL)

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	tc := tls.Client(conn, &tls.Config{InsecureSkipVerify: true})
	got := make([]byte, 4)
	if _, err := io.ReadFull(tc, got); err != nil {
		t.Fatalf("TLS through the tunnel: %v (a relayed body breaks the handshake)", err)
	}
	if string(got) != "pong" {
		t.Errorf("read %q through the tunnel, want %q", got, "pong")
	}
	_ = conn.Close()

	if rows := tunnelFailedRows(awaitDecision(t, p, done)); len(rows) != 0 {
		t.Errorf("a healthy tunnel wrote %d %s rows (%+v), want none", len(rows), tunnelFailedSource, rows)
	}
}

// TestHostileUpstreamPipelinedHTTPIsRefused: a hop that sends an HTTP response
// in the same flight as its 200 is refused although the client has sent
// nothing, so no hello armed the guard.
func TestHostileUpstreamPipelinedHTTPIsRefused(t *testing.T) {
	p := hostileProxy(t, hostilePipelinedHTTP)
	proxyURL, done := serveTunnelProxy(t, p)
	conn := openTunnel(t, proxyURL)
	if got := readToEnd(t, conn); len(got) != 0 {
		t.Errorf("client received %q, want nothing", got)
	}
	rows := tunnelFailedRows(awaitDecision(t, p, done))
	if len(rows) != 1 {
		t.Fatalf("%s rows = %d, want exactly 1", tunnelFailedSource, len(rows))
	}
	if d := rows[0]; d.Via != viaUpstreamProxy || !strings.Contains(d.Cause, "HTTP 503 (Service Unavailable) before the client sent anything") {
		t.Errorf("row = via %q cause %q, want the upstream proxy's unprompted 503", d.Via, d.Cause)
	}
}

// TestTunnelAllowRowNamesTheHop: the allow a CONNECT tunnel is recorded with
// says which hop carried it, and a tunnel the client never wrote into leaves
// no tunnel-failed row behind.
func TestTunnelAllowRowNamesTheHop(t *testing.T) {
	for _, c := range []struct {
		name string
		mode hostileMode
		via  string
	}{
		{"upstream", hostileCloseAfterHello, viaUpstreamProxy},
		{"direct", hostileDirectHTTPAfterHello, viaDirect},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := hostileProxy(t, c.mode)
			proxyURL, done := serveTunnelProxy(t, p)
			_ = openTunnel(t, proxyURL).Close()

			rows := awaitDecision(t, p, done)
			if failed := tunnelFailedRows(rows); len(failed) != 0 {
				t.Errorf("a tunnel the client never wrote into left %+v", failed)
			}
			var allows []egress.DecisionLog
			for _, d := range rows {
				if d.RuleSource == "policy:allowed" {
					allows = append(allows, d)
				}
			}
			if len(allows) != 1 || allows[0].Via != c.via {
				t.Errorf("CONNECT allows = %+v, want one via %q", allows, c.via)
			}
		})
	}
}

// pipedTunnel runs tunnel over in-memory pipes with the watch on the upstream
// side, returning the two far ends.
func pipedTunnel(t *testing.T) (w *tunnelWatch, clientPeer, upstreamPeer net.Conn) {
	t.Helper()
	client, clientPeer := net.Pipe()
	upstream, upstreamPeer := net.Pipe()
	w = newTunnelWatch(upstream)
	go func() { _, _ = tunnel(client, w) }()
	t.Cleanup(func() { _ = clientPeer.Close(); _ = upstreamPeer.Close() })
	return w, clientPeer, upstreamPeer
}

// relayThrough writes msg at from and requires it to arrive at to, unchanged.
func relayThrough(t *testing.T, from, to net.Conn, msg string) {
	t.Helper()
	go func() { _, _ = from.Write([]byte(msg)) }()
	_ = to.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(to, got); err != nil || string(got) != msg {
		t.Fatalf("relayed %q (err %v), want %q", got, err, msg)
	}
}

// TestTunnelWatchLeavesNonTLSTunnelsAlone: the guard is armed by a TLS hello
// and by nothing else, so the tunnels that are not TLS behave as they did.
func TestTunnelWatchLeavesNonTLSTunnelsAlone(t *testing.T) {
	// ssh over 443: the server speaks first, and what it says is not TLS.
	t.Run("ssh_server_speaks_first", func(t *testing.T) {
		w, clientPeer, upstreamPeer := pipedTunnel(t)
		relayThrough(t, upstreamPeer, clientPeer, "SSH-2.0-OpenSSH_9.6\r\n")
		relayThrough(t, clientPeer, upstreamPeer, "SSH-2.0-client\r\n")
		if got := w.failure(viaDirect, true, nil); got != "" {
			t.Errorf("failure = %q, want none for a tunnel that carried an answer", got)
		}
	})
	// Plain HTTP over CONNECT: an HTTP answer to an HTTP request is the
	// protocol working, not a hop repeating itself.
	t.Run("plain_http", func(t *testing.T) {
		w, clientPeer, upstreamPeer := pipedTunnel(t)
		relayThrough(t, clientPeer, upstreamPeer, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
		relayThrough(t, upstreamPeer, clientPeer, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n")
		if got := w.failure(viaDirect, true, nil); got != "" {
			t.Errorf("failure = %q, want none for a tunnel that carried an answer", got)
		}
	})
	// A TLS session: records pass in both directions once the first is vetted.
	t.Run("tls_records", func(t *testing.T) {
		w, clientPeer, upstreamPeer := pipedTunnel(t)
		relayThrough(t, clientPeer, upstreamPeer, string(fakeClientHello))
		relayThrough(t, upstreamPeer, clientPeer, "\x16\x03\x03\x00\x02ok")
		relayThrough(t, upstreamPeer, clientPeer, "anything after the first record is not inspected")
		if got := w.failure(viaDirect, true, nil); got != "" {
			t.Errorf("failure = %q, want none for a tunnel that carried an answer", got)
		}
	})
}

// TestTunnelWatchRefusesAShortFirstRecord: an upstream that closes before a
// whole record header has none of its bytes relayed into a TLS tunnel.
func TestTunnelWatchRefusesAShortFirstRecord(t *testing.T) {
	w, clientPeer, upstreamPeer := pipedTunnel(t)
	relayThrough(t, clientPeer, upstreamPeer, string(fakeClientHello))
	go func() {
		_, _ = upstreamPeer.Write([]byte{0x16, 0x03})
		_ = upstreamPeer.Close()
	}()
	_ = clientPeer.SetReadDeadline(time.Now().Add(5 * time.Second))
	if got, err := io.ReadAll(clientPeer); len(got) != 0 || isTimeout(err) {
		t.Fatalf("client read %q (err %v), want nothing and a closed tunnel", got, err)
	}
	if got := w.failure(viaDirect, true, nil); got != "tunnel first bytes: the upstream closed without answering" {
		t.Errorf("failure = %q", got)
	}
}
