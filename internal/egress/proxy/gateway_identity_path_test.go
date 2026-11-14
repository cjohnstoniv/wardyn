// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gwidPathLanes sends path to host and port on each lane that sends a model call: the plain proxy
// request, the terminated tunnel and the brokered route. The brokered lane's vendor upstream is put
// on the route's host with no base path, as when another of the run's providers shares that gateway
// with identity off, so the upstream is sent path itself; it ignores host and port.
func gwidPathLanes() map[string]func(t *testing.T, g *gwidRig, host string, port int, path string) int {
	body := func() *strings.Reader { return strings.NewReader(`{"hi":1}`) }
	return map[string]func(t *testing.T, g *gwidRig, host string, port int, path string) int{
		"plain": func(_ *testing.T, g *gwidRig, host string, port int, path string) int {
			rec := httptest.NewRecorder()
			g.p.servePlain(rec, gwidForge(httptest.NewRequest(http.MethodPost, fmt.Sprintf("https://%s:%d%s", host, port, path), body())))
			return rec.Code
		},
		"terminated": func(_ *testing.T, g *gwidRig, host string, port int, path string) int {
			rec := httptest.NewRecorder()
			g.p.serveMITMRequest(rec, gwidForge(httptest.NewRequest(http.MethodPost, fmt.Sprintf("https://%s:%d%s", host, port, path), body())), host, port)
			return rec.Code
		},
		"brokered": func(t *testing.T, g *gwidRig, _ string, _ int, path string) int {
			g.p.llmUpstreams[anthropicHost] = llmUpstream{host: gwidHost, port: 443}
			rec := httptest.NewRecorder()
			g.p.handleLocalRoute(rec, gwidForge(mustLocalReq(t, http.MethodPost, llmAnthropicPrefix+path[1:], body())))
			return rec.Code
		},
	}
}

// A route's host and port is sent only a request a route covers, on each lane that sends. Any
// other path there is refused before any dial or mint: another casing, an alias, a look-alike, a
// dot or encoded segment. A gateway that serves it as the route's call would take it, credential
// on and identity off, as anonymous. The refusal names the route and what to change. The same
// paths to another host or port go out as before, and every covered spelling still signs. REVERT
// PROOF: without gatewayOffRouteRefused, every plain and terminated row and the brokered rows with
// no dot segment or backslash reach the gateway with no identity.
func TestGatewayIdentity_RouteHostSendsOnlyWhatARouteCovers(t *testing.T) {
	refused := []string{"/other", "/v1evil/x", "/V1/messages", "/chat/completions", "/", "/v1./messages", "/%252e%252e/v1/messages",
		"/v1/./messages", "/x/../v1/messages", "//v1/messages", "/v1/%2e/messages", "/%2E%2E/v1/messages", "/%2fv1/messages",
		"/v1/../other", "/;x/v1/messages", "/v1;x/messages", "/v1%5cmessages"}
	signed := []string{"/v1/messages", "/v1%2fmessages", "/%761/messages", "/v1", "/v1/messages/", "/v1//messages"}
	for name, send := range gwidPathLanes() {
		t.Run(name, func(t *testing.T) {
			g := newGwidRig(t, gwidRoutes(), false)
			dials := gwidGatewayDials(g)
			want := len(refused)
			for _, path := range refused {
				if code := send(t, g, gwidHost, 443, path); code != http.StatusBadRequest {
					t.Errorf("%s: status %d, want 400", path, code)
				}
			}
			for _, host := range []string{strings.ToUpper(gwidHost), gwidHost + "."} {
				if name == "brokered" {
					break // the brokered lane's host is its configured upstream's
				}
				want++
				if code := send(t, g, host, 443, "/other"); code != http.StatusBadRequest {
					t.Errorf("/other to %s: status %d, want 400", host, code)
				}
			}
			if dials.Load() != 0 || g.gw.count() != 0 || g.mint.hits.Load() != 0 {
				t.Fatalf("a refused path left: %d dials, %d gateway requests, %d mints", dials.Load(), g.gw.count(), g.mint.hits.Load())
			}
			// A dot segment or a backslash on the brokered lane is refused earlier, with that lane's own row.
			if rows := strings.Count(g.buf.String(), gatewayOffRouteFix); name != "brokered" && rows != want {
				t.Errorf("%d decision rows carry the refusal, want %d", rows, want)
			}
			if !strings.Contains(g.buf.String(), "the call was not sent: run identity is on at this host and port, and this path is outside every route here (provider prov-1, path prefix /v1) or has a dot segment or a backslash. Remove any dot segments or backslashes. For a path outside these routes, widen the intended route's path prefix or give the other service its own host and port.") {
				t.Errorf("no refusal row names the route: %s", g.buf)
			}
			for _, path := range refused {
				for _, to := range []struct {
					host string
					port int
				}{{gwidOther, 443}, {gwidHost, 8443}} {
					if name == "brokered" {
						break // the brokered lane sends only to its configured upstream
					}
					before := g.other.count()
					if code := send(t, g, to.host, to.port, path); code != http.StatusOK || g.other.count() != before+1 {
						t.Errorf("%s to %s:%d: status %d, want it sent as before", path, to.host, to.port, code)
					}
				}
			}
			for i, path := range signed {
				code := send(t, g, gwidHost, 443, path)
				if code != http.StatusOK || g.gw.count() != i+1 || g.gw.last(t).Header.Get(gatewayIdentityHeader) != g.lastMinted(t) {
					t.Errorf("%s: status %d, %d gateway requests, want each covered spelling signed", path, code, g.gw.count())
				}
			}
			if dials.Load() == 0 {
				t.Error("the dial counter saw no dial, so its zero above proves nothing")
			}
		})
	}
}

