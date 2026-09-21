// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// h2peer_test.go is the rig for issue #359 (a TLS-terminating peer that
// answers HTTP/2 unconditionally to an HTTP/1.1 request) plus isH2Preface's
// own unit coverage. Kept reusable on purpose — the rig's two pieces (h2Peer,
// the CONNECT-tunnel stub) take no ALPN-specific assumptions, so issue #360
// can drive the same shape against a peer that DOES honour ALPN, or one that
// falls back to HTTP/1.1 when it is not offered h2.

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// h2Peer is a fake corp-upstream CONNECT proxy (plain TCP: "200 Connection
// Established", then a raw byte pipe) in front of a TLS listener that ALWAYS
// serves HTTP/2 via (&http2.Server{}).ServeConn, regardless of what ALPN the
// client offered — the field report's peer: a TLS terminator that speaks
// HTTP/2 even though this proxy's transport (mkTransport, ForceAttemptHTTP2
// false — issue #360) offers none.
type h2Peer struct {
	proxyLn net.Listener
}

func (h *h2Peer) addr() string { return h.proxyLn.Addr().String() }

// startH2Peer starts both listeners and their accept loops; both close via
// t.Cleanup.
func startH2Peer(t *testing.T) *h2Peer {
	t.Helper()
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}}
	tlsLn, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	t.Cleanup(func() { _ = tlsLn.Close() })
	go func() {
		for {
			c, err := tlsLn.Accept()
			if err != nil {
				return
			}
			go serveH2Preface(c)
		}
	}()

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = proxyLn.Close() })
	go func() {
		for {
			c, err := proxyLn.Accept()
			if err != nil {
				return
			}
			go serveConnectTunnel(c, tlsLn.Addr().String())
		}
	}()
	return &h2Peer{proxyLn: proxyLn}
}

// serveH2Preface completes the TLS handshake explicitly before handing the
// conn to http2.Server: tls.Listener.Accept returns a conn whose handshake is
// LAZY (it runs on first Read/Write), but http2.Server.ServeConn reads
// tls.Conn.ConnectionState() synchronously at the top of the call — before
// ever reading a byte — to enforce RFC 7540 §9.2's TLS-version floor. Against
// a not-yet-handshaked conn that state is the zero value (Version 0), which
// reads as "TLS version too low" and makes the server reject the connection
// with its OWN (different, misleading) GOAWAY before the scenario under test
// — an HTTP/2 answer to an HTTP/1.1 request — ever happens.
func serveH2Preface(c net.Conn) {
	if tc, ok := c.(*tls.Conn); ok {
		if err := tc.Handshake(); err != nil {
			return
		}
	}
	//lint:ignore SA1019 the deprecated explicit API is the only one that forces
	// h2 on a conn with NO ALPN negotiated — the exact shape under test; the
	// suggested replacement (http.Server.Serve/ServeTLS) negotiates HTTP/2 via
	// ALPN, which this rig must NOT do.
	(&http2.Server{}).ServeConn(c, &http2.ServeConnOpts{})
}

// serveConnectTunnel answers one CONNECT with "200 Connection Established"
// and pipes raw bytes to backendAddr — a corp proxy's tunnel shape, minus the
// TLS termination the peer past it does.
func serveConnectTunnel(c net.Conn, backendAddr string) {
	defer c.Close()
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		_, _ = c.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\n\r\n"))
		return
	}
	backend, err := net.Dial("tcp", backendAddr)
	if err != nil {
		return
	}
	defer backend.Close()
	if _, err := c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(backend, br); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, backend); done <- struct{}{} }()
	<-done
}

// selfSignedCert mints a throwaway leaf for h2Peer's TLS listener. The rig
// trusts it via testInsecureTLSConfig (InsecureSkipVerify), so the cert's
// name and issuer are irrelevant — a bare self-signed leaf is all a test peer
// needs.
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "h2peer-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// h2MismatchAttempts bounds awaitH2Mismatch's retry — see its doc comment for
// why a retry belongs in this rig at all.
const h2MismatchAttempts = 40

