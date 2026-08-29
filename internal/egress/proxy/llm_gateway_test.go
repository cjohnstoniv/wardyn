// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gatewayProxy builds a Proxy with an internal Anthropic gateway configured
// (Config.LLMUpstreams under a different name — Options.LLMUpstreams here),
// its dialer redirecting every forward-egress dial to upstreamAddr, and res
// resolving the gateway hostname. Upstream: nil per the plan's binding
// resolution — the corp-upstream interaction gets its own test.
func gatewayProxy(t *testing.T, gatewayBase string, res resolver, upstreamAddr string, inj *injector) (*Proxy, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 64)}
	return newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		Sink:            sink,
		Resolver:        res,
		Dial:            redirectDial(upstreamAddr),
		Injector:        inj,
		LLMUpstreams:    map[string]string{anthropicHost: gatewayBase},
		TLSClientConfig: testInsecureTLSConfig,
	}), buf
}

// TestLLMGateway_BrokeredRouteDialsGateway_PublicHostSeesZero_KeyInjected:
// the brokered /wardyn/llm/anthropic route dials a configured internal
// gateway on an RFC1918 address with NO upstream proxy and InternalHosts
// EMPTY — the gateway host needs no InternalHosts declaration at all — and
// the brokered credential is injected while the sandbox's own is stripped.
// The public api.anthropic.com host is never even referenced by this path,
// proven by a spy server that receives nothing.
func TestLLMGateway_BrokeredRouteDialsGateway_PublicHostSeesZero_KeyInjected(t *testing.T) {
	publicSpy := captureUpstream(t, true, "should-never-be-hit")

	gw := captureUpstream(t, true, `{"ok":true}`)
	res := fakeResolver{m: map[string][]net.IP{"llm-gateway.corp.internal": ips("10.40.1.5")}}
	inj := staticInj(map[string]injectedHeader{"llm-gateway.corp.internal": {name: "X-Api-Key", value: "BROKERED-KEY"}})
	p, buf := gatewayProxy(t, "https://llm-gateway.corp.internal/v1", res, upstreamAddr(gw.srv), inj)

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader(`{"hi":1}`))
	req.Header.Set("Authorization", "Bearer SANDBOX-SMUGGLED")
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if !gw.reached {
		t.Fatal("the configured gateway must be reached")
	}
	if got := gw.header.Get("X-Api-Key"); got != "BROKERED-KEY" {
		t.Fatalf("gateway X-Api-Key = %q, want the brokered credential", got)
	}
	if got := gw.header.Get("Authorization"); got != "" {
		t.Fatalf("sandbox Authorization must be stripped, gateway saw %q", got)
	}
	if want := "/v1/messages"; gw.path != want {
		t.Fatalf("gateway path = %q, want %q (path prefix + rest)", gw.path, want)
	}
	if publicSpy.reached {
		t.Fatal("api.anthropic.com must see ZERO requests once a gateway is configured")
	}
	if d := lastDecision(t, buf); d.Decision != egress.Allow {
		t.Fatalf("decision = %+v, want allow", d)
	}
}

// TestLLMGateway_SameHostOnConnectPath_StillBuiltinPrivateIP: the gateway
// hostname's own RESOLVED IP, reached directly (not through the brokered
// route, and not declared via SiteConfig.InternalHosts), still hits the
// unconditional private-IP guard — the gateway lift lives in the
// gatewayVendor table keyed by HOSTNAME, never by the address it resolves
// to, so a literal-IP CONNECT to the same target is not silently widened.
func TestLLMGateway_SameHostOnConnectPath_StillBuiltinPrivateIP(t *testing.T) {
	res := fakeResolver{m: map[string][]net.IP{"llm-gateway.corp.internal": ips("10.40.1.5")}}
	p, _ := gatewayProxy(t, "https://llm-gateway.corp.internal/v1", res, "127.0.0.1:1", nil)

	// Literal IP address (what the gateway hostname resolves to), requested
	// directly: p.gatewayVendor has no entry for the bare IP, and no
	// InternalHosts entry was declared, so this must be denied.
	guard := p.vetHost("10.40.1.5")
	if !guard.Denied {
		t.Fatalf("the gateway's own resolved IP, reached by literal address, must stay denied, got %+v", guard)
	}
}

