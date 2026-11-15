// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gwidGatewayDials counts the dials to the gateway's address, so a refusal can show that nothing
// left at all, even a request the gateway's TLS server would drop before its handler saw it.
func gwidGatewayDials(g *gwidRig) *atomic.Int64 {
	n := &atomic.Int64{}
	dial := g.p.dial
	g.p.dial = func(ctx context.Context, network, target string) (net.Conn, error) {
		if strings.HasPrefix(target, "93.184.216.10:") {
			n.Add(1)
		}
		return dial(ctx, network, target)
	}
	return n
}

// An absolute-form request (no CONNECT) that a configured identity route covers gets what the
// brokered and terminated lanes give it: the minted token, a fresh call id and a scrubbed answer,
// or nothing sent at all. A cleartext one goes out over TLS, so the token never rides cleartext.
// REVERT PROOF: forwarding a route's request through applyInjection and p.relay alone, as the
// plain lane does for any other host, fails every case.
func TestGatewayIdentity_PlainLaneRouteSignedOrNotSent(t *testing.T) {
	route := "https://" + gwidHost + "/v1/messages"
	for _, tc := range []struct {
		name, url string
		arrange   func(*gwidRig)
		want      int
		says      string
	}{
		{"healthy mint", route, func(*gwidRig) {}, http.StatusOK, `"X-Wardyn-Identity":["<secret-hidden>"]`},
		{"cleartext on the route's port", "http://" + gwidHost + ":443/v1/messages", func(*gwidRig) {}, http.StatusOK, `"X-Wardyn-Identity":["<secret-hidden>"]`},
		{"mint failure", route, func(g *gwidRig) { g.answerStatus(http.StatusInternalServerError) }, http.StatusBadGateway, "gateway identity"},
		{"access cut", route, func(g *gwidRig) { g.answerBody(http.StatusForbidden, gwidCutBody) }, http.StatusForbidden, gwidCutSentence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newGwidRig(t, gwidRoutes(), false)
			tc.arrange(g)
			dials := gwidGatewayDials(g)
			rec := httptest.NewRecorder()
			g.p.servePlain(rec, gwidForge(httptest.NewRequest(http.MethodPost, tc.url, strings.NewReader(`{"hi":1}`))))
			if rec.Code != tc.want || !strings.Contains(rec.Body.String(), tc.says) {
				t.Fatalf("status %d, want %d saying %q: %s", rec.Code, tc.want, tc.says, rec.Body)
			}
			if tc.want != http.StatusOK {
				if dials.Load() != 0 || g.gw.count() != 0 {
					t.Fatalf("a refused call reached the gateway: %d dials, %d requests", dials.Load(), g.gw.count())
				}
				return
			}
			if g.gw.count() != 1 {
				t.Fatalf("gateway requests %d, want 1", g.gw.count())
			}
			token := g.lastMinted(t)
			seen := g.gw.last(t)
			if got := seen.Header.Values(gatewayIdentityHeader); !slices.Equal(got, []string{token}) {
				t.Fatalf("gateway saw identity %q, want only the minted token", got)
			}
			callID := seen.Header.Get(gatewayCallIDHeader)
			if _, err := uuid.Parse(callID); err != nil || callID == gwidCallID {
				t.Errorf("call id %q, want a fresh one the proxy made", callID)
			}
			if !strings.Contains(g.buf.String(), fmt.Sprintf("%q:%q", "gateway_call_id", callID)) {
				t.Errorf("no decision row carries the call id: %s", g.buf)
			}
			if seen.Header.Get("X-Api-Key") != "BROKERED-MODEL-KEY" {
				t.Errorf("the model credential was lost: %v", seen.Header)
			}
			gwidNotIn(t, "response", rec.Body.String()+fmt.Sprint(rec.Header()), token)
			gwidNotIn(t, "decision rows", g.buf.String(), token)
		})
	}
}

