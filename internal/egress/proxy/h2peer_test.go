// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// h2peer_test.go is the rig for issues #359 and #360: TLS peers behind a fake
// corp CONNECT proxy that speak HTTP/2 negotiated, unasked, or not at all,
// plus isH2Preface's own unit coverage. Every peer is deterministic: the
// field-report peer (serveH2Frames) and the settings-first peer
// (serveSettingsFirst) write raw frames at fixed points in the exchange, and
// the x/net server (serveH2Unasked) is only ever spoken to in HTTP/2 — its
// known race (closing before its SETTINGS flush on a bad client preface) needs
// an HTTP/1.1 client, which the post-handshake sniff never lets through.

import (
	"bufio"
	"bytes"
	"context"
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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fieldReportFrames is the field report's own bytes, verbatim: a SETTINGS
// frame (type 0x04, stream 0), a WINDOW_UPDATE, and a GOAWAY with
// ErrCodeProtocol and last-stream-id 0 — kept as the ONE copy both the live
// rig (h2Peer) and TestIsH2Preface assert against, so the two can never drift
// apart into testing two different fixtures.
const fieldReportFrames = "\x00\x00\x12\x04\x00\x00\x00\x00\x00\x00\x03\x00\x00\x00\x80\x00\x04\x00\x01\x00\x00\x00\x05\x00\xff\xff\xff\x00\x00\x04\b\x00\x00\x00\x00\x00\x7f\xff\x00\x00\x00\x00\b\a\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01"

// spaceInPayloadSettings is a SETTINGS frame whose payload carries 0x20
// before any 0x0a: INITIAL_WINDOW_SIZE (setting id 0x0004) = 0x00200000.
var spaceInPayloadSettings = []byte{
	0x00, 0x00, 0x06, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, // SETTINGS header, 1 setting (6-byte payload)
	0x00, 0x04, 0x00, 0x20, 0x00, 0x00, // INITIAL_WINDOW_SIZE = 0x00200000
}

// h2Peer is a fake corp-upstream CONNECT proxy (plain TCP: "200 Connection
// Established", then a raw byte pipe) in front of a TLS peer. connects counts
// tunnels; it is bumped before the tunnel answers 200, so it has already moved
// by the time the proxy's dial returns — a deterministic count of how many
// connections a request cost.
type h2Peer struct {
	proxyLn  net.Listener
	connects atomic.Int32
}

func (h *h2Peer) addr() string { return h.proxyLn.Addr().String() }

// startH2Peer is the field report's peer (serveH2Frames) behind a tunnel.
func startH2Peer(t *testing.T) *h2Peer {
	t.Helper()
	return startTunnel(t, startTLSPeer(t, &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}}, serveH2Frames))
}

// startH2MismatchPeer is a bare (no CONNECT tunnel) field-report peer for the
// two git brokers (#382): they dial straight to the vetted target, the same
// shape newGitBrokerUpstream's TLS server takes, so the mismatch peer needs no
// tunnel in front of it either.
func startH2MismatchPeer(t *testing.T) string {
	t.Helper()
	return startTLSPeer(t, &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}}, serveH2Frames)
}

// splitDial routes a dial by the vetted target's PORT rather than discarding
// it the way redirectDial does: port 443 (the forge, both brokers'
// p.egressTarget(host, 443)) goes to forgeAddr, anything else (the
// control-plane mint call, ControlPlaneURL's own port) goes to controlAddr.
// This is what lets one test proxy hold a normal mint server AND a
// broken-HTTP/2 forge peer at once, where redirectDial's single fixed
// address cannot tell the two calls apart.
func splitDial(controlAddr, forgeAddr string) func(ctx context.Context, network, target string) (net.Conn, error) {
	return func(ctx context.Context, network, target string) (net.Conn, error) {
		addr := controlAddr
		if _, port, err := net.SplitHostPort(target); err == nil && port == "443" {
			addr = forgeAddr
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, addr)
	}
}

// startTLSPeer runs a TLS listener with cfg and hands each handshaken conn to
// serve, closing it afterwards. A cfg without NextProtos negotiates no ALPN
// whatever the client offers.
func startTLSPeer(t *testing.T, cfg *tls.Config, serve func(*tls.Conn)) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				tc := c.(*tls.Conn)
				if tc.Handshake() == nil {
					serve(tc)
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// startTunnel starts a fake corp CONNECT proxy piping every tunnel to
// backendAddr.
func startTunnel(t *testing.T, backendAddr string) *h2Peer {
	t.Helper()
	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = proxyLn.Close() })
	h := &h2Peer{proxyLn: proxyLn}
	go func() {
		for {
			c, err := proxyLn.Accept()
			if err != nil {
				return
			}
			h.connects.Add(1)
			go serveConnectTunnel(c, backendAddr)
		}
	}()
	return h
}

