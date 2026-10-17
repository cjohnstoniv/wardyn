// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

const refreshGatewayHost = "llm-gateway.corp.internal"

// refreshingGatewayProxy is gatewayProxy whose gateway credential is due for
// re-resolve, against a control plane answering every re-resolve with status.
func refreshingGatewayProxy(t *testing.T, status int, gw *capturedUpstream) (*Proxy, *bytes.Buffer) {
	t.Helper()
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"own_key_absent"}`, status)
	}))
	t.Cleanup(cp.Close)
	inj := &injector{
		byHost: map[string]*injEntry{refreshGatewayHost: {
			grantID:   uuid.New(),
			header:    injectedHeader{name: "X-Api-Key", value: "sk-last-good"},
			expiresAt: time.Now().Add(time.Minute).UnixMilli(),
		}},
		base:   cp.URL,
		token:  newTokenSource("tok"),
		client: cp.Client(),
	}
	res := fakeResolver{m: map[string][]net.IP{refreshGatewayHost: ips("10.40.1.5")}}
	return gatewayProxy(t, "https://"+refreshGatewayHost+"/v1", res, upstreamAddr(gw.srv), inj)
}

// An outage the injector rides out leaves the brokered route as it was: the
// request is forwarded on the last-good credential.
func TestLLMRouteForwardsOnTheLastGoodCredential(t *testing.T) {
	gw := captureUpstream(t, true, `{"ok":true}`)
	p, _ := refreshingGatewayProxy(t, http.StatusServiceUnavailable, gw)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader("{}")))
	if rec.Code != http.StatusOK || !gw.reached || gw.header.Get("X-Api-Key") != "sk-last-good" {
		t.Fatalf("status = %d reached=%v key=%q, want the request forwarded on the last-good credential",
			rec.Code, gw.reached, gw.header.Get("X-Api-Key"))
	}
}

// A host with no injection rule still gets the below-policy 404.
func TestLLMRouteWithoutARuleStillAnswersNoCredential(t *testing.T) {
	gw := captureUpstream(t, true, `{"ok":true}`)
	p, _ := gatewayProxy(t, "https://"+refreshGatewayHost+"/v1",
		fakeResolver{m: map[string][]net.IP{refreshGatewayHost: ips("10.40.1.5")}}, upstreamAddr(gw.srv),
		staticInj(map[string]injectedHeader{"other.example": {name: "X-Api-Key", value: "K"}}))
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader("{}")))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no_llm_credential") || gw.reached {
		t.Fatalf("status = %d body=%q reached=%v, want the no_llm_credential 404", rec.Code, rec.Body.String(), gw.reached)
	}
}

// A credential the control plane refused to refresh is a refresh failure, not
// a route with no credential: the answer and the row say so, with the reason.
func TestLLMRouteReportsAFailedCredentialRefresh(t *testing.T) {
	gw := captureUpstream(t, true, `{"ok":true}`)
	p, rows := refreshingGatewayProxy(t, http.StatusFailedDependency, gw)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader("{}")))
	body := rec.Body.String()
	if rec.Code != http.StatusBadGateway || !strings.Contains(body, "llm credential refresh failed") ||
		strings.Contains(body, llmBelowPolicyClause) || gw.reached {
		t.Fatalf("status = %d body=%q reached=%v, want a 502 naming the failed refresh", rec.Code, body, gw.reached)
	}
	d := lastDecision(t, rows)
	if d.Decision != egress.Deny || d.RuleSource != ruleSourceLLM || !strings.Contains(d.Cause, "424") ||
		strings.Contains(d.Cause, "sk-last-good") {
		t.Fatalf("row = %s/%s cause %q, want a %s deny whose cause names the refusal", d.RuleSource, d.Decision, d.Cause, ruleSourceLLM)
	}
}

// A caller that hung up before the refresh was answered was never refused.
func TestLLMRouteWritesNoRefusalForACallerThatLeft(t *testing.T) {
	gw := captureUpstream(t, true, `{"ok":true}`)
	p, rows := refreshingGatewayProxy(t, http.StatusFailedDependency, gw)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader("{}")).WithContext(ctx)
	p.ServeHTTP(httptest.NewRecorder(), req)
	if got := rows.String(); got != "" || gw.reached {
		t.Fatalf("rows = %q reached=%v, want no row and nothing forwarded", got, gw.reached)
	}
}