// TestLLMGateway_ResolverAnswersMetadata_RefusedNeverDialled: a gateway
// hostname whose resolver answers the metadata address is refused per
// request and the dialer is NEVER invoked (fail closed before any network
// activity) — the decision log names "builtin:dial-failed", not the
// misleading allow-shaped "brokered:llm".
func TestLLMGateway_ResolverAnswersMetadata_RefusedNeverDialled(t *testing.T) {
	dialed := false
	res := fakeResolver{m: map[string][]net.IP{"llm-gateway.corp.internal": ips("169.254.169.254")}}
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)}
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{}),
		Sink:     sink,
		Resolver: res,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialed = true
			return nil, errors.New("must not be called")
		},
		Injector:     staticInj(map[string]injectedHeader{"llm-gateway.corp.internal": {name: "X-Api-Key", value: "K"}}),
		LLMUpstreams: map[string]string{anthropicHost: "https://llm-gateway.corp.internal/v1"},
	})

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader("{}"))
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if dialed {
		t.Fatal("a refused gateway resolution must never reach the dialer")
	}
	if d := lastDecision(t, buf); d.RuleSource != "builtin:dial-failed" || d.Decision != egress.Deny {
		t.Fatalf("decision = %+v, want deny/builtin:dial-failed", d)
	}
}

// TestLLMGateway_ResolverError_502DialFailed_RunUnaffected: a resolve
// failure for the gateway host 502s that one request; the run (this proxy
// instance) keeps serving — a second, resolvable request still succeeds.
func TestLLMGateway_ResolverError_502DialFailed_RunUnaffected(t *testing.T) {
	res := &toggleResolver{err: errors.New("dns down")}
	gw := captureUpstream(t, true, "ok")
	inj := staticInj(map[string]injectedHeader{"llm-gateway.corp.internal": {name: "X-Api-Key", value: "K"}})
	p, buf := gatewayProxy(t, "https://llm-gateway.corp.internal/v1", res, upstreamAddr(gw.srv), inj)

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader("{}"))
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if d := lastDecision(t, buf); d.RuleSource != "builtin:dial-failed" {
		t.Fatalf("rule_source = %q, want builtin:dial-failed", d.RuleSource)
	}

	// The run is unaffected: flip the resolver to succeed and the SAME proxy
	// instance serves the next request normally.
	res.err = nil
	res.ips = ips("10.40.1.5")
	rec2 := httptest.NewRecorder()
	req2 := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader("{}"))
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200 (the run must not be wedged by the first failure)", rec2.Code)
	}
}

// toggleResolver lets a test flip between a resolve failure and a fixed
// answer without rebuilding the Proxy.
type toggleResolver struct {
	err error
	ips []net.IP
}

func (r *toggleResolver) LookupIP(string) ([]net.IP, error) { return r.ips, r.err }

// TestLLMGateway_NonDefaultPortAndPrefix: a gateway base URL carrying a
// non-443 port and a path prefix is preserved end to end — the Host header
// includes the port (never omitted except at 443) and the forwarded path is
// prefix+rest.
func TestLLMGateway_NonDefaultPortAndPrefix(t *testing.T) {
	res := fakeResolver{m: map[string][]net.IP{"llm-gateway.corp.internal": ips("10.40.1.5")}}
	gw := captureUpstream(t, true, "ok")
	inj := staticInj(map[string]injectedHeader{"llm-gateway.corp.internal": {name: "X-Api-Key", value: "K"}})
	p, _ := gatewayProxy(t, "https://llm-gateway.corp.internal:8443/v1beta", res, upstreamAddr(gw.srv), inj)

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader("{}"))
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	if want := "llm-gateway.corp.internal:8443"; gw.host != want {
		t.Fatalf("upstream Host = %q, want %q (non-443 port must not be omitted)", gw.host, want)
	}
	if want := "/v1beta/messages"; gw.path != want {
		t.Fatalf("upstream path = %q, want %q (prefix + rest)", gw.path, want)
	}
}

// TestLLMGateway_UpstreamFirst_CorpProxySeesCONNECT_DirectSeesZero: with a
// corporate upstream ALSO configured, the gateway is dialled THROUGH it (the
// stated ceiling: upstream-first, no bypass) — the corp proxy sees a CONNECT
// naming the gateway host:port, never a direct dial.
func TestLLMGateway_UpstreamFirst_CorpProxySeesCONNECT_DirectSeesZero(t *testing.T) {
	f := startFakeUpstream(t)
	up, err := parseUpstreamProxy("http://" + f.addr())
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)}
	p := newProxy(Options{
		RunID:        uuid.New(),
		Policy:       CompilePolicy(types.RunPolicySpec{}),
		Sink:         sink,
		Resolver:     publicResolver{},
		Upstream:     up,
		Injector:     staticInj(map[string]injectedHeader{"llm-gateway.corp.internal": {name: "X-Api-Key", value: "K"}}),
		LLMUpstreams: map[string]string{anthropicHost: "https://llm-gateway.corp.internal:8443/v1"},
	})

	rec := httptest.NewRecorder()
	req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader("{}"))
	p.ServeHTTP(rec, req)
	gotConnect, _ := f.snapshot()
	if gotConnect != "CONNECT llm-gateway.corp.internal:8443" {
		t.Fatalf("corp proxy saw %q, want CONNECT llm-gateway.corp.internal:8443 (upstream-first: the gateway is dialled THROUGH the corp proxy, never directly)", gotConnect)
	}
}