// serveH2Frames is the deterministic field-report peer: it reads the client's
// request first — an HTTP/1.1 request, or an HTTP/2 client's preface and
// frames up to its first HEADERS — so it never speaks first and the proxy's
// post-handshake sniff sees nothing (this is the error-text path, isH2Preface),
// then writes fieldReportFrames and drains before closing.
//
// Waiting for HEADERS on the HTTP/2 resend is what makes that resend fail the
// same way every time: the GOAWAY (last-stream-id 0, PROTOCOL_ERROR) lands on
// an open stream 1, which x/net refuses to retry, instead of racing the
// stream's creation into x/net's retry-with-backoff path.
//
// The drain matters: closing while the client's bytes still sit unread in the
// kernel's receive buffer can turn the close into a RST, which on some stacks
// discards this peer's own just-written frames along with it.
func serveH2Frames(tc *tls.Conn) {
	br := bufio.NewReader(tc)
	if !awaitRequest(br) {
		return
	}
	if _, err := tc.Write([]byte(fieldReportFrames)); err != nil {
		return
	}
	drain(tc, br)
}

// awaitRequest reads the client's request and reports whether one arrived: an
// HTTP/1.1 request, or an HTTP/2 client's preface and frames up to its first
// HEADERS.
func awaitRequest(br *bufio.Reader) bool {
	if pre, err := br.Peek(len(http2.ClientPreface)); err == nil && string(pre) == http2.ClientPreface {
		_, _ = br.Discard(len(pre))
		fr := http2.NewFramer(io.Discard, br)
		for {
			f, err := fr.ReadFrame()
			if err != nil {
				return false
			}
			if _, ok := f.(*http2.HeadersFrame); ok {
				return true
			}
		}
	}
	_, err := http.ReadRequest(br)
	return err == nil
}

func drain(tc *tls.Conn, br *bufio.Reader) {
	_ = tc.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	_, _ = io.Copy(io.Discard, br)
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
// forwardInspectedLLM), re-originated through a corp upstream to h2Peer,
// which cannot serve HTTP/2 either, so the resend fails too. Expect
// builtin:upstream-protocol-mismatch (never builtin:dial-failed), a 400
// (never the 500 an SDK retries ~26 times), the AWS SDK JSON error shape, and
// a cause naming both attempts.
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
		ControlTLS:      testInsecureTLSConfig,
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
	if !strings.Contains(body["message"], "peer answered HTTP/2") || !strings.Contains(body["message"], "ALPN: none") ||
		!strings.Contains(body["message"], "the HTTP/2 resend also failed") {
		t.Errorf("message = %q, want the h2-mismatch sentence with ALPN: none and the failed resend", body["message"])
	}

	d := findDecision(t, buf, ruleSourceUpstreamProtocolMismatch)
	if d.Via != viaUpstreamProxy {
		t.Errorf("via = %q, want %q", d.Via, viaUpstreamProxy)
	}
	if d.Cause != body["message"] {
		t.Errorf("sandbox message %q != decision log Cause %q — the sandbox and the audit row must agree", body["message"], d.Cause)
	}
}

// TestUpstreamProtocolMismatch_PlainForward pins the plain lane's
// protocol-mismatch refusal, reached through an ordinary absolute-URI forward
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
		ControlTLS:      testInsecureTLSConfig,
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

	// A SETTINGS payload with a 0x20 byte before any 0x0a trips response.go's
	// "malformed HTTP status code" arm instead, carrying only a line fragment:
	// the error text cannot show it (isH2Preface's doc comment), so detection
	// of that shape is the post-handshake byte read, which sees the frame
	// header itself (end to end: TestH2Fallback_PeerWritesSettingsFirst).
	_, spaceErr := http.ReadResponse(bufio.NewReader(bytes.NewReader(spaceInPayloadSettings)), nil)
	if spaceErr == nil || strings.Contains(spaceErr.Error(), "malformed HTTP response") {
		t.Fatalf("http.ReadResponse on the space-in-payload fixture = %v, want the status-code parse error the byte read exists for", spaceErr)
	}
	if !isH2FrameHeader(spaceInPayloadSettings) {
		t.Error("isH2FrameHeader missed the space-in-payload SETTINGS frame")
	}

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"field report: Go's own parse error on the report's exact bytes", fieldReportErr, true},
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

