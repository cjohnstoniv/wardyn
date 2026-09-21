// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// h2peer_test.go is the rig for issue #359 (a TLS-terminating peer that
// answers HTTP/2 unconditionally to an HTTP/1.1 request) plus isH2Preface's
// own unit coverage. The peer is DETERMINISTIC on purpose: it writes the
// field report's own frame bytes the moment its TLS handshake completes,
// unconditionally — no HTTP/2 server logic, no preface negotiation, and
// therefore no race to retry around. An earlier version drove a real
// golang.org/x/net/http2 Server here, which turned out to race ITS OWN
// initial-SETTINGS write against ITS OWN preface-rejection close (confirmed
// by reproducing the race against a bare http2.Server with no Wardyn code in
// the loop at all) — good for realism, bad for a `-count=20` gate.

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
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

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fieldReportFrames is the field report's own bytes, verbatim: a SETTINGS
// frame (type 0x04, stream 0), a WINDOW_UPDATE, and a GOAWAY with
// ErrCodeProtocol and last-stream-id 0 — kept as the ONE copy both the live
// rig (h2Peer) and TestIsH2Preface assert against, so the two can never drift
// apart into testing two different fixtures.
const fieldReportFrames = "\x00\x00\x12\x04\x00\x00\x00\x00\x00\x00\x03\x00\x00\x00\x80\x00\x04\x00\x01\x00\x00\x00\x05\x00\xff\xff\xff\x00\x00\x04\b\x00\x00\x00\x00\x00\x7f\xff\x00\x00\x00\x00\b\a\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01"

// h2Peer is a fake corp-upstream CONNECT proxy (plain TCP: "200 Connection
// Established", then a raw byte pipe) in front of a TLS listener that writes
// fieldReportFrames unconditionally, the instant its TLS handshake completes
// — the field report's own peer: a TLS terminator that speaks HTTP/2 even
// though this proxy's transport (mkTransport, ForceAttemptHTTP2 false — issue
// #360) offers no ALPN at all.
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
			go serveH2Frames(c)
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

// serveH2Frames is the deterministic field-report peer: complete the TLS
// handshake, read the client's request (or hit a short deadline), then write
// fieldReportFrames UNCONDITIONALLY — no preface check — and close.
//
// Reading first matters twice. It is the reported peer's order (a GOAWAY with
// last-stream-id 0 answers a preface it has already read), and frames written
// before the client has counted its request as outstanding are dropped by
// net/http as an unsolicited response on an idle connection, with no bytes in
// the error. Draining before the close also keeps it a FIN rather than a RST
// that could discard the frames just written.
func serveH2Frames(c net.Conn) {
	defer c.Close()
	tc, ok := c.(*tls.Conn)
	if !ok {
		return
	}
	if err := tc.Handshake(); err != nil {
		return
	}
	_ = tc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 4096)
	_, _ = tc.Read(buf)
	_, _ = tc.Write([]byte(fieldReportFrames))
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

	rec := httptest.NewRecorder()
	p.serveMITMRequest(rec, httptest.NewRequest(http.MethodPost, "https://"+awsHost+"/", nil), awsHost, 443)

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
// (handlePlain): a plain 400 carrying the same sentence, through the same
// corp upstream and the same peer. writeUpstreamProtocolMismatch still
// evaluates isAWSLane on THIS lane too — the plain body here is because
// "h2.test" is not an AWS-lane host, not because the plain lane skips the
// check (see TestUpstreamProtocolMismatch_MITM_AWSLane for the AWS-lane
// twin of this exact code path).
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

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "https://h2.test/thing"))

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

// TestIsH2Preface pins the detection function against Go's OWN parse error,
// not a hand-typed imitation of it: http.ReadResponse is exactly what
// net/http/transport.go's readLoop calls under the hood (wrapping whatever it
// returns as "net/http: HTTP/1.x transport connection broken: %w"), so
// driving it directly on fieldReportFrames pins isH2Preface against
// whatever THIS Go toolchain actually produces. A future wording change in
// net/http fails this test loudly instead of silently falling back to
// builtin:dial-failed everywhere else.
func TestIsH2Preface(t *testing.T) {
	_, fieldReportErr := http.ReadResponse(bufio.NewReader(strings.NewReader(fieldReportFrames)), nil)
	if fieldReportErr == nil {
		t.Fatal("http.ReadResponse accepted the field report's frame bytes as a valid response — the fixture no longer reproduces a parse failure")
	}

	// KNOWN MISS, recorded rather than hidden: response.go's line-parser cuts
	// the first line on the first SPACE byte, before it ever gets to
	// "malformed HTTP response". A SETTINGS frame whose payload happens to
	// carry a 0x20 byte before any 0x0a — here, INITIAL_WINDOW_SIZE (setting
	// id 0x0004) set to 0x00200000 — instead trips response.go's "malformed
	// HTTP status code" arm, carrying only a short fragment of the line, not
	// the frame header isH2Preface needs. Detection from the response error
	// text cannot see this shape; a post-TLS byte sniff that does not depend
	// on net/http's parse error at all is tracked as issue #360's fix for it
	// (see isH2Preface's own doc comment, upstream_protocol.go).
	spaceInPayload := []byte{
		0x00, 0x00, 0x06, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, // SETTINGS header, 1 setting (6-byte payload)
		0x00, 0x04, 0x00, 0x20, 0x00, 0x00, // INITIAL_WINDOW_SIZE = 0x00200000
	}
	_, spaceErr := http.ReadResponse(bufio.NewReader(bytes.NewReader(spaceInPayload)), nil)
	if spaceErr == nil {
		t.Fatal("http.ReadResponse accepted the space-in-payload fixture as a valid response — it no longer demonstrates the known miss")
	}

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"field report: Go's own parse error on the report's exact bytes", fieldReportErr, true},
		{"known miss: a space byte before any newline reads as a status-code error, not frame bytes", spaceErr, false},
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
