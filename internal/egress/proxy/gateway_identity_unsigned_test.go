// Copyright 2026 The Wardyn Authors
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

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/armscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gwidUnsignedText is the part of gatewayUnsignedRefused's answer that names it.
const gwidUnsignedText = "and this kind of call does not carry it. Give the other service its own host and port."

// A git broker never carries the run's identity, so it sends nothing to a host and port an identity
// route has: no credential is resolved and the forge is not reached, whether the route's prefix covers
// the git path or not. The first row is a composition authoring allows: a custom model endpoint under
// /llm/v1 with identity on and a git_pat grant with api off on the same host. A refused call dials
// nothing, so no TLS handshake can offer the run's certificate there. A route on another host changes
// nothing, and the forge never sees the identity header. REVERT PROOF: without
// gatewayUnsignedRefused in handlePATBroker or handleGitBroker, each refused row mints the credential
// and reaches the forge with it and no identity.
func TestGatewayIdentity_GitBrokersSendNothingAtARoutesHost(t *testing.T) {
	const patRefs, ghRefs = "/wardyn/git/forge.example/team/repo/info/refs?service=git-upload-pack", "/wardyn/gh/org/repo.git/info/refs?service=git-upload-pack"
	route := func(host, prefix string) []GatewayIdentityRoute {
		return []GatewayIdentityRoute{{Host: host, Port: 443, PathPrefix: prefix, ProviderID: "custom-1", Audience: gwidAudience}}
	}
	for _, tc := range []struct {
		name, target string
		routes       []GatewayIdentityRoute
		sent         bool
	}{
		{"git_pat, a model route under /llm/v1", patRefs, route("forge.example", "/llm/v1"), false},
		{"git_pat, a whole-host model route", patRefs, route("forge.example", ""), false},
		{"git_pat, a route on another host", patRefs, route("other.example", ""), true},
		{"github, a whole-host route on github.com", ghRefs, route(githubHost, ""), false},
		{"github, a route on another host", ghRefs, route("other.example", ""), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var up *gitBrokerUpstream
			var p *Proxy
			var buf *bytes.Buffer
			if strings.HasPrefix(tc.target, "/wardyn/git/") {
				up = newPATBrokerUpstream(t, "PAT-VALUE", "oauth2")
				p, buf = newPATBrokerProxy(t, map[string]PATGrant{"forge.example": {GrantID: uuid.New(), Forge: types.PATForgeGitLab, Repos: types.PATRepoSet("team/repo")}}, upstreamAddr(up.srv))
			} else {
				up = newGitBrokerUpstream(t, "GH-VALUE")
				p, buf = newGitBrokerProxy(t, map[string]uuid.UUID{"org/repo": uuid.New()}, upstreamAddr(up.srv))
			}
			p.gatewayRoutes = tc.routes
			dials, dial := 0, p.dial
			p.dial = func(ctx context.Context, network, target string) (net.Conn, error) {
				dials++
				return dial(ctx, network, target)
			}
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, tc.target, nil))
			up.mu.Lock()
			defer up.mu.Unlock()
			if tc.sent {
				if rec.Code != http.StatusOK || up.mintCalls != 1 || up.gitHits != 1 || up.gitHeaders.Get(gatewayIdentityHeader) != "" {
					t.Fatalf("status %d, %d mints, %d forge requests, identity %q; want it sent as before, unsigned: %s", rec.Code, up.mintCalls, up.gitHits, up.gitHeaders.Get(gatewayIdentityHeader), rec.Body)
				}
				return
			}
			if rec.Code != http.StatusBadRequest || up.mintCalls != 0 || up.gitHits != 0 || dials != 0 || !strings.Contains(buf.String(), "the call was not sent: run identity is on at this host and port (provider custom-1, ") ||
				!strings.Contains(buf.String(), gwidUnsignedText) {
				t.Fatalf("status %d, %d mints, %d upstream requests, %d dials, want 400, none, none, none and a row naming the route: %s",
					rec.Code, up.mintCalls, up.gitHits, dials, buf)
			}
		})
	}
}