// h2Counts is a peer-side tally of the requests a test peer served, by
// protocol. Each count moves before the response is written, so it has moved
// by the time the proxy returns that response.
type h2Counts struct{ h1, h2 atomic.Int32 }

func (c *h2Counts) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor == 2 {
			c.h2.Add(1)
		} else {
			c.h1.Add(1)
		}
		body, _ := io.ReadAll(r.Body)
		if len(body) == 0 {
			body = []byte("ok")
		}
		_, _ = w.Write(body)
	})
}

// noALPNConfig is a server TLS config with no NextProtos: whatever the client
// offers, the handshake negotiates no ALPN protocol.
func noALPNConfig(t *testing.T) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}}
}

// serveH2Unasked serves real HTTP/2 (x/net's server) on a conn that negotiated
// no ALPN: the operator's peer, minus the frames it sent because it was spoken
// to in HTTP/1.1. x/net's server queues its SETTINGS before reading anything.
func serveH2Unasked(c *h2Counts) func(*tls.Conn) {
	//lint:ignore SA1019 net/http's server, like its client, speaks HTTP/2 over TLS only after ALPN selected h2
	srv := &http2.Server{}
	return func(tc *tls.Conn) {
		//lint:ignore SA1019 see above
		srv.ServeConn(tc, &http2.ServeConnOpts{Handler: c.handler()})
	}
}

// serveSettingsFirst writes settings (a SETTINGS frame) the instant the
// handshake completes, synchronously and before reading a byte, then answers
// each HTTP/2 request with 200 "ok" by hand. It is the deterministic form of a
// peer whose SETTINGS ride the same flight as the end of the handshake —
// the shape net/http drops as "Unsolicited response received on idle HTTP
// channel" with no bytes in the error.
func serveSettingsFirst(settings []byte, c *h2Counts) func(*tls.Conn) {
	return func(tc *tls.Conn) {
		if _, err := tc.Write(settings); err != nil {
			return
		}
		pre := make([]byte, len(http2.ClientPreface))
		if _, err := io.ReadFull(tc, pre); err != nil || string(pre) != http2.ClientPreface {
			return
		}
		fr := http2.NewFramer(tc, tc)
		var block bytes.Buffer
		enc := hpack.NewEncoder(&block)
		for {
			f, err := fr.ReadFrame()
			if err != nil {
				return
			}
			switch f := f.(type) {
			case *http2.SettingsFrame:
				if !f.IsAck() {
					_ = fr.WriteSettingsAck()
				}
			case *http2.HeadersFrame:
				c.h2.Add(1)
				block.Reset()
				_ = enc.WriteField(hpack.HeaderField{Name: ":status", Value: "200"})
				_ = fr.WriteHeaders(http2.HeadersFrameParam{StreamID: f.StreamID, BlockFragment: block.Bytes(), EndHeaders: true})
				_ = fr.WriteData(f.StreamID, true, []byte("ok"))
			}
		}
	}
}

// newH2TestProxy is a proxy chained through peer's tunnel that may reach host.
func newH2TestProxy(t *testing.T, peer *h2Peer, host string) (*Proxy, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	return newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{host}}),
		Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)},
		Resolver:        publicResolver{},
		Upstream:        mustUpstream(t, peer.addr()),
		TLSClientConfig: testInsecureTLSConfig,
		ControlTLS:      testInsecureTLSConfig,
	}), buf
}

// forwardGet sends GET https://h2.test/thing through the plain lane and
// requires a 200 "ok" with an allow row and no protocol-mismatch row.
func forwardGet(t *testing.T, p *Proxy, buf *bytes.Buffer) {
	t.Helper()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "https://h2.test/thing"))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("status = %d body %q, want 200 ok", rec.Code, rec.Body.String())
	}
	if d := lastDecision(t, buf); d.Decision != egress.Allow {
		t.Fatalf("last decision = %+v, want an allow", d)
	}
	if strings.Contains(buf.String(), ruleSourceUpstreamProtocolMismatch) {
		t.Fatalf("a request that completed left a protocol-mismatch row: %s", buf.String())
	}
}

