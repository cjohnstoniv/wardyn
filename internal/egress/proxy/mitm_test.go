// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

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
	"encoding/pem"
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

// genTestCA returns a self-signed CA cert+key (PEM) for MITM tests.
func genTestCA(t *testing.T) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Wardyn Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// mitmProxy builds a proxy with MITM enabled and returns it + the decision buffer
// + the Wardyn CA cert (which the test agent will trust).
func mitmProxy(t *testing.T, mode string, upstream *httptest.Server) (*Proxy, *bytes.Buffer, []byte) {
	t.Helper()
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 32)}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{anthropicHost}}),
		Sink:            sink,
		Scanner:         scanEngine(t, mode, scanTestSecret),
		CA:              ca,
		MITMLLM:         true, // these tests exercise the intended LLM-MITM path (inspection/injection)
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(upstream)),
		TLSClientConfig: testInsecureTLSConfig,
	})
	return p, buf, certPEM
}

// agentMITMConn drives the agent side: CONNECT through the proxy, then a TLS
// handshake trusting the Wardyn CA. Returns the decrypted TLS conn to the "model".
func agentMITMConn(t *testing.T, proxyURL string, caPEM []byte) *tls.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "CONNECT "+anthropicHost+":443 HTTP/1.1\r\nHost: "+anthropicHost+":443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d", resp.StatusCode)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to add Wardyn CA to agent trust pool")
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: anthropicHost, RootCAs: pool})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("agent TLS handshake (must trust the Wardyn CA leaf): %v", err)
	}
	return tlsConn
}