// An API door never carries the run's identity either, so it sends nothing to a host and port an
// identity route has, an MCP server's route as much as a model provider's: no credential is minted or
// redeemed and the service is not reached, inside the route's prefix or outside it. Each door is
// sent a request it admits without the route. REVERT PROOF: without the door branch in
// gatewayOffRouteRefusedInTunnel, or without a door's own term in unsignedDoorHost, that door mints
// or redeems its credential and reaches the service with no identity on the whole-host route, and
// is answered with the off-route text on the other.
func TestGatewayIdentity_APIDoorSendsNothingAtARoutesHost(t *testing.T) {
	type outcome struct {
		rec                   *httptest.ResponseRecorder
		credentials, upstream int
	}
	route := func(host string, port int, prefix string) []GatewayIdentityRoute {
		return []GatewayIdentityRoute{{Host: host, Port: port, PathPrefix: prefix, MCPKey: "svc", Audience: gwidAudience}}
	}
	for name, send := range map[string]func(t *testing.T, prefix string) outcome{
		"git_pat": func(t *testing.T, prefix string) outcome {
			h := newAPIHarness(t, types.PATForgeGitLab, PATGrant{Repos: types.PATRepoSet("team/app")})
			h.p.gatewayRoutes = route(apiHost, 443, prefix)
			rec := h.serve(t, apiReq{method: "GET", target: glBase + "/issues"})
			return outcome{rec, h.mints(), h.forgeHits()}
		},
		"github": func(t *testing.T, prefix string) outcome {
			up := newGHUpstream(t)
			p, _ := newGHProxy(t, up)
			p.gatewayRoutes = route(ghscope.APIHost, 443, prefix)
			rec := httptest.NewRecorder()
			p.serveMITMRequest(rec, ghTunnelRequest(t, http.MethodGet, "/repos/acme/app/issues"), ghscope.APIHost, 443)
			mints, seen := up.counts()
			return outcome{rec, mints, len(seen)}
		},
		"azure": func(t *testing.T, prefix string) outcome {
			h := newARMHarness(t, false)
			h.p.gatewayRoutes = route(armscope.Host, 443, prefix)
			rec := h.tunnel(t, http.MethodGet, armGroups, armscope.Host)
			return outcome{rec, int(h.redeemed.Load()), len(h.upstreamSaw())}
		},
		"aws": func(t *testing.T, prefix string) outcome {
			g := newSGWHarness(t)
			g.p.gatewayRoutes = append(g.p.gatewayRoutes, route(sgwControl, 443, prefix)...)
			rec := g.send(t, sgwRequest(t, sgwControl, "/foundation-models", "", nil), sgwControl)
			return outcome{rec, int(g.redeemed.Load()), len(g.arrivals())}
		},
		"kubernetes": func(t *testing.T, prefix string) outcome {
			h := newKubeHarness(t, kubeOpts{})
			h.p.gatewayRoutes = route(kubeHost, kubePort, prefix)
			rec := h.tunnel(t, http.MethodGet, "/api/v1/namespaces/team-a/pods")
			return outcome{rec, int(h.minted.Load()), len(h.upstreamSaw())}
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, prefix := range []string{"", "/mcp"} {
				o := send(t, prefix)
				if body := o.rec.Body.String(); o.rec.Code != http.StatusBadRequest || o.credentials != 0 || o.upstream != 0 ||
					!strings.Contains(body, "(MCP server svc, ") || !strings.Contains(body, gwidUnsignedText) {
					t.Errorf("prefix %q: status %d, %d credentials, %d upstream requests, want 400, none, none and the route named: %s",
						prefix, o.rec.Code, o.credentials, o.upstream, body)
				}
			}
		})
	}
}