// TestH2_ALPNNegotiated is acceptance (a): a peer that honours ALPN and serves
// HTTP/2 gets HTTP/2 from the egress transport, on one connection.
func TestH2_ALPNNegotiated(t *testing.T) {
	var c h2Counts
	srv := httptest.NewUnstartedServer(c.handler())
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	peer := startTunnel(t, srv.Listener.Addr().String())
	p, buf := newH2TestProxy(t, peer, "h2.test")

	forwardGet(t, p, buf)
	if c.h2.Load() != 1 || c.h1.Load() != 0 {
		t.Fatalf("peer served h1=%d h2=%d, want the request over HTTP/2", c.h1.Load(), c.h2.Load())
	}
	if got := peer.connects.Load(); got != 1 {
		t.Fatalf("connects = %d, want 1 (negotiated h2 needs no fallback)", got)
	}
	// Enabling HTTP/2 edits the egress transport's own TLS config; the shared
	// base (also controlTransport's, which stays HTTP/1.1) must be untouched.
	if len(testInsecureTLSConfig.NextProtos) != 0 {
		t.Fatalf("shared TLS config NextProtos = %v, want untouched", testInsecureTLSConfig.NextProtos)
	}
}

// TestH2Fallback_UnaskedPeer_MITM_AWSLane is acceptance (b) on the field
// report's own lane: a peer that speaks HTTP/2 without negotiating it is
// resent over HTTP/2 and allowed, and the next request to it goes straight to
// the HTTP/2 transport: no new tunnel, no probe connection.
func TestH2Fallback_UnaskedPeer_MITM_AWSLane(t *testing.T) {
	var c h2Counts
	peer := startTunnel(t, startTLSPeer(t, noALPNConfig(t), serveH2Unasked(&c)))
	p, buf := newH2TestProxy(t, peer, awsHost)

	for i, wantConnects := range []int32{2, 2} {
		rec := httptest.NewRecorder()
		p.serveMITMRequest(rec, httptest.NewRequest(http.MethodGet, "https://"+awsHost+"/federation/credentials", nil), awsHost, 443)
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Fatalf("request %d: status = %d body %q, want 200 ok", i+1, rec.Code, rec.Body.String())
		}
		if got := peer.connects.Load(); got != wantConnects {
			t.Fatalf("request %d: connects = %d, want %d (first: the sniffed connection + the HTTP/2 one; then the pooled HTTP/2 one)", i+1, got, wantConnects)
		}
		if d := lastDecision(t, buf); d.Decision != egress.Allow {
			t.Fatalf("request %d: last decision = %+v, want an allow", i+1, d)
		}
	}
	if c.h2.Load() != 2 || c.h1.Load() != 0 {
		t.Fatalf("peer served h1=%d h2=%d, want both requests over HTTP/2", c.h1.Load(), c.h2.Load())
	}
	if n := strings.Count(buf.String(), `"Host":"`+awsHost+`"`); n != 2 {
		t.Fatalf("decision rows for %s = %d, want exactly one per request: %s", awsHost, n, buf.String())
	}
}

// TestH2Fallback_StreamedBodyIsResent is acceptance (c): a streamed body has
// no GetBody, but a peer caught by the post-handshake sniff failed the DIAL —
// nothing was written and nothing was read — so the first request is resent
// over HTTP/2 with that same body, intact.
func TestH2Fallback_StreamedBodyIsResent(t *testing.T) {
	var c h2Counts
	peer := startTunnel(t, startTLSPeer(t, noALPNConfig(t), serveH2Unasked(&c)))
	p, buf := newH2TestProxy(t, peer, awsHost)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "https://"+awsHost+"/", io.NopCloser(strings.NewReader(`{"a":1}`)))
	p.serveMITMRequest(rec, r, awsHost, 443)

	if rec.Code != http.StatusOK || rec.Body.String() != `{"a":1}` {
		t.Fatalf("status = %d body %q, want 200 with the body echoed back", rec.Code, rec.Body.String())
	}
	if c.h2.Load() != 1 || c.h1.Load() != 0 {
		t.Fatalf("peer served h1=%d h2=%d, want the one request over HTTP/2", c.h1.Load(), c.h2.Load())
	}
	if d := lastDecision(t, buf); d.Decision != egress.Allow {
		t.Fatalf("last decision = %+v, want an allow", d)
	}
}

// TestUpstreamProtocolMismatch_BodyAlreadyWritten is the other half of that
// pair: a peer that answers only once it has READ the request (the field
// report's own order) is caught from net/http's parse error, by which point
// the body has gone out. That one cannot be sent again, so it is #359's 400,
// and the sentence says which of the two reasons applies.
func TestUpstreamProtocolMismatch_BodyAlreadyWritten(t *testing.T) {
	peer := startH2Peer(t)
	p, buf := newH2TestProxy(t, peer, awsHost)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "https://"+awsHost+"/", io.NopCloser(strings.NewReader(`{"a":1}`)))
	p.serveMITMRequest(rec, r, awsHost, 443)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
	}
	body := decodeAWSSDKError(t, rec.Body.Bytes())
	if !strings.Contains(body["message"], "ALPN: none") || !strings.Contains(body["message"], "cannot be replayed") {
		t.Fatalf("message = %q, want the ALPN clause and the reason the resend was skipped", body["message"])
	}
	if d := findDecision(t, buf, ruleSourceUpstreamProtocolMismatch); d.Cause != body["message"] {
		t.Fatalf("deny row cause = %q, want %q", d.Cause, body["message"])
	}
}

