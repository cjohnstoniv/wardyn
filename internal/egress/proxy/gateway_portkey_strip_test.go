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

// A gateway that honours x-portkey-* routing headers (Portkey's open-source gateway reads
// x-portkey-custom-host and sends the request, with the credential, to that host) would let a
// sandbox's own header redirect the stored key. The proxy drops every sandbox x-portkey-* header on
// a configured gateway host, on both the brokered route, the terminated-tunnel lane and the plain forward lane, and keeps
// the one header the rule owns.

const portkeyGatewayHost = "portkey.corp.test"

type portkeyLane struct {
	name string
	send func(t *testing.T, p *Proxy, host string, h http.Header) *httptest.ResponseRecorder
}

var portkeyLanes = []portkeyLane{
	{"brokered", func(t *testing.T, p *Proxy, host string, h http.Header) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+"messages", strings.NewReader(`{"hi":1}`))
		req.Header = h
		p.ServeHTTP(rec, req)
		return rec
	}},
	{"mitm", func(t *testing.T, p *Proxy, host string, h http.Header) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "https://"+host+"/v1/messages", strings.NewReader(`{"hi":1}`))
		req.Header = h
		p.serveMITMRequest(rec, req, host, 443)
		return rec
	}},
	// The plain forward lane: an absolute-form https request with no CONNECT.
	{"plain", func(t *testing.T, p *Proxy, host string, h http.Header) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := mustAbsReq(t, http.MethodPost, "https://"+host+"/v1/messages", `{"hi":1}`)
		req.Header = h
		p.ServeHTTP(rec, req)
		return rec
	}},
}

// portkeyProxy builds a proxy whose Anthropic upstream is the gateway host (gateway=true) or the
// vendor host itself (gateway=false), both resolving to a public address and dialled to up.
func portkeyProxy(up *capturedUpstream, host string, gateway bool, inj *injector) *Proxy {
	o := Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{host}}),
		Sink:            &decisionSink{out: &bytes.Buffer{}, ch: make(chan egress.DecisionLog, 64)},
		Resolver:        fakeResolver{m: map[string][]net.IP{host: ips("93.184.216.10")}},
		Dial:            redirectDial(upstreamAddr(up.srv)),
		Injector:        inj,
		TLSClientConfig: testInsecureTLSConfig,
	}
	if gateway {
		o.LLMUpstreams = map[string]string{anthropicHost: "https://" + host + "/v1"}
	}
	return newProxy(o)
}

func portkeySandboxHeaders() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("X-Portkey-Custom-Host", "https://attacker.example")
	h.Set("X-Portkey-Config", `{"retry":{"attempts":3}}`)
	h.Set("X-Portkey-Provider", "openai")
	h.Set("X-Portkey-Api-Key", "SANDBOX-PORTKEY-KEY")
	return h
}

func requireNoPortkeyRouting(t *testing.T, got http.Header) {
	t.Helper()
	for _, k := range []string{"X-Portkey-Custom-Host", "X-Portkey-Config", "X-Portkey-Provider"} {
		if v := got.Values(k); len(v) != 0 {
			t.Errorf("gateway saw sandbox %s = %q, want it dropped", k, v)
		}
	}
}

// The hosted shape: the rule owns x-portkey-api-key. Its brokered value arrives, the sandbox's own
// copy does not, and the sandbox's routing headers are dropped.
func TestGatewayPortkeyStrip_OwnedCredentialHeaderSurvives(t *testing.T) {
	for _, lane := range portkeyLanes {
		t.Run(lane.name, func(t *testing.T) {
			up := captureUpstream(t, true, `{"ok":true}`)
			inj := staticInj(map[string]injectedHeader{portkeyGatewayHost: {name: "x-portkey-api-key", value: "BROKERED-PORTKEY-KEY"}})
			p := portkeyProxy(up, portkeyGatewayHost, true, inj)

			rec := lane.send(t, p, portkeyGatewayHost, portkeySandboxHeaders())
			if rec.Code != http.StatusOK || !up.reached {
				t.Fatalf("status = %d reached = %v body = %q", rec.Code, up.reached, rec.Body.String())
			}
			if got := up.header.Values("X-Portkey-Api-Key"); len(got) != 1 || got[0] != "BROKERED-PORTKEY-KEY" {
				t.Errorf("gateway x-portkey-api-key = %q, want exactly the brokered value", got)
			}
			requireNoPortkeyRouting(t, up.header)
		})
	}
}