// awaitH2Mismatch calls attempt (one full request against a proxy backed by
// an h2Peer) until buf carries a builtin:upstream-protocol-mismatch decision
// or the budget above is spent, and returns the recorder that produced it.
//
// A retry belongs here because of a genuine, PRE-EXISTING race INSIDE
// golang.org/x/net/http2's own Server.ServeConn, confirmed by reproducing it
// against a bare http2.Server with no Wardyn code anywhere in the loop: the
// goroutine that writes the server's initial SETTINGS frame is scheduled
// independently of serve()'s own readPreface, which rejects a non-HTTP/2
// client (every real caller of this rig) and closes the conn — see
// server.go's serve()/scheduleFrameWrite/readPreface. On loopback, where both
// sides finish in low-single-digit microseconds, that race drops the SETTINGS
// bytes and delivers a bare connection reset instead, measured at roughly
// 30-60% of attempts. Neither delaying reads nor delaying the close narrows
// the window (both were tried against the bare reproduction above): the race
// is between two of x/net/http2's OWN goroutines, not anything this rig
// controls. Retrying a fresh connection is the ordinary answer to a genuine
// upstream race, and it keeps the assertions pointed at real net/http and
// x/net/http2 behaviour instead of a hand-rolled substitute for either.
func awaitH2Mismatch(t *testing.T, buf *bytes.Buffer, attempt func() *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	var rec *httptest.ResponseRecorder
	for i := 0; i < h2MismatchAttempts; i++ {
		rec = attempt()
		if hasDecision(buf, ruleSourceUpstreamProtocolMismatch) {
			return rec
		}
	}
	t.Fatalf("no %s decision after %d attempts against the h2Peer rig — sink:\n%s",
		ruleSourceUpstreamProtocolMismatch, h2MismatchAttempts, buf.String())
	return rec
}

// hasDecision reports whether any decision logged in buf carries ruleSource —
// awaitH2Mismatch's non-fatal probe; findDecision (proxy_test.go) is the
// fatal-on-miss assertion used once the retry above has already succeeded.
func hasDecision(buf *bytes.Buffer, ruleSource string) bool {
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var d egress.DecisionLog
		if json.Unmarshal([]byte(line), &d) == nil && d.RuleSource == ruleSource {
			return true
		}
	}
	return false
}

// TestUpstreamProtocolMismatch_MITM_AWSLane is the field report itself,
// end-to-end: the run's own SSO portal, MITM'd (mitm.go's serveMITMRequest ->
// forwardInspectedLLM), re-originated through a corp upstream to h2Peer.
// Expect builtin:upstream-protocol-mismatch (never builtin:dial-failed), a
// 400 (never the 500 an SDK retries ~26 times), and the AWS SDK JSON error
// shape.
func TestUpstreamProtocolMismatch_MITM_AWSLane(t *testing.T) {
	peer := startH2Peer(t)
	up, err := parseUpstreamProxy("http://" + peer.addr())
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)},
		Resolver:        publicResolver{},
		Upstream:        up,
		TLSClientConfig: testInsecureTLSConfig,
	})

	rec := awaitH2Mismatch(t, buf, func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.serveMITMRequest(rec, httptest.NewRequest(http.MethodPost, "https://"+awsHost+"/", nil), awsHost, 443)
		return rec
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("x-amzn-errortype"); got != "UpstreamProtocolMismatchException" {
		t.Errorf("x-amzn-errortype = %q, want UpstreamProtocolMismatchException", got)
	}
	body := decodeAWSSDKError(t, rec.Body.Bytes())
	if body["__type"] != "UpstreamProtocolMismatchException" {
		t.Errorf("__type = %q, want UpstreamProtocolMismatchException", body["__type"])
	}
	if !strings.Contains(body["message"], "peer answered HTTP/2") || !strings.Contains(body["message"], "ALPN: none") {
		t.Errorf("message = %q, want the h2-mismatch sentence with ALPN: none", body["message"])
	}

	d := findDecision(t, buf, ruleSourceUpstreamProtocolMismatch)
	if d.Via != viaUpstreamProxy {
		t.Errorf("via = %q, want %q", d.Via, viaUpstreamProxy)
	}
	if d.Cause != body["message"] {
		t.Errorf("sandbox message %q != decision log Cause %q — the sandbox and the audit row must agree", body["message"], d.Cause)
	}
}

