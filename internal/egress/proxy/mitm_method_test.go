// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mitmProxyMethods is mitmProxy with allowed_methods spelled out — the one
// policy field this test is about.
func mitmProxyMethods(t *testing.T, methods []string, upstream *httptest.Server) (*Proxy, *bytes.Buffer, []byte) {
	t.Helper()
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 32)}
	p := newProxy(Options{
		RunID: uuid.New(),
		Policy: CompilePolicy(types.RunPolicySpec{
			AllowedDomains: []string{anthropicHost},
			AllowedMethods: methods,
		}),
		Sink:            sink,
		Scanner:         scanEngine(t, "alert", scanTestSecret),
		CA:              ca,
		MITMLLM:         true,
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(upstream)),
		TLSClientConfig: testInsecureTLSConfig,
	})
	return p, buf, certPEM
}

// TestMITMInnerRequestHonoursAllowedMethods pins F107: allowed_methods is
// enforced on the requests INSIDE a TLS-terminated tunnel, not only on the
// CONNECT that opened it.
//
// The CONNECT is evaluated as method "CONNECT" and the inner method is chosen
// by the sandbox afterwards. serveMITMRequest saw the plaintext method and
// never applied Policy.methodAllowed, so with allowed_methods=[GET,CONNECT] —
// evaluate(CONNECT)=allow, evaluate(POST)=deny — an inner POST was forwarded to
// the model host and audited decision=allow. The proxy is holding the
// plaintext here; the restriction the operator wrote has to hold on the one
// lane where it is visible.
//
// (The APPROVAL half of "one CONNECT carries many inner requests" is a
// documented, deliberate exemption and is NOT changed by this test.)
func TestMITMInnerRequestHonoursAllowedMethods(t *testing.T) {
	cu := captureUpstream(t, true, "llm-ok")
	p, buf, caPEM := mitmProxyMethods(t, []string{"GET", "CONNECT"}, cu.srv)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	tlsConn := agentMITMConn(t, proxySrv.URL, caPEM)
	defer tlsConn.Close()

	// A method the policy forbids, sent inside the tunnel.
	req := "POST /v1/messages HTTP/1.1\r\nHost: " + anthropicHost +
		"\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}"
	if _, err := io.WriteString(tlsConn, req); err != nil {
		t.Fatalf("write inner POST: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read inner response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("inner POST under allowed_methods=[GET,CONNECT] = %d, want 403: the method "+
			"restriction must hold on the MITM lane, where the proxy can see the method", resp.StatusCode)
	}
	if cu.reached {
		t.Error("inner POST reached the model upstream: a method-denied request must not be forwarded")
	}
	if !strings.Contains(buf.String(), `"rule_source":"policy:method"`) {
		t.Errorf("decision stream has no policy:method row — the same rule_source the plain lane and "+
			"the CONNECT itself emit. Got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), `"decision":"allow","rule_source":"scan:mitm"`) {
		t.Errorf("a method-denied inner request was audited as an allow:\n%s", buf.String())
	}
}

// TestMITMInnerRequestAllowsPermittedMethod is the no-over-denial half: an
// allowed inner method still rides the tunnel exactly as before.
func TestMITMInnerRequestAllowsPermittedMethod(t *testing.T) {
	cu := captureUpstream(t, true, "llm-ok")
	p, _, caPEM := mitmProxyMethods(t, []string{"GET", "POST", "CONNECT"}, cu.srv)
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	tlsConn := agentMITMConn(t, proxySrv.URL, caPEM)
	defer tlsConn.Close()

	req := "POST /v1/messages HTTP/1.1\r\nHost: " + anthropicHost +
		"\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n{}"
	if _, err := io.WriteString(tlsConn, req); err != nil {
		t.Fatalf("write inner POST: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read inner response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("inner POST under allowed_methods=[GET,POST,CONNECT] = %d, want 200", resp.StatusCode)
	}
	if !cu.reached {
		t.Error("an allowed inner method did not reach the upstream")
	}
}