// A placeholder rule on an x-portkey-* header is filled, not dropped: the header is the rule's own.
func TestGatewayPortkeyStrip_PlaceholderHeaderIsFilled(t *testing.T) {
	for _, lane := range portkeyLanes {
		t.Run(lane.name, func(t *testing.T) {
			up := captureUpstream(t, true, `{"ok":true}`)
			inj := staticInj(map[string]injectedHeader{portkeyGatewayHost: {name: "X-Portkey-Api-Key", value: "REAL-PORTKEY-KEY", placeholder: "PLACEHOLDER-KEY"}})
			p := portkeyProxy(up, portkeyGatewayHost, true, inj)

			h := portkeySandboxHeaders()
			h.Set("X-Portkey-Api-Key", "PLACEHOLDER-KEY")
			rec := lane.send(t, p, portkeyGatewayHost, h)
			if rec.Code != http.StatusOK || !up.reached {
				t.Fatalf("status = %d reached = %v body = %q", rec.Code, up.reached, rec.Body.String())
			}
			if got := up.header.Values("X-Portkey-Api-Key"); len(got) != 1 || got[0] != "REAL-PORTKEY-KEY" {
				t.Errorf("gateway x-portkey-api-key = %q, want the placeholder filled", got)
			}
			requireNoPortkeyRouting(t, up.header)
		})
	}
}

// Another credential header owns the rule (a gateway keyed by Authorization): every sandbox
// x-portkey-* header goes, including a forged x-portkey-api-key.
func TestGatewayPortkeyStrip_OtherOwnedHeaderDropsAllPortkey(t *testing.T) {
	for _, lane := range portkeyLanes {
		t.Run(lane.name, func(t *testing.T) {
			up := captureUpstream(t, true, `{"ok":true}`)
			inj := staticInj(map[string]injectedHeader{portkeyGatewayHost: {name: "Authorization", value: "Bearer BROKERED"}})
			p := portkeyProxy(up, portkeyGatewayHost, true, inj)

			rec := lane.send(t, p, portkeyGatewayHost, portkeySandboxHeaders())
			if rec.Code != http.StatusOK || !up.reached {
				t.Fatalf("status = %d reached = %v body = %q", rec.Code, up.reached, rec.Body.String())
			}
			if got := up.header.Get("Authorization"); got != "Bearer BROKERED" {
				t.Errorf("Authorization = %q, want the brokered credential", got)
			}
			requireNoPortkeyRouting(t, up.header)
			if got := up.header.Values("X-Portkey-Api-Key"); len(got) != 0 {
				t.Errorf("gateway saw sandbox x-portkey-api-key = %q, want it dropped", got)
			}
		})
	}
}

// A vendor host is no gateway: a sandbox x-portkey-* header passes as any other header does.
func TestGatewayPortkeyStrip_VendorHostUntouched(t *testing.T) {
	for _, lane := range portkeyLanes {
		t.Run(lane.name, func(t *testing.T) {
			up := captureUpstream(t, true, `{"ok":true}`)
			inj := staticInj(map[string]injectedHeader{anthropicHost: {name: "X-Api-Key", value: "BROKERED"}})
			p := portkeyProxy(up, anthropicHost, false, inj)

			rec := lane.send(t, p, anthropicHost, portkeySandboxHeaders())
			if rec.Code != http.StatusOK || !up.reached {
				t.Fatalf("status = %d reached = %v body = %q", rec.Code, up.reached, rec.Body.String())
			}
			for k, want := range map[string]string{
				"X-Portkey-Custom-Host": "https://attacker.example",
				"X-Portkey-Provider":    "openai",
			} {
				if got := up.header.Get(k); got != want {
					t.Errorf("vendor host saw %s = %q, want it passed unchanged (%q)", k, got, want)
				}
			}
		})
	}
}
