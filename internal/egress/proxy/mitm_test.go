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
	"encoding/json"
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

	// The leaf the agent accepted must arrive WITH the CA that signed it
	// (leafFor's Certificate is [leaf DER, caCert.Raw]). Without the chain the
	// agent's own verification would depend on it already holding the issuer for
	// some other reason — true in this test, where the CA is pinned in the trust
	// pool, and NOT true of a sandbox client that only has the CA in a system
	// store it does not consult per-connection. So the second cert is load-bearing
	// and is asserted to be that exact CA, byte for byte, not merely "some cert".
	st := tlsConn.ConnectionState()
	if len(st.PeerCertificates) != 2 {
		t.Fatalf("MITM leaf chain = %d cert(s), want 2 (leaf then the Wardyn CA)", len(st.PeerCertificates))
	}
	caDER, _ := pem.Decode(caPEM)
	if caDER == nil {
		t.Fatal("could not decode the Wardyn CA PEM")
	}
	if !bytes.Equal(st.PeerCertificates[1].Raw, caDER.Bytes) {
		t.Fatal("the chain's second cert is not the Wardyn CA that signed the leaf")
	}

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

// TestMITMCorpHost_DialsConfiguredPort: a corp artifact MITM host is matched
// by hostname (mitmHosts), so serveMITMRequest must dial the port the sandbox
// actually CONNECTed to, not a hardcoded 443. An operator's mirror living on
// a non-443 port (npmrc/pip.conf/etc. pointing at "mirror.corp:5000") gets
// its tunnel TLS-terminated (hostname matched) and its registry token
// injected, so a 443 dial would forward it to port 443 of that same host,
// presenting the token to whatever answers there instead of the configured
// mirror. The real CONNECT port is threaded through
// mitmConnect/serveMITMRequest.
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

// TestMITMCorpHost_PortMismatchFallsThroughOpaque is other half:
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

// TestMITMCorpHost_ForwardEgressScanCoversBody: channelForHost maps every
// corp artifact MITM host to ChannelGeneric, which classifyLLM
// unconditionally treats as scanNone (not prompt-bearing) — so inspectLLM
// alone would stream an artifact-MITM body through completely unscanned, even
// with inspect_forward_egress on (the flag that extends inspection to the
// plain, non-MITM forward path). A secret leaking through a "corp registry"
// MITM tunnel must be caught exactly like one leaking through a plain HTTP
// connector.
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
	procMask([]byte(secret))
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

// TestMITMCorpHost_DecisionCarriesRealPort is the audit half of
// TestMITMCorpHost_DialsConfiguredPort: that test pins that the real CONNECT
// port reaches the dial; this pins that it also reaches the decision log. The
// two are separate plumbing — mitmConnect threads port into serveMITMRequest,
// which hands it to both egressTarget and emitLLMDecision — so a change that
// reverted only the decision arm would leave the audit trail saying the
// operator's registry token went to mirror.corp:443 while the wire says
// :5000. On a host-matched MITM lane the port is the only field
// distinguishing the configured mirror from anything else answering on that
// hostname, so a row naming the wrong one is worse than no row.
func TestMITMCorpHost_DecisionCarriesRealPort(t *testing.T) {
	cu := captureUpstream(t, true, "mirror-ok")

	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:  uuid.New(),
		Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"mirror.corp"}}),
		Sink:   &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 16)},
		CA:     ca,
		// Port-SCOPED entry (what planArtifactRedirect authors): mitmPorts["mirror.corp"]
		// is 5000, so only a CONNECT to :5000 is MITM-eligible at all.
		MITMHosts:       []string{"mirror.corp:5000"},
		Resolver:        publicResolver{},
		TLSClientConfig: testInsecureTLSConfig,
		Dial:            redirectDial(upstreamAddr(cu.srv)),
	})
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	conn, status := connectThrough(t, proxySrv.URL, "mirror.corp:5000")
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

	req, _ := http.NewRequest(http.MethodGet, "https://mirror.corp/api/npm/some-pkg", nil)
	if err := req.Write(tlsConn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		t.Fatalf("read MITM response: %v", err)
	}
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(rb) != "mirror-ok" {
		t.Fatalf("mirror request must forward, got %d %q", resp.StatusCode, rb)
	}

	d := findDecision(t, buf, ruleSourceArtifactMITM)
	if d.Request.Port != 5000 {
		t.Fatalf("decision port = %d, want 5000 (the CONNECT's real port). A row saying 443 "+
			"credits the operator's registry token to a service the redirect never named (W13-S1-5)", d.Request.Port)
	}
	if d.Request.Host != "mirror.corp" {
		t.Fatalf("decision host = %q, want mirror.corp", d.Request.Host)
	}
	if d.Decision != egress.Allow {
		t.Fatalf("decision = %q, want allow", d.Decision)
	}
}