// A route on the whole host and port, the bare base URL the docs recommend, covers every clean path,
// so each one still signs on every lane and nothing is refused for being off the route. REVERT
// PROOF: refusing on the host and port alone, whatever the prefix, fails every row.
func TestGatewayIdentity_WholeHostRouteSignsEveryCleanPath(t *testing.T) {
	paths := []string{"/v1/messages", "/other", "/V1/messages", "/chat/completions", "/v1evil/x", "/v1//messages"}
	for name, send := range gwidPathLanes() {
		t.Run(name, func(t *testing.T) {
			routes := gwidRoutes()
			routes[0].PathPrefix = ""
			g := newGwidRig(t, routes, false)
			for i, path := range paths {
				code := send(t, g, gwidHost, 443, path)
				if code != http.StatusOK || g.gw.count() != i+1 || g.gw.last(t).Header.Get(gatewayIdentityHeader) != g.lastMinted(t) {
					t.Errorf("%s: status %d, %d gateway requests, want it signed", path, code, g.gw.count())
				}
			}
			if strings.Contains(g.buf.String(), gatewayOffRouteFix) {
				t.Errorf("a clean path on a whole-host route was refused: %s", g.buf)
			}
			// A dot segment is still not sent there. The brokered lane refuses it earlier, with its own row.
			code := send(t, g, gwidHost, 443, "/v1/../other")
			named := strings.Contains(g.buf.String(), "outside every route here (provider prov-1, the whole host and port)")
			if code != http.StatusBadRequest || g.gw.count() != len(paths) || named != (name != "brokered") {
				t.Errorf("a dot segment on a whole-host route: status %d, %d gateway requests, route named %v", code, g.gw.count(), named)
			}
		})
	}
}

// An MCP server's route keeps its host and port the same way: another path there is refused and the
// refusal names the server. REVERT PROOF: skipping MCP routes in gatewayOffRouteRefused sends both
// paths to the server with no identity.
func TestGatewayIdentity_MCPRouteHostSendsOnlyWhatItCovers(t *testing.T) {
	for name, send := range gwidPathLanes() {
		if name == "brokered" {
			continue // the brokered model route never sends to an MCP server
		}
		t.Run(name, func(t *testing.T) {
			g := newGwidRig(t, []GatewayIdentityRoute{{Host: gwidHost, Port: 443, PathPrefix: "/mcp", MCPKey: "jira", Audience: gwidAudience}}, false)
			dials := gwidGatewayDials(g)
			for _, path := range []string{"/other", "/v1/messages"} {
				if code := send(t, g, gwidHost, 443, path); code != http.StatusBadRequest {
					t.Errorf("%s: status %d, want 400", path, code)
				}
			}
			if dials.Load() != 0 || g.gw.count() != 0 || g.mint.hits.Load() != 0 {
				t.Fatalf("a refused path left: %d dials, %d gateway requests, %d mints", dials.Load(), g.gw.count(), g.mint.hits.Load())
			}
			if rows := strings.Count(g.buf.String(), "outside every route here (MCP server jira, path prefix /mcp)"); rows != 2 {
				t.Errorf("%d decision rows name the MCP server's route, want 2: %s", rows, g.buf)
			}
		})
	}
}

