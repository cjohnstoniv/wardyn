// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestBrokeredLLMRouteKeepsSSRFGuardOnPublicVendorHost pins F087: the RELAXED
// per-request vet (gatewayTarget -> vetTrustedHost, which admits RFC1918/ULA/
// CGNAT by design) belongs to a CONTROL-PLANE-authored gateway host and to
// nothing else. Applied to every host the brokered route dials, it stripped the
// unconditional private-IP guard off `api.anthropic.com` on a run with NO
// gateway configured at all — so a poisoned/split-horizon resolver answering
// RFC1918 for the public vendor host got the startup-minted brokered credential
// delivered to it.
//
// THREAT-MODEL.md residual #29 and OPERATIONS.md both already scope the
// relaxation to the GATEWAY; this is the test that makes the code say so.
func TestBrokeredLLMRouteKeepsSSRFGuardOnPublicVendorHost(t *testing.T) {
	cu := captureUpstream(t, true, `{"ok":true}`)
	buf := &bytes.Buffer{}
	p := newProxy(Options{
		RunID:  uuid.New(),
		Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{anthropicHost}}),
		Sink:   &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 32)},
		// No LLMUpstreams: there IS no configured gateway on this run.
		Resolver: fakeResolver{m: map[string][]net.IP{anthropicHost: ips("10.40.1.5")}},
		Injector: staticInj(map[string]injectedHeader{
			anthropicHost: {name: "X-Api-Key", value: "BROKERED-KEY"},
		}),
		Dial:            redirectDial(upstreamAddr(cu.srv)),
		TLSClientConfig: testInsecureTLSConfig,
	})

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages",
		strings.NewReader(anthropicMessagesBody("hello")))
	p.ServeHTTP(rec, req)

	if cu.reached {
		t.Fatalf("the brokered credential was delivered to an RFC1918 answer for %s (status=%d, upstream saw %q): "+
			"the public vendor host is NOT a control-plane-authored gateway, so it must take egressTarget's "+
			"unconditional private-IP guard like any other host",
			anthropicHost, rec.Code, cu.header.Get("X-Api-Key"))
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (the SSRF vet refused the target)", rec.Code)
	}
}

// TestBrokeredLLMRouteStillUsesTheRelaxedVetForAConfiguredGateway is the
// other half of F087's fix: an operator-configured internal gateway on RFC1918
// space is exactly what vetTrustedHost exists to admit, and narrowing the vet
// to gateway hosts must not break it.
func TestBrokeredLLMRouteStillUsesTheRelaxedVetForAConfiguredGateway(t *testing.T) {
	cu := captureUpstream(t, true, `{"ok":true}`)
	p := newProxy(Options{
		RunID:        uuid.New(),
		Policy:       CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{anthropicHost, "gw.corp.internal"}}),
		Sink:         &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 32)},
		LLMUpstreams: map[string]string{anthropicHost: "http://gw.corp.internal:8443"},
		Resolver:     fakeResolver{m: map[string][]net.IP{"gw.corp.internal": ips("10.40.1.5")}},
		Injector: staticInj(map[string]injectedHeader{
			"gw.corp.internal": {name: "X-Api-Key", value: "BROKERED-KEY"},
		}),
		Dial:            redirectDial(upstreamAddr(cu.srv)),
		TLSClientConfig: testInsecureTLSConfig,
	})

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"v1/messages",
		strings.NewReader(anthropicMessagesBody("hello")))
	p.ServeHTTP(rec, req)

	if !cu.reached || rec.Code != http.StatusOK {
		t.Fatalf("a CONFIGURED gateway on RFC1918 must still be reached with the relaxed vet: status=%d reached=%v",
			rec.Code, cu.reached)
	}
}