// A hold that expires answers the SANDBOX with the modelled AWS credential
// error, not the generic 502 a refresh failure gives — and leaves a decision row
// that names the hold rather than a refresh failure.
//
// The two are different facts with different fixes: "the credential could not be
// refreshed" is the operator's, "nobody signed in" is a person's. And the status
// matters to the SDK: 502 is a transport error both AWS SDKs RETRY (three more
// full holds for one lapse), UnauthorizedException is modelled and terminal.
func TestMITMReauthTimeoutWrites401AndItsOwnDecision(t *testing.T) {
	prevPoll := holdPollInterval
	holdPollInterval = 5 * time.Millisecond
	defer func() { holdPollInterval = prevPoll }()

	// The decision row is narrower than the 401 (security NIT-B). Every case
	// below ends without a credential and every one of them earns the modelled
	// 401 — the sandbox has to be told. Only ONE of them expired, and only that
	// one may write credential:reauth-timeout, because that row is what an
	// operator reads as "the owner had the whole window". A shutdown and a
	// killed run did not have the whole window.
	for _, tc := range []struct {
		name     string
		budget   string
		steps    []approvalStep
		shutdown bool // end the hold through the coordinator, as Shutdown does
		wantRow  bool
		sentence string
	}{{
		name:     "the budget really expired",
		budget:   "10s", // the clamp's floor
		steps:    pending(1),
		wantRow:  true,
		sentence: reauthTimedOutSentence,
	}, {
		name:     "the run was killed under the hold",
		budget:   "1800s",
		steps:    []approvalStep{{state: types.ApprovalCancelled, status: http.StatusOK}},
		sentence: reauthEndedSentence,
	}, {
		name:     "the proxy shut down under the hold",
		budget:   "1800s",
		steps:    pending(1),
		shutdown: true,
		sentence: reauthEndedSentence,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envCredentialReauthTimeout, tc.budget)

			// A control plane that asks for a human forever.
			approvalID := uuid.New()
			cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusLocked)
				_, _ = w.Write([]byte(`{"state":"reauth_pending","approval_id":"` + approvalID.String() + `"}`))
			}))
			defer cp.Close()

			inj := &injector{
				byHost:    map[string]*injEntry{"portal.sso.eu-west-2.amazonaws.com": {grantID: uuid.New(), expiresAt: time.Now().Add(-time.Hour).UnixMilli()}},
				base:      cp.URL,
				token:     newTokenSource("tok"),
				client:    cp.Client(),
				reauth:    newReauthCoordinator(),
				approvals: &fakeApprovalReader{steps: tc.steps},
			}
			t.Cleanup(inj.reauth.stop)
			p, buf := newLocalRouteProxy(t, cp.URL, "RUNTOK", upstreamAddr(cp), inj, nil)

			if tc.shutdown {
				// The parked request is already waiting when the proxy stops.
				go func() {
					waitForWorkflowDeadline(t, inj.reauth)
					inj.reauth.stop()
				}()
			}
			req := httptest.NewRequest(http.MethodPost, "https://portal.sso.eu-west-2.amazonaws.com/federation/credentials", nil)
			rec := httptest.NewRecorder()
			p.serveMITMRequest(rec, req, "portal.sso.eu-west-2.amazonaws.com", 443)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401 — a 502 is a transport error the SDK retries", rec.Code)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %v; got %q", err, rec.Body.String())
			}
			if body["__type"] != "UnauthorizedException" {
				t.Errorf("__type = %q, want UnauthorizedException", body["__type"])
			}
			if body["message"] != tc.sentence {
				t.Errorf("message = %q, want %q — the body must not misstate WHY the hold ended", body["message"], tc.sentence)
			}
			// I8 — nothing credential-shaped in the body the sandbox reads.
			if strings.Contains(rec.Body.String(), "Bearer") || strings.Contains(rec.Body.String(), approvalID.String()) {
				t.Errorf("the refusal body leaked something it should not: %q", rec.Body.String())
			}
			_ = p.sink.close(context.Background())
			switch got := strings.Count(buf.String(), ruleSourceCredentialReauthTimeout); {
			case tc.wantRow && got != 1:
				t.Errorf("%s rows = %d, want exactly 1: %s", ruleSourceCredentialReauthTimeout, got, buf.String())
			case !tc.wantRow && got != 0:
				t.Errorf("a hold that did NOT expire wrote %d %s row(s) — the trail claims the owner had the whole window: %s",
					got, ruleSourceCredentialReauthTimeout, buf.String())
			}
		})
	}
}