// The push comparison's own read of the forge carries the forge's credential and never the run's
// identity, so it is not sent to a host and port an identity route has. REVERT PROOF: without the
// check in forgeRepo.read the read is authorized and sent.
func TestGatewayIdentity_ForgeReadSendsNothingAtARoutesHost(t *testing.T) {
	up := newGitBrokerUpstream(t, "GH-VALUE")
	p, _ := newGitBrokerProxy(t, map[string]uuid.UUID{"org/repo": uuid.New()}, upstreamAddr(up.srv))
	p.gatewayRoutes = []GatewayIdentityRoute{{Host: githubAPIHost, Port: 443, ProviderID: "custom-1", Audience: gwidAudience}}
	authorized := 0
	var v map[string]any
	_, _, err := (&forgeRepo{p: p, repo: "org/repo"}).read(context.Background(), githubAPIHost, "/repos/org/repo", func(context.Context, *http.Request) error {
		authorized++
		return nil
	}, &v)
	up.mu.Lock()
	defer up.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), gwidUnsignedText) || authorized != 0 || up.gitHits != 0 {
		t.Fatalf("err %v, %d authorizations, %d forge requests; want the refusal and neither", err, authorized, up.gitHits)
	}
}

// The token hand-back at shutdown carries the token and never the run's identity, so it is not sent to
// a host and port an identity route has. The token is minted before the route is set: the git broker
// shares this cache, and its github.com host may have no route. REVERT PROOF: without the check in
// revokeGitHubToken the DELETE reaches the route's host with the token.
func TestGatewayIdentity_TokenHandBackSendsNothingAtARoutesHost(t *testing.T) {
	up := newGHUpstream(t)
	p, _ := newGHProxy(t, up)
	p.serveMITMRequest(httptest.NewRecorder(), ghTunnelRequest(t, http.MethodGet, "/repos/acme/app/issues"), ghscope.APIHost, 443)
	p.gatewayRoutes = []GatewayIdentityRoute{{Host: ghscope.APIHost, Port: 443, ProviderID: "custom-1", Audience: gwidAudience}}
	p.revokeGitHubAPITokens(t.Context())
	if mints, seen := up.counts(); mints != 1 || len(seen) != 1 {
		t.Fatalf("%d mints, seen %q; want the one request and no hand-back", mints, seen)
	}
}

// The git brokers refuse before anything else can answer or hold a push: a push this run's walls
// withhold gets the identity refusal, not the capability one, and nothing is minted. REVERT PROOF:
// with either check moved to just before the mint, after the push rules, that broker's push gets
// the 403 the capability check writes.
func TestGatewayIdentity_GitBrokersRefuseBeforeThePushRules(t *testing.T) {
	for name, target := range map[string]string{
		"git_pat": "/wardyn/git/forge.example/team/repo/git-receive-pack",
		"github":  "/wardyn/gh/org/repo.git/git-receive-pack",
	} {
		t.Run(name, func(t *testing.T) {
			var up *gitBrokerUpstream
			var p *Proxy
			host := githubHost
			if name == "git_pat" {
				host = "forge.example"
				up = newPATBrokerUpstream(t, "PAT-VALUE", "oauth2")
				p, _ = newPATBrokerProxy(t, map[string]PATGrant{host: {GrantID: uuid.New(), Forge: types.PATForgeGitLab, Repos: types.PATRepoSet("team/repo")}}, upstreamAddr(up.srv))
			} else {
				up = newGitBrokerUpstream(t, "GH-VALUE")
				p, _ = newGitBrokerProxy(t, map[string]uuid.UUID{"org/repo": uuid.New()}, upstreamAddr(up.srv))
			}
			p.gatewayRoutes = []GatewayIdentityRoute{{Host: host, Port: 443, ProviderID: "custom-1", Audience: gwidAudience}}
			p.gitPushWithheld = map[string]string{host: "this forge"}
			rec := httptest.NewRecorder()
			p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, target, strings.NewReader("0000")))
			up.mu.Lock()
			defer up.mu.Unlock()
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), gwidUnsignedText) || up.mintCalls != 0 || up.gitHits != 0 {
				t.Fatalf("status %d, %d mints, %d forge requests; want the identity refusal first: %s", rec.Code, up.mintCalls, up.gitHits, rec.Body)
			}
		})
	}
}