// A route's call whose scheme the tail changes keeps the port the proxy judged. The transport
// pools by the outgoing URL's address, so a URL that left the port out would name the new
// scheme's default port: the call would ride a connection to that other port's service, token
// and credential on it, and that service's calls would ride the gateway's. REVERT PROOF: setting
// only outReq.URL.Scheme fails both cases at the first count.
func TestGatewayIdentity_PlainLaneRouteKeepsTheJudgedPort(t *testing.T) {
	send := func(g *gwidRig, url string) int {
		rec := httptest.NewRecorder()
		g.p.servePlain(rec, gwidForge(httptest.NewRequest(http.MethodPost, url, strings.NewReader(`{"hi":1}`))))
		return rec.Code
	}
	t.Run("cleartext with no port to a TLS route on 80", func(t *testing.T) {
		routes := gwidRoutes()
		routes[0].Port = 80 // the rig dials every address but the gateway host's 443 to g.other
		g := newGwidRig(t, routes, false)
		if code := send(g, "https://"+gwidHost+"/other"); code != http.StatusOK || g.gw.count() != 1 {
			t.Fatalf("warming the 443 service: status %d, %d requests", code, g.gw.count())
		}
		if code := send(g, "http://"+gwidHost+"/v1/messages"); code != http.StatusOK || g.gw.count() != 1 || g.other.count() != 1 {
			t.Fatalf("status %d: port 443 saw %d requests and the route's port 80 saw %d, want 1 and 1", code, g.gw.count(), g.other.count())
		}
		if seen := g.other.last(t); seen.Host != gwidHost+":80" || seen.Header.Get(gatewayIdentityHeader) != g.lastMinted(t) {
			t.Errorf("the gateway saw Host %q and identity %q", seen.Host, seen.Header.Get(gatewayIdentityHeader))
		}
		if code := send(g, "https://"+gwidHost+"/other"); code != http.StatusOK || g.gw.count() != 2 || g.other.count() != 1 {
			t.Errorf("status %d: a later call to 443 rode the gateway's connection (%d, %d)", code, g.gw.count(), g.other.count())
		}
	})
	t.Run("TLS with no port to a cleartext route on 443", func(t *testing.T) {
		g := newGwidRig(t, gwidRoutes(), false)
		g.p.mitmPlaintext = map[string]bool{plaintextKey(hostKey(gwidHost), 443): true}
		var on80, on443 atomic.Int64
		var seen atomic.Pointer[http.Request]
		srv80 := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { on80.Add(1) }))
		srv443 := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { on443.Add(1); seen.Store(r) }))
		t.Cleanup(srv80.Close)
		t.Cleanup(srv443.Close)
		dial := g.p.dial
		g.p.dial = func(ctx context.Context, network, target string) (net.Conn, error) {
			switch target {
			case "93.184.216.10:80":
				return (&net.Dialer{}).DialContext(ctx, network, upstreamAddr(srv80))
			case "93.184.216.10:443":
				return (&net.Dialer{}).DialContext(ctx, network, upstreamAddr(srv443))
			}
			return dial(ctx, network, target)
		}
		if code := send(g, "http://"+gwidHost+"/other"); code != http.StatusOK || on80.Load() != 1 {
			t.Fatalf("warming the port 80 service: status %d, %d requests", code, on80.Load())
		}
		if code := send(g, "https://"+gwidHost+"/v1/messages"); code != http.StatusOK || on80.Load() != 1 || on443.Load() != 1 {
			t.Fatalf("status %d: port 80 saw %d requests and the route's port 443 saw %d, want 1 and 1", code, on80.Load(), on443.Load())
		}
		if r := seen.Load(); r.Host != gwidHost+":443" || r.Header.Get(gatewayIdentityHeader) != g.lastMinted(t) {
			t.Errorf("the gateway saw Host %q and identity %q", r.Host, r.Header.Get(gatewayIdentityHeader))
		}
	})
}

// A call whose authority ends in an empty port ("host:") is judged as port 0, so no route covers it
// and it is not signed. It must not then ride the connection a signed call left open to the route's
// port. REVERT PROOF: with the outgoing URL's host left as the sandbox wrote it, both calls reach
// the gateway with the brokered key and no identity.
func TestGatewayIdentity_PlainLaneEmptyPortStaysOffTheRoute(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	send := func(url string) int {
		rec := httptest.NewRecorder()
		g.p.servePlain(rec, httptest.NewRequest(http.MethodPost, url, strings.NewReader(`{"hi":1}`)))
		return rec.Code
	}
	if code := send("https://" + gwidHost + "/v1/messages"); code != http.StatusOK || g.gw.count() != 1 {
		t.Fatalf("the signed call: status %d, %d gateway requests", code, g.gw.count())
	}
	for _, path := range []string{"/v1/messages", "/other"} {
		send("https://" + gwidHost + ":" + path)
		if seen := g.gw.last(t); g.gw.count() != 1 {
			t.Fatalf("%s with an empty port reached the gateway: key %q, identity %q", path, seen.Header.Get("X-Api-Key"), seen.Header.Get(gatewayIdentityHeader))
		}
	}
}

// A host name the transport would map onto the route's host (full-width letters, a soft hyphen) is
// judged as another host, so no route covers it and it is not signed. It must not then ride the
// connection a signed call left open to the route's host and port. The look-alike resolves to the
// other server, and a wildcard entry allows it. REVERT PROOF: without the refusal in servePlain both
// calls reach the gateway with no identity.
func TestGatewayIdentity_PlainLaneLookAlikeHostStaysOffTheRoute(t *testing.T) {
	for _, look := range []string{"\uff47\uff57.corp.test", "g\u00adw.corp.test"} {
		g := newGwidRig(t, gwidRoutes(), false)
		pol := CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"*.corp.test"}})
		g.p.policy, g.p.evaluator = pol, builtinEvaluator{p: pol}
		g.p.res = fakeResolver{m: map[string][]net.IP{gwidHost: ips("93.184.216.10"), look: ips("93.184.216.20"), "wardynd.test": ips("10.9.9.9")}}
		send := func(url string) int {
			rec := httptest.NewRecorder()
			g.p.servePlain(rec, httptest.NewRequest(http.MethodPost, url, strings.NewReader(`{"hi":1}`)))
			return rec.Code
		}
		if code := send("https://" + gwidHost + "/v1/messages"); code != http.StatusOK || g.gw.count() != 1 {
			t.Fatalf("the signed call: status %d, %d gateway requests", code, g.gw.count())
		}
		for _, path := range []string{"/v1/messages", "/other"} {
			if code := send("https://" + look + path); code != http.StatusBadRequest || g.gw.count() != 1 || g.other.count() != 0 {
				t.Fatalf("%q%s: status %d, %d gateway requests, %d at the other server; want 400, 1 and none", look, path, code, g.gw.count(), g.other.count())
			}
		}
	}
}
