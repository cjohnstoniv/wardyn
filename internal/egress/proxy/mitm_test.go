// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
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
