// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
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