// A client that hung up is written nothing — no 401, no deny row (security
// SHOULD-1). The first shape ended the workflow with an expiry whenever its
// first caller's ctx died, so a disconnect was recorded as "nobody signed in
// before the hold expired" for a hold that still had minutes left, and every
// other waiter was handed that terminal answer.
func TestMITMReauthClientDisconnectWritesNothing(t *testing.T) {
	prevPoll := holdPollInterval
	holdPollInterval = 5 * time.Millisecond
	defer func() { holdPollInterval = prevPoll }()
	t.Setenv(envCredentialReauthTimeout, "1800s")

	approvalID := uuid.New()
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusLocked)
		_, _ = w.Write([]byte(`{"state":"reauth_pending","approval_id":"` + approvalID.String() + `"}`))
	}))
	defer cp.Close()

	inj := &injector{
		byHost:    map[string]*injEntry{"portal.sso.eu-west-2.amazonaws.com": {grantID: uuid.New(), expiresAt: time.Now().Add(-time.Hour).UnixMilli()}},
		base:      cp.URL,
		token:     newTokenSource("tok"),
		client:    cp.Client(),
		reauth:    newReauthCoordinator(),
		approvals: &fakeApprovalReader{steps: pending(1)}, // PENDING forever
	}
	t.Cleanup(inj.reauth.stop)
	p, buf := newLocalRouteProxy(t, cp.URL, "RUNTOK", upstreamAddr(cp), inj, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "https://portal.sso.eu-west-2.amazonaws.com/federation/credentials", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	p.serveMITMRequest(rec, req, "portal.sso.eu-west-2.amazonaws.com", 443)

	if rec.Body.Len() != 0 {
		t.Errorf("a hung-up client was written %q", rec.Body.String())
	}
	_ = p.sink.close(context.Background())
	if strings.Contains(buf.String(), ruleSourceCredentialReauthTimeout) {
		t.Errorf("a disconnect was recorded as a hold expiry: %s", buf.String())
	}
	// …and the hold itself is UNTOUCHED: the owner may still be signing in, and
	// the sandbox's next retry joins this same workflow.
	inj.reauth.mu.Lock()
	wf := inj.reauth.workflows[approvalID]
	inj.reauth.mu.Unlock()
	if wf == nil || wf.finished() {
		t.Error("the disconnect ended the hold; the next retry would open a second one for the same lapse")
	}
}
