// Copyright 2026 The Wardyn Authors
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

// gatewayKeyedHost is the gateway a model provider's BaseURL names: the run's
// credential is keyed on it, never on the vendor host.
const gatewayKeyedHost = "gw.corp.example"

func gatewayKeyedInjector() *injector {
	return staticInj(map[string]injectedHeader{gatewayKeyedHost: {name: "X-Api-Key", value: "OWN-KEY"}})
}

// The shape a local runner would hold for a provider routed through a
// gateway (credential keyed on the gateway, no gateway table) fails closed on
// the brokered route: the route resolves to the vendor host, no rule matches
// it, and the vendor host is never dialled.
func TestGatewayKeyedCredential_BrokeredRouteWithoutTable_FailsClosed(t *testing.T) {
	cu := captureUpstream(t, true, "llm-ok")
	p, buf := newLocalRouteProxy(t, "http://wardynd.test:8080", "RUNTOK", upstreamAddr(cu.srv), gatewayKeyedInjector(), testInsecureTLSConfig)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages", strings.NewReader(`{"hi":1}`)))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no_llm_credential") {
		t.Fatalf("brokered route = %d %q, want the no_llm_credential 404", rec.Code, rec.Body.String())
	}
	if cu.reached {
		t.Fatalf("the vendor host was dialled with body %q", cu.body)
	}
	if d := lastDecision(t, buf); d.RuleSource != ruleSourceLLM || d.Decision != egress.Deny {
		t.Fatalf("decision = %+v, want brokered:llm deny", d)
	}
}

// MITM pin: a run routes the vendor host through a configured gateway
// and its credential is keyed on the gateway. A direct, MITM-terminated
// request to the vendor host has no rule of its own: it is refused with the
// brokered 404, and its body never reaches the vendor host with the sandbox's
// placeholder key.
func TestGatewayKeyedCredential_MITMNeverForwardsBodyToVendor(t *testing.T) {
	cu := captureUpstream(t, true, "llm-ok")
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{anthropicHost}}),
		Sink:            &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 32)},
		Scanner:         scanEngine(t, "alert", scanTestSecret),
		CA:              ca,
		MITMLLM:         true,
		Injector:        gatewayKeyedInjector(),
		LLMUpstreams:    map[string]string{anthropicHost: "https://" + gatewayKeyedHost},
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(cu.srv)),
		TLSClientConfig: testInsecureTLSConfig,
	})
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()
	tlsConn := agentMITMConn(t, proxySrv.URL, certPEM)
	defer tlsConn.Close()

	body := anthropicMessagesBody("private prompt")
	req, _ := http.NewRequest(http.MethodPost, "https://"+anthropicHost+"/v1/messages", strings.NewReader(body))
	req.Header.Set("X-Api-Key", "wardyn-proxy-injected")
	req.Header.Set("Content-Type", "application/json")
	if err := req.Write(tlsConn); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(tlsConn), req)
	if err != nil {
		t.Fatalf("read MITM response: %v", err)
	}
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(rb), "no_llm_credential") {
		t.Fatalf("MITM = %d %q, want the no_llm_credential 404", resp.StatusCode, rb)
	}
	if cu.reached {
		t.Fatalf("the request body reached the vendor host: %q (key %q)", cu.body, cu.header.Get("X-Api-Key"))
	}
}