func TestMITMInspectsAndForwardsPreservingResidentCred(t *testing.T) {
	cu := captureUpstream(t, true, "llm-ok")

	p, buf, caPEM := mitmProxy(t, "alert", cu.srv)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	tlsConn := agentMITMConn(t, proxySrv.URL, caPEM)
	defer tlsConn.Close()

	body := anthropicMessagesBody("leak " + scanTestSecret)
	req, _ := http.NewRequest(http.MethodPost, "https://"+anthropicHost+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer RESIDENT-OAUTH-TOKEN") // the subscription cred
	req.Header.Set("Content-Type", "application/json")
	if err := req.Write(tlsConn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		t.Fatalf("read MITM response: %v", err)
	}
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(rb) != "llm-ok" {
		t.Fatalf("alert MITM must forward, got %d %q", resp.StatusCode, rb)
	}
	if cu.body != body {
		t.Fatalf("upstream body not forwarded intact")
	}
	// The agent's RESIDENT credential must be preserved (NOT stripped/injected) —
	// MITM is inspect-only passthrough for the subscription path.
	if got := cu.header.Get("Authorization"); got != "Bearer RESIDENT-OAUTH-TOKEN" {
		t.Fatalf("MITM must preserve the resident credential, upstream saw %q", got)
	}
	if !strings.Contains(buf.String(), ruleSourceLLMMITM) {
		t.Fatalf("expected a scan:mitm decision, log=%s", buf.String())
	}
	if strings.Contains(buf.String(), scanTestSecret) {
		t.Fatal("decision log leaked the secret")
	}
}

// With an injection rule for the MITM host (the subscription path), the proxy
// STRIPS the sandbox's sentinel credential and injects the live one — mirroring
// the api-key local route. This is what makes the sandbox's inert sentinel work.
func TestMITMInjectsLiveTokenAndStripsSentinel(t *testing.T) {
	cu := captureUpstream(t, true, "llm-ok")

	p, _, caPEM := mitmProxy(t, "alert", cu.srv)
	// Subscription injection rule for the MITM host: swap the sandbox's inert
	// sentinel credential for the live, host-refreshed token.
	p.inject = staticInj(map[string]injectedHeader{
		anthropicHost: {name: "Authorization", value: "Bearer BROKERED-LIVE-TOKEN"},
	})
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	tlsConn := agentMITMConn(t, proxySrv.URL, caPEM)
	defer tlsConn.Close()

	body := anthropicMessagesBody("hello")
	req, _ := http.NewRequest(http.MethodPost, "https://"+anthropicHost+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer SENTINEL-EXPIRED") // the sandbox's inert sentinel
	req.Header.Set("Content-Type", "application/json")
	if err := req.Write(tlsConn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		t.Fatalf("read MITM response: %v", err)
	}
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(rb) != "llm-ok" {
		t.Fatalf("MITM inject must forward, got %d %q", resp.StatusCode, rb)
	}
	if got := cu.header.Get("Authorization"); got != "Bearer BROKERED-LIVE-TOKEN" {
		t.Fatalf("upstream Authorization = %q, want the injected live token (sentinel must be swapped)", got)
	}
	if cu.body != body {
		t.Fatalf("upstream body not forwarded intact")
	}
}

func TestMITMBlockRefusesOverTunnel(t *testing.T) {
	cu := captureUpstream(t, true, "")

	p, _, caPEM := mitmProxy(t, "block", cu.srv)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	tlsConn := agentMITMConn(t, proxySrv.URL, caPEM)
	defer tlsConn.Close()

	body := anthropicMessagesBody("leak " + scanTestSecret)
	req, _ := http.NewRequest(http.MethodPost, "https://"+anthropicHost+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if err := req.Write(tlsConn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		t.Fatalf("read MITM response: %v", err)
	}
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("block MITM must 403, got %d", resp.StatusCode)
	}
	if !strings.Contains(string(rb), "llm_content_blocked") {
		t.Fatalf("block body = %q", rb)
	}
	if cu.reached {
		t.Fatal("blocked MITM request must not reach the upstream")
	}
}

// TestMITMCorpHost_DialsConfiguredPort is the W13-S1-5 regression. A corp
// artifact MITM host is matched by HOSTNAME (mitmHosts), so before this fix
// serveMITMRequest hardcoded the dial target to port 443 regardless of which
// port the sandbox actually CONNECTed to. An operator's mirror living on a
// non-443 port (npmrc/pip.conf/etc. pointing at "mirror.corp:5000") would
// still get its tunnel TLS-terminated (hostname matched) and its registry
// token injected — then FORWARDED to port 443 of that same host, presenting
// the token to whatever answers there instead of the configured mirror. This
// test fails on base 6d76911 (captures a dial to port 443) and passes once
// the real CONNECT port is threaded through mitmConnect/serveMITMRequest.
func TestMITMCorpHost_DialsConfiguredPort(t *testing.T) {
	cu := captureUpstream(t, true, "mirror-ok")

	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}

	var gotDial string
	p := newProxy(Options{
		RunID:  uuid.New(),
		Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"mirror.corp"}}),
		Sink:   &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		CA:     ca,
		// Bare host: the historical, still-supported format (mitmPorts defaults
		// to "any port" for it), so the MITM-ELIGIBILITY gate alone can't be
		// what makes this test pass — only fixing the DIAL inside
		// serveMITMRequest does, which is what this test isolates.
		MITMHosts:       []string{"mirror.corp"},
		Resolver:        publicResolver{},
		TLSClientConfig: testInsecureTLSConfig,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			gotDial = addr
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, upstreamAddr(cu.srv))
		},
	})
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	// Agent CONNECTs to the mirror's REAL, non-443 port.
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxySrv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "CONNECT mirror.corp:5000 HTTP/1.1\r\nHost: mirror.corp:5000\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200 (mirror.corp is policy-allowed on any port)", resp.StatusCode)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("failed to add Wardyn CA to agent trust pool")
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: "mirror.corp", RootCAs: pool})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("agent TLS handshake (must trust the Wardyn CA leaf): %v", err)
	}
	defer tlsConn.Close()

	req, _ := http.NewRequest(http.MethodGet, "https://mirror.corp/api/npm/some-pkg", nil)
	req.Header.Set("Authorization", "Bearer OPERATOR-REGISTRY-TOKEN")
	if err := req.Write(tlsConn); err != nil {
		t.Fatal(err)
	}
	getResp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		t.Fatalf("read MITM response: %v", err)
	}
	rb, _ := io.ReadAll(getResp.Body)
	if getResp.StatusCode != http.StatusOK || string(rb) != "mirror-ok" {
		t.Fatalf("mirror request must forward, got %d %q", getResp.StatusCode, rb)
	}

	if gotDial == "" {
		t.Fatal("proxy never dialed an upstream")
	}
	_, gotPort, err := net.SplitHostPort(gotDial)
	if err != nil {
		t.Fatalf("dial target %q: %v", gotDial, err)
	}
	if gotPort != "5000" {
		t.Fatalf("proxy dialed port %q, want 5000 (the CONNECT's real port). Dialing 443 instead "+
			"presents the operator's registry token to whatever answers on port 443 of mirror.corp, "+
			"not the configured mirror (W13-S1-5)", gotPort)
	}
}