// A prefix that authoring accepts covers its own calls, a ";" or a doubled slash in it included,
// so a saved route is never one whose every call is refused.
func TestGatewayIdentity_RouteHostSignsUnderAnAuthoredPrefix(t *testing.T) {
	for _, prefix := range []string{"/v1;tenant=a", "/a//b", ""} {
		routes := gwidRoutes()
		routes[0].PathPrefix = prefix
		g := newGwidRig(t, routes, false)
		rec := httptest.NewRecorder()
		g.p.servePlain(rec, gwidForge(httptest.NewRequest(http.MethodPost, "https://"+gwidHost+prefix+"/messages", strings.NewReader(`{"hi":1}`))))
		if rec.Code != http.StatusOK || g.gw.count() != 1 || g.gw.last(t).Header.Get(gatewayIdentityHeader) != g.lastMinted(t) {
			t.Errorf("prefix %q: status %d, %d gateway requests, want its call signed", prefix, rec.Code, g.gw.count())
		}
	}
}

// A host rule that holds every call to a route's host never puts a call no route covers to a person: on
// each lane the refusal comes first, since nothing would be sent after the answer. REVERT PROOF: with the
// terminated lane's refusal left in forwardInspectedLLM alone, its host rule hold asks first.
func TestGatewayIdentity_OffRouteRefusedBeforeAHoldAsks(t *testing.T) {
	for name, send := range gwidPathLanes() {
		t.Run(name, func(t *testing.T) {
			g := newGwidRig(t, gwidRoutes(), false)
			g.p.policy = CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{gwidHost, gwidOther},
				HostRules: []types.HostRuleSet{{Rules: []types.HostRule{{Host: gwidHost, Effect: types.HostRuleHold}}}}})
			cp := newHCCP(t, g.p)
			code := send(t, g, gwidHost, 443, "/other")
			if _, raises, _ := cp.counts(); code != http.StatusBadRequest || raises != 0 || g.gw.count() != 0 || !strings.Contains(g.buf.String(), gatewayOffRouteFix) {
				t.Fatalf("status %d, %d holds raised, %d gateway requests, want the refusal and no hold: %s", code, raises, g.gw.count(), g.buf)
			}
			send(t, g, gwidHost, 443, "/v1/messages")
			if _, raises, _ := cp.counts(); raises == 0 {
				t.Error("a covered call raised no hold, so the zero above proves nothing")
			}
		})
	}
}

// The brokered lane refuses a path no route covers before it resolves the provider's credential, so a
// refused call never redeems one or waits on a sign-in for it. REVERT PROOF: with the refusal after the
// resolve (in forwardInspectedLLM, where it was), the off-route call redeems the credential first.
func TestGatewayIdentity_BrokeredOffRouteResolvesNoCredential(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	var redeemed atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redeemed.Add(1)
		_ = json.NewEncoder(w).Encode(types.ResolvedInjection{
			Header: "X-Api-Key", Value: "BROKERED-MODEL-KEY", JTI: "jti", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
		})
	}))
	t.Cleanup(cp.Close)
	// An expired entry: a request that reaches the resolve redeems.
	g.p.inject = &injector{
		byHost: map[string]*injEntry{gwidHost: {grantID: uuid.New(), header: injectedHeader{name: "X-Api-Key", value: "stale"}, expiresAt: time.Now().Add(-time.Hour).UnixMilli()}},
		base:   cp.URL, token: newTokenSource("RUNTOK"), client: cp.Client(),
	}
	send := gwidPathLanes()["brokered"]
	if code := send(t, g, gwidHost, 443, "/other"); code != http.StatusBadRequest || redeemed.Load() != 0 || g.gw.count() != 0 {
		t.Fatalf("status %d, %d redemptions, %d gateway requests; want 400, none and none", code, redeemed.Load(), g.gw.count())
	}
	send(t, g, gwidHost, 443, "/v1/messages")
	if redeemed.Load() == 0 {
		t.Error("a covered call redeemed nothing, so the zero above proves nothing")
	}
}