// TestSniffH2 drives the post-handshake read directly, over a synchronous
// in-memory TLS pair: what a peer writes is delivered by the read itself, so
// each case is a fact about sniffH2 rather than a race with a scheduler.
func TestSniffH2(t *testing.T) {
	// A peer that speaks first is refused, so no request is ever written to it.
	t.Run("SETTINGS frame", func(t *testing.T) {
		client, server := tlsPipe(t)
		go func() { _, _ = server.Write([]byte(fieldReportFrames)) }()
		if _, err := sniffH2(client); !errors.Is(err, errPeerSpeaksH2) {
			t.Fatalf("sniffH2 = %v, want errPeerSpeaksH2", err)
		}
	})

	// Silence is what an HTTP/1.1 peer does; the connection is handed back
	// usable, and the dialer remembers the host so nothing probes it again.
	t.Run("nothing", func(t *testing.T) {
		client, server := tlsPipe(t)
		conn, err := sniffH2(client)
		if !errors.Is(err, errNoUnpromptedBytes) || conn != net.Conn(client) {
			t.Fatalf("sniffH2 = (%T, %v), want the same conn and errNoUnpromptedBytes", conn, err)
		}
		go func() { _, _ = server.Write([]byte("HTTP/1.1 204 No Content\r\n\r\n")) }()
		if got := readN(t, conn, 12); got != "HTTP/1.1 204" {
			t.Fatalf("read %q after the probe, want the peer's answer", got)
		}
	})

	// A partial frame header is not a verdict: the bytes go back in front of
	// the connection, in order, so the HTTP/1.1 reader sees what it would have
	// seen without the probe.
	t.Run("partial frame header", func(t *testing.T) {
		client, server := tlsPipe(t)
		go func() { _, _ = server.Write([]byte(fieldReportFrames[:4])) }()
		conn, err := sniffH2(client)
		if err != nil {
			t.Fatalf("sniffH2 = %v, want the connection back", err)
		}
		go func() { _, _ = server.Write([]byte(fieldReportFrames[4:])) }()
		if got := readN(t, conn, len(fieldReportFrames)); got != fieldReportFrames {
			t.Fatalf("read %q, want the peer's bytes whole and in order", got)
		}
	})
}

// tlsPipe is a handshaken TLS client/server pair over net.Pipe, with no ALPN
// negotiated. net.Pipe is synchronous and unbuffered: a write lands exactly
// when the other side reads it.
func tlsPipe(t *testing.T) (*tls.Conn, *tls.Conn) {
	t.Helper()
	c, s := net.Pipe()
	client := tls.Client(c, testInsecureTLSConfig)
	server := tls.Server(s, noALPNConfig(t))
	// The RAW ends: closing a tls.Conn writes close_notify first, and on an
	// unbuffered pipe with nobody reading that costs crypto/tls's own 5s
	// write deadline per conn.
	t.Cleanup(func() { _ = c.Close(); _ = s.Close() })
	done := make(chan error, 1)
	go func() { done <- server.Handshake() }()
	if err := client.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("server handshake: %v", err)
	}
	return client, server
}

func readN(t *testing.T, conn net.Conn, n int) string {
	t.Helper()
	buf := make([]byte, n)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	return string(buf)
}

// TestH2Fallback_PeerWritesSettingsFirst covers the two shapes net/http's
// error text never carries (isH2Preface's doc comment) — acceptance (d), a
// SETTINGS payload with 0x20 before any 0x0a, and a peer whose SETTINGS
// arrive before the request (the field report's own SETTINGS frame, written
// unprompted) — both detected from the bytes and resent over HTTP/2.
func TestH2Fallback_PeerWritesSettingsFirst(t *testing.T) {
	for name, settings := range map[string][]byte{
		"space byte before any newline":     spaceInPayloadSettings,
		"unsolicited field-report SETTINGS": []byte(fieldReportFrames[:27]),
	} {
		t.Run(name, func(t *testing.T) {
			var c h2Counts
			peer := startTunnel(t, startTLSPeer(t, noALPNConfig(t), serveSettingsFirst(settings, &c)))
			p, buf := newH2TestProxy(t, peer, "h2.test")
			forwardGet(t, p, buf)
			if c.h2.Load() != 1 {
				t.Fatalf("peer answered %d HTTP/2 requests, want 1", c.h2.Load())
			}
		})
	}
}

