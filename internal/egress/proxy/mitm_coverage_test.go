// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// bedrockVPCEHost is the PrivateLink spelling of a Bedrock data-plane endpoint —
// the form internal/api/runs_bedrock.go authors onto MITMHosts when
// WARDYN_BEDROCK_BASE_URL names a VPC endpoint.
const bedrockVPCEHost = "vpce-0abc1234.bedrock-runtime.us-east-1.vpce.amazonaws.com"

// TestMITMGenericChannelBodyIsScannedWhateverTheHostClassification pins F036: on
// a TLS-terminated tunnel the inspection core must be chosen by whether the body
// is PARSEABLE (the channel), never by whether the host is classified as an LLM.
//
// serveMITMRequest used to run inspectForwardBody only when
// `mitmSource == ruleSourceArtifactMITM`, i.e. only when !isLLMHost. Widening
// isBedrockHost to the PrivateLink form — a legitimate matcher fix — therefore
// MOVED vpce Bedrock hosts from the artifact branch (scanned) to the LLM branch,
// where channelForHost is ChannelGeneric and classifyLLM maps that to scanNone:
// the body streamed through unscanned and the row was a bare `scan:mitm` allow
// with no scan block at all, which an auditor reads as "inspected via MITM".
func TestMITMGenericChannelBodyIsScannedWhateverTheHostClassification(t *testing.T) {
	for _, host := range []string{
		bedrockVPCEHost,
		"bedrock-runtime.us-east-1.amazonaws.com",
		"mirror.corp", // the non-LLM control this branch already covered
	} {
		t.Run(host, func(t *testing.T) {
			cu := captureUpstream(t, true, "upstream-ok")
			certPEM, keyPEM := genTestCA(t)
			ca, err := newCertAuthority(certPEM, keyPEM)
			if err != nil {
				t.Fatalf("newCertAuthority: %v", err)
			}
			p := newProxy(Options{
				RunID:           uuid.New(),
				Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{host}}),
				Sink:            &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
				Scanner:         forwardScanEngine(t, "block"),
				CA:              ca,
				MITMHosts:       []string{host},
				Resolver:        publicResolver{},
				TLSClientConfig: testInsecureTLSConfig,
				Dial:            redirectDial(upstreamAddr(cu.srv)),
			})
			proxySrv := httptest.NewServer(p)
			defer proxySrv.Close()

			resp := mitmPost(t, proxySrv.URL, certPEM, host, "/model/anthropic.claude/invoke",
				`{"prompt":"exfiltrate `+scanTestSecret+`"}`)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: a secret in a MITM'd body must be blocked under "+
					"inspect_forward_egress no matter how the HOST is classified (upstream body %q)",
					resp.StatusCode, cu.body)
			}
			if cu.reached {
				t.Fatalf("blocked MITM request reached the upstream with %q", cu.body)
			}
		})
	}
}

// mitmPost drives a real CONNECT + TLS + POST through the proxy's MITM tunnel
// and returns the response the sandbox sees.
func mitmPost(t *testing.T, proxyURL string, caPEM []byte, host, path, body string) *http.Response {
	t.Helper()
	conn, status := connectThrough(t, proxyURL, host+":443")
	t.Cleanup(func() { _ = conn.Close() })
	if !strings.Contains(status, "200") {
		t.Fatalf("CONNECT status = %q, want 200", status)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to add Wardyn CA to the agent trust pool")
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: host, RootCAs: pool})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("agent TLS handshake through the MITM tunnel: %v", err)
	}
	t.Cleanup(func() { _ = tlsConn.Close() })
	req, _ := http.NewRequest(http.MethodPost, "https://"+host+path, strings.NewReader(body))
	req.ContentLength = int64(len(body))
	if err := req.Write(tlsConn); err != nil {
		t.Fatal(err)
	}
	_ = tlsConn.SetReadDeadline(time.Now().Add(10 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		t.Fatalf("read MITM response: %v", err)
	}
	return resp
}

// TestMITMPortClampHoldsOnTheLLMBranchToo pins F009: MITM eligibility must not
// be decidable without the port.
//
// The W13-S1-5 clamp lived inside handleConnect's isCorpMITMHost branch alone,
// so a port MISMATCH fell through to `if p.isLLMHost(host)` -> mitmLLMHost,
// which consulted no port at all. Any operator-configured MITM host that ALSO
// satisfies isLLMHost — a bedrock/vpce host, which dispatch itself authors onto
// MITMHosts, or a configured gateway host — was TLS-terminated and
// credential-injected on ports the operator never configured.
func TestMITMPortClampHoldsOnTheLLMBranchToo(t *testing.T) {
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	p := newProxy(Options{
		RunID:     uuid.New(),
		Policy:    CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{bedrockVPCEHost}}),
		Sink:      &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 8)},
		CA:        ca,
		MITMHosts: []string{bedrockVPCEHost + ":8443"}, // scoped to :8443 only
		MITMLLM:   true,                                // the LLM branch is live on this run
		Resolver:  publicResolver{},
		Dial:      redirectDial(startEcho(t)), // opaque-tunnel stand-in
	})
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	conn, status := connectThrough(t, proxySrv.URL, bedrockVPCEHost+":9999")
	defer conn.Close()
	if !strings.Contains(status, "200") {
		t.Fatalf("policy-allowed host (any port) must still tunnel: %q", status)
	}
	// A MITM'd tunnel answers a ClientHello with the proxy's own TLS server
	// handshake; an OPAQUE one pipes raw bytes, so the echo server returns them
	// verbatim. That is the difference this test is built to see.
	_, _ = io.WriteString(conn, "ping")
	buf := make([]byte, 4)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, rerr := conn.Read(buf)
	if rerr != nil || string(buf[:n]) != "ping" {
		t.Fatalf("expected an opaque passthrough echo of %q on the UNCONFIGURED port, got %q err=%v — "+
			"the corp branch's port clamp was undone by the LLM branch, so the operator's token is "+
			"offered on a port the redirect never named", "ping", buf[:n], rerr)
	}
	// Positive control: the CONFIGURED port is still MITM'd — the clamp must
	// narrow the surface, never close it.
	okConn, okStatus := connectThrough(t, proxySrv.URL, bedrockVPCEHost+":8443")
	defer okConn.Close()
	if !strings.Contains(okStatus, "200") {
		t.Fatalf("CONNECT on the configured port = %q, want 200", okStatus)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		t.Fatal("failed to add Wardyn CA to the agent trust pool")
	}
	tlsConn := tls.Client(okConn, &tls.Config{ServerName: bedrockVPCEHost, RootCAs: pool})
	if herr := tlsConn.Handshake(); herr != nil {
		t.Fatalf("the CONFIGURED port must still be TLS-terminated: %v", herr)
	}
	_ = tlsConn.Close()
}