// TestUpstreamProtocolMismatch_PlainForward pins plain_lane.go's own
// isH2Preface arm, reached through an ordinary absolute-URI forward
// (handlePlain), which is never AWS-lane classified: a plain 400 carrying the
// same sentence, through the same corp upstream and the same peer.
func TestUpstreamProtocolMismatch_PlainForward(t *testing.T) {
	peer := startH2Peer(t)
	up, err := parseUpstreamProxy("http://" + peer.addr())
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"h2.test"}}),
		Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)},
		Resolver:        publicResolver{},
		Upstream:        up,
		TLSClientConfig: testInsecureTLSConfig,
	})

	rec := awaitH2Mismatch(t, buf, func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "https://h2.test/thing"))
		return rec
	})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("x-amzn-errortype"); got != "" {
		t.Errorf("x-amzn-errortype = %q, want unset on a non-AWS lane", got)
	}
	if got := denyBody(rec); !strings.HasPrefix(got, "upstream error: peer answered HTTP/2") || !strings.Contains(got, "ALPN: none") {
		t.Errorf("body = %q, want the plain h2-mismatch sentence with ALPN: none", got)
	}

	d := findDecision(t, buf, ruleSourceUpstreamProtocolMismatch)
	if d.Via != viaUpstreamProxy {
		t.Errorf("via = %q, want %q", d.Via, viaUpstreamProxy)
	}
}

// TestIsH2Preface pins the detection function directly, including the exact
// field-report error string (copy-pasted from the report): a toolchain change
// that alters how net/http renders this failure should fail THIS test loudly,
// rather than silently falling back to builtin:dial-failed everywhere else.
func TestIsH2Preface(t *testing.T) {
	// The field report's own text: SETTINGS (type 0x04, stream 0), WINDOW_UPDATE,
	// GOAWAY PROTOCOL_ERROR last-stream-id=0 — a real HTTP/2 server connection
	// preface, byte for byte.
	fieldReportFrames := "\x00\x00\x12\x04\x00\x00\x00\x00\x00\x00\x03\x00\x00\x00\x80\x00\x04\x00\x01\x00\x00\x00\x05\x00\xff\xff\xff\x00\x00\x04\b\x00\x00\x00\x00\x00\x7f\xff\x00\x00\x00\x00\b\a\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01"

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{
			"field report: HTTP/1.x transport connection broken, malformed HTTP response",
			fmt.Errorf("net/http: HTTP/1.x transport connection broken: %w",
				fmt.Errorf("%s %q", "malformed HTTP response", fieldReportFrames)),
			true,
		},
		{
			"malformed HTTP response, but not an HTTP/2 frame header",
			fmt.Errorf("net/http: HTTP/1.x transport connection broken: %w",
				fmt.Errorf("%s %q", "malformed HTTP response", "not a status line at all\r\n")),
			false,
		},
		{
			"malformed HTTP response, too short to be a frame header",
			fmt.Errorf("net/http: HTTP/1.x transport connection broken: %w",
				fmt.Errorf("%s %q", "malformed HTTP response", "\x00\x00\x00\x04")),
			false,
		},
		{
			"a genuine dial error",
			&net.OpError{Op: "dial", Err: errors.New("connection refused")},
			false,
		},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isH2Preface(tc.err); got != tc.want {
				t.Errorf("isH2Preface(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