// TestH2_HTTP1Peers is acceptance (e) and (f): an HTTP/1.1 peer, with or
// without ALPN, is served over HTTP/1.1 on one connection exactly as before.
// Only the no-ALPN peer pays the sniff, and h2ProbeTimeout bounds it.
func TestH2_HTTP1Peers(t *testing.T) {
	if h2ProbeTimeout != 250*time.Millisecond {
		t.Fatalf("h2ProbeTimeout = %v; the latency a no-ALPN HTTP/1.1 peer pays per new connection is documented as 250ms", h2ProbeTimeout)
	}
	for name, nextProtos := range map[string][]string{
		"negotiates http/1.1": {"http/1.1"},
		"negotiates nothing":  nil,
	} {
		t.Run(name, func(t *testing.T) {
			var c h2Counts
			ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}, NextProtos: nextProtos})
			if err != nil {
				t.Fatalf("tls listen: %v", err)
			}
			srv := &http.Server{Handler: c.handler(), ReadHeaderTimeout: 5 * time.Second}
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })
			peer := startTunnel(t, ln.Addr().String())
			p, buf := newH2TestProxy(t, peer, "h2.test")

			start := time.Now()
			forwardGet(t, p, buf)
			elapsed := time.Since(start)
			if c.h1.Load() != 1 || c.h2.Load() != 0 || peer.connects.Load() != 1 {
				t.Fatalf("h1=%d h2=%d connects=%d, want one HTTP/1.1 request on one connection", c.h1.Load(), c.h2.Load(), peer.connects.Load())
			}
			if nextProtos == nil && elapsed < h2ProbeTimeout {
				t.Fatalf("elapsed %v < h2ProbeTimeout: the no-ALPN connection was not sniffed", elapsed)
			}
		})
	}
}

// TestH2_NoALPNHTTP1PeerIsProbedOnce is the memo's other half: an HTTP/1.1
// peer that negotiates nothing is remembered too, so the whole run pays the
// probe once rather than on every new connection to it.
func TestH2_NoALPNHTTP1PeerIsProbedOnce(t *testing.T) {
	var c h2Counts
	ln, err := tls.Listen("tcp", "127.0.0.1:0", noALPNConfig(t))
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	srv := &http.Server{Handler: c.handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	peer := startTunnel(t, ln.Addr().String())
	p, buf := newH2TestProxy(t, peer, "h2.test")

	forwardGet(t, p, buf)
	if v, seen := p.h2.hosts.Load("h2.test:443"); !seen || v.(bool) {
		t.Fatalf("memo for h2.test:443 = (%v, %v), want a recorded HTTP/1.1 peer", v, seen)
	}

	// A second CONNECTION, not just a second request: the pooled one would skip
	// the dial altogether and prove nothing.
	p.transport.CloseIdleConnections()
	start := time.Now()
	forwardGet(t, p, buf)
	if elapsed := time.Since(start); elapsed >= h2ProbeTimeout {
		t.Fatalf("second connection took %v (>= h2ProbeTimeout): it paid the probe again", elapsed)
	}
	if c.h1.Load() != 2 || c.h2.Load() != 0 || peer.connects.Load() != 2 {
		t.Fatalf("h1=%d h2=%d connects=%d, want two HTTP/1.1 requests on two connections", c.h1.Load(), c.h2.Load(), peer.connects.Load())
	}
}

// TestH2Fallback_PATBrokerLane is the broker lanes' share of the fallback:
// they re-originate to a forge on the same transport, and a forge that speaks
// HTTP/2 without negotiating it is answered over HTTP/2 rather than refused.
// The mint (control plane, HTTP/1.1) and the forge are separate peers here,
// told apart by port.
func TestH2Fallback_PATBrokerLane(t *testing.T) {
	var c h2Counts
	mint := newPATBrokerUpstream(t, "T", "oauth2")
	mintAddr := upstreamAddr(mint.srv)
	forgeAddr := startTLSPeer(t, noALPNConfig(t), serveH2Unasked(&c))
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 16)},
		Resolver:        publicResolver{},
		ControlPlaneURL: "https://wardynd.test:8080",
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		ControlTLS:      testInsecureTLSConfig,
		PATGrants:       map[string]PATGrant{"dev.azure.com": {GrantID: uuid.New()}},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			to := forgeAddr
			if _, port, _ := net.SplitHostPort(addr); port == "8080" {
				to = mintAddr
			}
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, to)
		},
	})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet,
		"/wardyn/git/dev.azure.com/org/repo/info/refs?service=git-upload-pack", nil))

	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("status = %d body %q, want 200 ok from the forge", rec.Code, rec.Body.String())
	}
	if c.h2.Load() != 1 || c.h1.Load() != 0 {
		t.Fatalf("forge served h1=%d h2=%d, want the clone over HTTP/2", c.h1.Load(), c.h2.Load())
	}
}