// TestMITMCorpHost_PortMismatchFallsThroughOpaque is W13-S1-5's other half:
// an EXPLICITLY port-scoped mitmHosts entry ("host:port", what
// planArtifactRedirect now authors) is MITM/injection-eligible ONLY at that
// port. A CONNECT to the same host on a DIFFERENT, unconfigured port must NOT
// be TLS-terminated — it falls through to an ordinary opaque tunnel, so the
// operator's token is never even offered to a service the redirect never
// named. This pins isMITMHost's "EXACT-hostname allowlist" doc claim now
// covering port too.
func TestMITMCorpHost_PortMismatchFallsThroughOpaque(t *testing.T) {
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}

	p := newProxy(Options{
		RunID:     uuid.New(),
		Policy:    CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"mirror.corp"}}),
		Sink:      &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		CA:        ca,
		MITMHosts: []string{"mirror.corp:5000"}, // scoped to :5000 only
		Resolver:  publicResolver{},
		Dial:      redirectDial(startEcho(t)), // opaque-tunnel stand-in
	})
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	// CONNECT to the SAME host on a DIFFERENT (unconfigured) port.
	conn, status := connectThrough(t, proxySrv.URL, "mirror.corp:9999")
	defer conn.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("policy-allowed host (any port) must still tunnel: %q", status)
	}
	// A MITM'd connection would have the proxy attempt its own TLS server
	// handshake here instead of piping raw bytes; sending plaintext and getting
	// it echoed back verbatim proves this is an OPAQUE tunnel, not TLS-terminated.
	_, _ = io.WriteString(conn, "ping")
	buf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, err := conn.Read(buf)
	if err != nil || string(buf[:n]) != "ping" {
		t.Fatalf("expected an opaque passthrough echo of %q, got %q err=%v", "ping", buf[:n], err)
	}
}

// TestMITMCorpHost_ForwardEgressScanCoversBody is the W19-W19d-1 regression.
// channelForHost maps every corp artifact MITM host to ChannelGeneric, which
// classifyLLM unconditionally treats as scanNone (not prompt-bearing) — so
// inspectLLM alone streamed an artifact-MITM body through completely
// unscanned, even with inspect_forward_egress on (the flag that already
// extends inspection to the PLAIN, non-MITM forward path). A secret leaking
// through a "corp registry" MITM tunnel must be caught exactly like one
// leaking through a plain HTTP connector.
func TestMITMCorpHost_ForwardEgressScanCoversBody(t *testing.T) {
	cu := captureUpstream(t, true, "mirror-ok")

	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"mirror.corp"}}),
		Sink:            &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		Scanner:         forwardScanEngine(t, "block"),
		CA:              ca,
		MITMHosts:       []string{"mirror.corp"},
		Resolver:        publicResolver{},
		TLSClientConfig: testInsecureTLSConfig,
		Dial:            redirectDial(upstreamAddr(cu.srv)),
	})
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	conn, status := connectThrough(t, proxySrv.URL, "mirror.corp:443")
	defer conn.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("failed to add Wardyn CA to agent trust pool")
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: "mirror.corp", RootCAs: pool})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("agent TLS handshake: %v", err)
	}
	defer tlsConn.Close()

	body := `{"payload":"leak ` + scanTestSecret + `"}`
	req, _ := http.NewRequest(http.MethodPost, "https://mirror.corp/api/npm/publish", strings.NewReader(body))
	req.ContentLength = int64(len(body))
	if err := req.Write(tlsConn); err != nil {
		t.Fatal(err)
	}
	getResp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		t.Fatalf("read MITM response: %v", err)
	}
	if getResp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 — a secret in a corp-MITM'd body must be blocked "+
			"under inspect_forward_egress like any other generic connector", getResp.StatusCode)
	}
	if cu.reached {
		t.Fatal("a blocked corp-MITM request must never reach the upstream registry")
	}
}

// A credential refresh that fails must fail CLOSED and must not relay the
// control plane's response text verbatim: the error is written into the SANDBOX,
// so any registered secret it carries is masked (httpError) before it leaves.
func TestMITMRefreshFailureMasksSecretInError(t *testing.T) {
	const secret = "sk-ant-oat-LEAKED-0123456789"
	procRegistry.AddGlobal([]byte(secret))
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "resolve failed for "+secret, http.StatusInternalServerError)
	}))
	defer cp.Close()

	// A DYNAMIC entry already past expiry forces a re-resolve, which the stub
	// control plane fails with a secret-bearing body.
	inj := &injector{
		byHost: map[string]*injEntry{"api.test": {grantID: uuid.New(), expiresAt: time.Now().Add(-time.Hour).UnixMilli()}},
		base:   cp.URL,
		token:  newTokenSource("tok"),
		client: cp.Client(),
	}
	p, _ := newLocalRouteProxy(t, cp.URL, "RUNTOK", upstreamAddr(cp), inj, nil)

	rec := httptest.NewRecorder()
	p.serveMITMRequest(rec, httptest.NewRequest(http.MethodPost, "https://api.test/v1/messages", nil), "api.test", 443)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (fail closed on refresh failure)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("registered secret reached the sandbox: %q", rec.Body.String())
	}
	// …and the secret really was in the error text (else the test is vacuous).
	if !strings.Contains(rec.Body.String(), "<secret-hidden>") {
		t.Fatalf("expected the masked placeholder in the error, got %q", rec.Body.String())
	}
}