// newGitBrokerH2Proxy is a GitHub App lane proxy whose mint is an ordinary
// control plane and whose forge is forgeAddr; forgeDials counts the dials that
// reached the forge.
func newGitBrokerH2Proxy(t *testing.T, forgeAddr string, forgeDials *atomic.Int32) (*Proxy, *bytes.Buffer) {
	t.Helper()
	mint := newGitBrokerUpstream(t, "gh-inst-token")
	split := splitDial(upstreamAddr(mint.srv), forgeAddr)
	buf := &bytes.Buffer{}
	return newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{}),
		Sink:     &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 16)},
		Resolver: publicResolver{},
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if _, port, _ := net.SplitHostPort(addr); port == "443" {
				forgeDials.Add(1)
			}
			return split(ctx, network, addr)
		},
		ControlPlaneURL: "https://wardynd.test:8080",
		RunToken:        newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig,
		ControlTLS:      testInsecureTLSConfig,
		GitGrants:       map[string]uuid.UUID{"octocat/hello-world": uuid.New()},
	}), buf
}

// TestH2Fallback_GitBrokerLane is TestH2Fallback_PATBrokerLane's GitHub App
// sibling: a forge that speaks HTTP/2 without negotiating it answers the clone
// over HTTP/2, and the request leaves one allow row.
func TestH2Fallback_GitBrokerLane(t *testing.T) {
	var c h2Counts
	var dials atomic.Int32
	p, buf := newGitBrokerH2Proxy(t, startTLSPeer(t, noALPNConfig(t), serveH2Unasked(&c)), &dials)

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet,
		"/wardyn/gh/octocat/hello-world.git/info/refs?service=git-upload-pack", nil))

	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("status = %d body %q, want 200 ok from the forge", rec.Code, rec.Body.String())
	}
	if c.h2.Load() != 1 || c.h1.Load() != 0 {
		t.Fatalf("forge served h1=%d h2=%d, want the clone over HTTP/2", c.h1.Load(), c.h2.Load())
	}
	if got := decisionRows(t, buf); !slices.Equal(got, []string{"allow " + ruleSourceGit}) {
		t.Fatalf("rows = %q, want one allow", got)
	}
}

// TestH2Fallback_PushBodyResentOnlyWhenUnread drives a receive-pack POST — the
// command section the broker buffered, re-prepended to the streaming pack
// (confinePush's MultiReader, no GetBody) — through the HTTP/2 fallback.
// resendable may reuse that body only while nothing has read it:
//
//   - a forge caught by the post-handshake sniff failed the dial before any
//     write, so the push is resent over HTTP/2 and arrives byte-identical;
//   - a forge that answers HTTP/2 only after reading the request has consumed
//     the body, so the push is refused with 400 and never replayed.
func TestH2Fallback_PushBodyResentOnlyWhenUnread(t *testing.T) {
	t.Setenv(envEnforceBranchNS, "on")
	pushBody := func(p *Proxy) string {
		ref := "refs/heads/wardyn/" + p.runID.String() + "/feature"
		return pkt(someOID+" "+otherOID+" "+ref+firstCaps) + "0000" +
			"PACK\x00\x02\x00\x00\x00\x01\xff\xfe\x00 binary"
	}
	const path = "/wardyn/gh/octocat/hello-world.git/git-receive-pack"

	t.Run("unread: resent intact", func(t *testing.T) {
		var c h2Counts
		var dials atomic.Int32
		p, buf := newGitBrokerH2Proxy(t, startTLSPeer(t, noALPNConfig(t), serveH2Unasked(&c)), &dials)
		body := pushBody(p)

		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, path, io.NopCloser(strings.NewReader(body))))

		// h2Counts echoes the body it received.
		if rec.Code != http.StatusOK || rec.Body.String() != body {
			t.Fatalf("status = %d body %q, want 200 with the push echoed byte-for-byte (%q)", rec.Code, rec.Body.String(), body)
		}
		if c.h2.Load() != 1 || c.h1.Load() != 0 {
			t.Fatalf("forge served h1=%d h2=%d, want the push once, over HTTP/2", c.h1.Load(), c.h2.Load())
		}
		if got := decisionRows(t, buf); !slices.Equal(got, []string{"allow " + ruleSourceGit}) {
			t.Fatalf("rows = %q, want one allow", got)
		}
	})

	t.Run("read: refused, not replayed", func(t *testing.T) {
		var dials atomic.Int32
		p, buf := newGitBrokerH2Proxy(t, startH2MismatchPeer(t), &dials)

		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, path, io.NopCloser(strings.NewReader(pushBody(p)))))

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (body %q)", rec.Code, rec.Body.String())
		}
		if got := denyBody(rec); !strings.Contains(got, "cannot be replayed") {
			t.Errorf("body = %q, want the reason the resend was skipped", got)
		}
		if got := dials.Load(); got != 1 {
			t.Errorf("forge dials = %d, want 1: a consumed push body is never sent again", got)
		}
		if got := decisionRows(t, buf); !slices.Equal(got, []string{"deny " + ruleSourceUpstreamProtocolMismatch}) {
			t.Fatalf("rows = %q, want the mismatch deny alone", got)
		}
	})
}

// TestH2Fallback_NoProxyBypass is acceptance (g): a host on
// upstream_proxy_no_proxy reaches the HTTP/2 fallback by the same direct dial
// to its vetted address as the HTTP/1.1 attempt — never through the corp proxy.
func TestH2Fallback_NoProxyBypass(t *testing.T) {
	var c h2Counts
	f := startFakeUpstream(t)
	d := &routingDialer{upstreamAddr: f.addr(), directAddr: startTLSPeer(t, noALPNConfig(t), serveH2Unasked(&c))}
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"mirror.corp.internal"}}),
		Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)},
		Resolver:        fakeResolver{m: map[string][]net.IP{"mirror.corp.internal": ips("93.184.216.34")}},
		Dial:            d.dial,
		Upstream:        mustUpstream(t, f.addr()),
		UpstreamNoProxy: []string{".corp.internal"},
		TLSClientConfig: testInsecureTLSConfig,
		ControlTLS:      testInsecureTLSConfig,
	})

	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "https://mirror.corp.internal/"))
	if rec.Code != http.StatusOK || c.h2.Load() != 1 {
		t.Fatalf("status = %d, h2 requests = %d; want 200 over HTTP/2", rec.Code, c.h2.Load())
	}
	if got := atomic.LoadInt32(&f.accepts); got != 0 {
		t.Fatalf("corp proxy accepts = %d, want 0: the bypass must hold on the HTTP/2 transport too", got)
	}
	if got := d.dialed(); len(got) != 2 || got[0] != "93.184.216.34:443" || got[1] != "93.184.216.34:443" {
		t.Fatalf("dials = %v, want the vetted address twice (the sniffed attempt, then HTTP/2)", got)
	}
}

// A request net/http can rebuild is rebuilt rather than resent through the
// shield: a failed attempt's write goroutine outlives RoundTrip for exactly
// those requests, and it still holds the shield.
func TestResendableRebuildsBeforeReusingTheShield(t *testing.T) {
	t.Parallel()
	rebuildable, err := http.NewRequest(http.MethodPost, "https://example.test/v1", bytes.NewReader([]byte("payload")))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if rebuildable.GetBody == nil {
		t.Fatal("precondition: a bytes.Reader body should carry GetBody")
	}
	shield := &shieldedBody{rc: io.NopCloser(bytes.NewReader([]byte("payload")))}
	out, ok := resendable(rebuildable, shield, true)
	if !ok {
		t.Fatal("a rebuildable body should be resendable")
	}
	if out.Body == io.ReadCloser(shield) {
		t.Error("the resend reused the shield although GetBody could rebuild the body")
	}

	// No GetBody: the shield is the only way to send those bytes again, and a
	// sniffed peer never read them.
	streamed, err := http.NewRequest(http.MethodPost, "https://example.test/v1", struct{ io.Reader }{bytes.NewReader([]byte("payload"))})
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if streamed.GetBody != nil {
		t.Fatal("precondition: a bare io.Reader body should not carry GetBody")
	}
	shield2 := &shieldedBody{rc: io.NopCloser(bytes.NewReader([]byte("payload")))}
	out, ok = resendable(streamed, shield2, true)
	if !ok {
		t.Fatal("an unread body on a sniffed peer should be resendable")
	}
	if out.Body != io.ReadCloser(shield2) {
		t.Error("the resend did not carry the shielded body")
	}
	shield2.read.Store(true)
	if _, ok := resendable(streamed, shield2, true); ok {
		t.Error("a body already read must not be resent")
	}
}
