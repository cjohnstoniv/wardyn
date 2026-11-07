// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/gatewayidentity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func testGatewayMCPComponent() types.Component {
	return types.Component{
		ID: uuid.MustParse("00000000-0000-0000-0000-0000000000a1"), Name: "Tools", Version: 3,
		Definition: types.ComponentDefinition{
			Hosts: []string{"mcp.corp.example"},
			MCP: &types.ComponentMCP{
				URL: "https://mcp.corp.example/tools", Transport: types.ComponentMCPTransportHTTP, GatewayAudience: "mcp-aud",
			},
		},
	}
}

// sealMCP stores the component, the run's snapshot of it and the sealed route dispatch would write,
// next to the provider's.
func (f *gatewayFixture) sealMCP(t *testing.T, c types.Component) {
	t.Helper()
	prov, _ := gatewayRouteOf(testGatewayProvider())
	rt, ok := gatewayMCPRouteOf(c.Definition.MCP.URL, "tools", c.Definition.MCP.GatewayAudience, gatewayMCPUID(c.ID, c.Version))
	if !ok {
		t.Fatalf("no route for %+v", c.Definition.MCP)
	}
	raw, err := json.Marshal(proxy.Config{GatewayIdentity: []proxy.GatewayIdentityRoute{prov, rt}})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.storeRunProxyConfig(context.Background(), f.run.ID, raw); err != nil {
		t.Fatal(err)
	}
	id := c.ID
	f.st.set(func(s *gatewayStore) {
		s.comps = map[uuid.UUID]types.Component{c.ID: c}
		s.runComps = []types.RunComponent{{RunID: f.run.ID, ComponentID: &id, Name: c.Name, Version: c.Version, Definition: c.Definition}}
	})
}

func (f *gatewayFixture) mintMCP(t *testing.T, key string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, f.srv, http.MethodPost, gatewayMintPath, f.token, `{"mcp_server":"`+key+`"}`)
}

// An MCP server's mint signs for the sealed audience and names the server, not a provider, and the
// first one is audited as the server's.
func TestGatewayMCPMintNamesTheServer(t *testing.T) {
	f := newGatewayFixture(t)
	f.sealMCP(t, testGatewayMCPComponent())
	var token string
	for range 2 {
		w := f.mintMCP(t, "tools")
		if w.Code != http.StatusOK {
			t.Fatalf("mint = %d %s", w.Code, w.Body)
		}
		var body struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		token = body.Token
	}
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	var c gatewayidentity.Claims
	if err := parsed.UnsafeClaimsWithoutVerification(&c); err != nil {
		t.Fatal(err)
	}
	if c.Audience != "mcp-aud" || c.MCP != "tools" || c.Provider != "" || c.Subject != "alice@example.com" || c.Run != f.run.ID.String() {
		t.Errorf("claims = %+v", c)
	}
	if got := c.ExpiresAt - c.IssuedAt; got != 600 {
		t.Errorf("life = %d s, want the 600 s cap", got)
	}
	evs := f.mintEvents()
	if len(evs) != 1 || evs[0].Outcome != "success" || evs[0].Target != "tools" || !strings.Contains(string(evs[0].Data), `"mcp_server":"tools"`) {
		t.Fatalf("rows = %+v, want one success on the server", evs)
	}
	// The provider's mint still stands beside it, with its own first-mint row.
	if w := f.mint(t); w.Code != http.StatusOK {
		t.Fatalf("provider mint = %d %s", w.Code, w.Body)
	}
	if evs := f.mintEvents(); len(evs) != 2 {
		t.Errorf("rows = %d, want the provider's first mint recorded too", len(evs))
	}
}

// Whatever moved since dispatch refuses the next mint, and editing it back lets it through.
func TestGatewayMCPMintRefusesAServerThatChanged(t *testing.T) {
	edits := map[string]func(*types.Component, *gatewayStore){
		"audience": func(c *types.Component, _ *gatewayStore) { c.Definition.MCP.GatewayAudience = "new-aud" },
		"url":      func(c *types.Component, _ *gatewayStore) { c.Definition.MCP.URL = "https://mcp.corp.example/other" },
		"port": func(c *types.Component, _ *gatewayStore) {
			c.Definition.MCP.URL = "https://mcp.corp.example:8443/tools"
		},
		"version": func(c *types.Component, _ *gatewayStore) { c.Version++ },
		"identity off": func(c *types.Component, _ *gatewayStore) {
			c.Definition.MCP.GatewayAudience = ""
		},
		"made personal": func(c *types.Component, _ *gatewayStore) { c.Owner = "alice@example.com" },
		"made local": func(c *types.Component, _ *gatewayStore) {
			c.Definition.MCP = &types.ComponentMCP{Transport: types.ComponentMCPTransportStdio, Launch: &types.ComponentMCPLaunch{Command: "npx"}}
		},
		"deleted":        func(c *types.Component, s *gatewayStore) { s.comps = map[uuid.UUID]types.Component{} },
		"run lost it":    func(_ *types.Component, s *gatewayStore) { s.runComps = nil },
		"run's snapshot": func(_ *types.Component, s *gatewayStore) { s.runComps[0].Version-- },
	}
	for name, edit := range edits {
		t.Run(name, func(t *testing.T) {
			f := newGatewayFixture(t)
			orig := testGatewayMCPComponent()
			f.sealMCP(t, orig)
			live := testGatewayMCPComponent()
			f.st.set(func(s *gatewayStore) {
				edit(&live, s)
				if _, kept := s.comps[orig.ID]; kept {
					s.comps[orig.ID] = live
				}
			})
			w := f.mintMCP(t, "tools")
			if w.Code != http.StatusForbidden || reasonOf(t, w) != reasonGatewayIdentityChanged || !strings.Contains(w.Body.String(), "Create a new run") {
				t.Fatalf("mint = %d %s", w.Code, w.Body)
			}
			f.sealMCP(t, orig)
			if w := f.mintMCP(t, "tools"); w.Code != http.StatusOK {
				t.Fatalf("edited back: mint = %d %s", w.Code, w.Body)
			}
		})
	}
	t.Run("store down", func(t *testing.T) {
		f := newGatewayFixture(t)
		f.sealMCP(t, testGatewayMCPComponent())
		f.st.set(func(s *gatewayStore) { s.compErr = errors.New("db down") })
		if w := f.mintMCP(t, "tools"); w.Code != http.StatusServiceUnavailable || reasonOf(t, w) != reasonGatewayIdentityUnavailable {
			t.Fatalf("mint = %d %s", w.Code, w.Body)
		}
	})
}

// The body names a server the run's sealed routes do not hold, or both kinds at once, or a provider's
// id in the server field: refused, whatever the live rows say.
func TestGatewayMCPMintRefusesWhatWasNeverSealed(t *testing.T) {
	f := newGatewayFixture(t)
	f.sealMCP(t, testGatewayMCPComponent())
	for name, body := range map[string]string{
		"another server":     `{"mcp_server":"other"}`,
		"provider as MCP":    `{"mcp_server":"corp-gw"}`,
		"both":               `{"provider_id":"corp-gw","mcp_server":"tools"}`,
		"neither":            `{}`,
		"server as provider": `{"provider_id":"tools"}`,
	} {
		w := do(t, f.srv, http.MethodPost, gatewayMintPath, f.token, body)
		if w.Code != http.StatusForbidden || reasonOf(t, w) != reasonGatewayIdentityChanged {
			t.Errorf("%s: mint = %d %s", name, w.Code, w.Body)
		}
	}
}

// A revive checks an MCP route as a mint does.
func TestGatewayMCPReviveAfterAnEdit(t *testing.T) {
	f := newGatewayFixture(t)
	c := testGatewayMCPComponent()
	f.sealMCP(t, c)
	rt, _ := gatewayMCPRouteOf(c.Definition.MCP.URL, "tools", "mcp-aud", gatewayMCPUID(c.ID, c.Version))
	cfg := &proxy.Config{RunID: f.run.ID, GatewayIdentity: []proxy.GatewayIdentityRoute{rt}}
	if ref, err := f.srv.gatewayIdentityRefusal(context.Background(), cfg); ref != nil || err != nil {
		t.Fatalf("unchanged: %v %v", ref, err)
	}
	moved := c
	moved.Version++
	f.st.set(func(s *gatewayStore) { s.comps[c.ID] = moved })
	ref, err := f.srv.gatewayIdentityRefusal(context.Background(), cfg)
	if err != nil || ref == nil || ref.status != http.StatusConflict || ref.reason != reasonGatewayIdentityChanged ||
		!strings.Contains(ref.msg, `"tools"`) || !strings.Contains(ref.msg, "Create a new run") {
		t.Fatalf("got %+v %v, want a refusal naming the server", ref, err)
	}
	f.st.set(func(s *gatewayStore) { s.compErr = errors.New("db down") })
	if _, err := f.srv.gatewayIdentityRefusal(context.Background(), cfg); err == nil {
		t.Error("a failed read passed")
	}
}

// One audience is never on two destinations, across providers and organisation MCP servers: a model
// route's token must not be good on an MCP route, or the reverse. Respellings of one address share.
func TestGatewayAudiencesCoverMCPServers(t *testing.T) {
	prov := &types.ModelProviders{Providers: []types.ModelProvider{testGatewayProvider()}}
	srv := testGatewayMCPComponent()
	srv.Definition.MCP.GatewayAudience = "gw-aud" // the provider's, at another address
	err := validateGatewayAudiences(prov, []types.Component{srv})
	if err == nil || refusalField(err) != "mcp.gateway_audience" || !strings.Contains(err.Error(), `provider "corp-gw"`) || !strings.Contains(err.Error(), `MCP server "Tools"`) {
		t.Fatalf("a provider's audience on an MCP server: %v", err)
	}
	if err := validateGatewayAudiences(prov, []types.Component{testGatewayMCPComponent()}); err != nil {
		t.Errorf("audiences apart refused: %v", err)
	}
	same := testGatewayMCPComponent()
	same.Definition.MCP.GatewayAudience = "mcp-aud"
	other := testGatewayMCPComponent()
	other.ID, other.Name = uuid.New(), "Tools again"
	other.Definition.MCP.URL = "https://MCP.corp.example.:443/tools/"
	if err := validateGatewayAudiences(nil, []types.Component{same, other}); err != nil {
		t.Errorf("one destination, two spellings, refused: %v", err)
	}
	other.Definition.MCP.URL = "https://mcp.corp.example/elsewhere"
	if err := validateGatewayAudiences(nil, []types.Component{same, other}); err == nil {
		t.Error("two MCP servers at two addresses on one audience accepted")
	}
	if got := gatewayMCPAudienceUses([]types.Component{same, {Owner: "bob", Definition: same.Definition}, {Name: "none"}}); len(got) != 1 {
		t.Errorf("uses = %d, want only the organisation's server with an audience", len(got))
	}
}

// Dispatch seals one MCP route for an organisation's server with an audience, keyed by the server and
// bound to the component's version; a person's own server, or one with none, seals nothing.
func TestGatewayMCPDispatchRoutes(t *testing.T) {
	id := uuid.MustParse("00000000-0000-0000-0000-0000000000b2")
	def := testGatewayMCPComponent().Definition
	snap := func(self bool, owner string) types.RunComponent {
		return types.RunComponent{ComponentID: &id, SelfDefined: self, Owner: owner, Name: "Tools", Version: 4, Definition: def}
	}
	d := runComponents{attached: []attachedComponent{{snapshot: snap(false, "")}}}.dispatch()
	if len(d.MCP) != 1 || d.MCP[0].GatewayAudience != "mcp-aud" || d.MCP[0].GatewayUID != id.String()+"/v4" {
		t.Fatalf("org server: %+v", d.MCP)
	}
	for name, sn := range map[string]types.RunComponent{"personal": snap(true, "alice@example.com"), "inline": snap(true, "")} {
		if d := (runComponents{attached: []attachedComponent{{snapshot: sn}}}).dispatch(); len(d.MCP) != 1 || d.MCP[0].GatewayAudience != "" {
			t.Errorf("%s: %+v, want no audience", name, d.MCP)
		}
	}

	servers := []componentMCP{d.MCP[0]}
	servers[0].Harnesses = []string{"claude-code"}
	out := dispatchMCP(servers, withholding{}, &[]runner.InjectionGrant{}, "http://p:3128", false, map[string]string{})
	want := proxy.GatewayIdentityRoute{
		Host: "mcp.corp.example", Port: 443, PathPrefix: "/tools", MCPKey: "tools", Audience: "mcp-aud", ProviderUID: id.String() + "/v4",
	}
	if len(out.gatewayRoutes) != 1 || out.gatewayRoutes[0] != want || len(out.gatewayBad) != 0 {
		t.Fatalf("routes %+v bad %v, want %+v", out.gatewayRoutes, out.gatewayBad, want)
	}
	// A server dropped for a withheld host seals no route; one with an address no route can hold is named.
	if out := dispatchMCP(servers, withholding{deny: []string{"mcp.corp.example"}}, &[]runner.InjectionGrant{}, "http://p:3128", false, map[string]string{}); len(out.gatewayRoutes) != 0 || len(out.gatewayBad) != 0 {
		t.Errorf("withheld: %+v %v", out.gatewayRoutes, out.gatewayBad)
	}
	servers[0].URL = "https://mcp.corp.example/a/../tools"
	bad := dispatchMCP(servers, withholding{}, &[]runner.InjectionGrant{}, "http://p:3128", false, map[string]string{})
	if len(bad.gatewayRoutes) != 0 || !slices.Equal(bad.gatewayBad, []string{"tools"}) {
		t.Errorf("dot segment: %+v %v", bad.gatewayRoutes, bad.gatewayBad)
	}
	if routes, msg := gatewayMCPRoutesOf(nil, bad); routes != nil || !strings.Contains(msg, `"tools"`) {
		t.Errorf("a route that cannot be written: %v %q, want a refusal", routes, msg)
	}
}

// The routes a run is sealed with are the providers' and the servers', and a server on a provider's
// gateway path, or another server's, refuses the run.
func TestGatewayMCPRoutesOfRefusesAClash(t *testing.T) {
	prov, _ := gatewayRouteOf(testGatewayProvider()) // gw.corp.example:443/llm
	apart := proxy.GatewayIdentityRoute{Host: "mcp.corp.example", Port: 443, PathPrefix: "/tools", MCPKey: "tools", Audience: "mcp-aud", ProviderUID: "x/v1"}
	routes, msg := gatewayMCPRoutesOf([]proxy.GatewayIdentityRoute{prov}, mcpDispatch{gatewayRoutes: []proxy.GatewayIdentityRoute{apart}})
	if msg != "" || len(routes) != 2 {
		t.Fatalf("apart: %v %q", routes, msg)
	}
	onProvider := apart
	onProvider.Host, onProvider.PathPrefix = "gw.corp.example", "/llm/mcp"
	if routes, msg := gatewayMCPRoutesOf([]proxy.GatewayIdentityRoute{prov}, mcpDispatch{gatewayRoutes: []proxy.GatewayIdentityRoute{onProvider}}); routes != nil ||
		!strings.Contains(msg, `"tools"`) || !strings.Contains(msg, `"corp-gw"`) {
		t.Errorf("on a provider's path: %v %q", routes, msg)
	}
	second := apart
	second.MCPKey = "tools-2"
	if routes, msg := gatewayMCPRoutesOf(nil, mcpDispatch{gatewayRoutes: []proxy.GatewayIdentityRoute{apart, second}}); routes != nil || !strings.Contains(msg, `"tools-2"`) {
		t.Errorf("two servers on one path: %v %q", routes, msg)
	}
	if routes, msg := gatewayMCPRoutesOf([]proxy.GatewayIdentityRoute{prov}, mcpDispatch{}); msg != "" || len(routes) != 1 {
		t.Errorf("no servers: %v %q", routes, msg)
	}
}

// Two saves at once can leave one audience on a provider and a server, or on two servers, at different
// addresses; the run is refused rather than sent a token one gateway would take for the other.
func TestGatewayMCPRoutesOfRefusesAnAudienceTwice(t *testing.T) {
	prov, _ := gatewayRouteOf(testGatewayProvider()) // gw-aud at gw.corp.example:443/llm
	tools := proxy.GatewayIdentityRoute{Host: "mcp.corp.example", Port: 443, PathPrefix: "/tools", MCPKey: "tools", Audience: "gw-aud", ProviderUID: "x/v1"}
	if routes, msg := gatewayMCPRoutesOf([]proxy.GatewayIdentityRoute{prov}, mcpDispatch{gatewayRoutes: []proxy.GatewayIdentityRoute{tools}}); routes != nil ||
		!strings.Contains(msg, `"tools"`) || !strings.Contains(msg, `model provider "corp-gw"`) || !strings.Contains(msg, "own audience") {
		t.Errorf("a provider's audience: %v %q", routes, msg)
	}
	tools.Audience = "mcp-aud"
	docs := proxy.GatewayIdentityRoute{Host: "docs.corp.example", Port: 443, MCPKey: "docs", Audience: "mcp-aud", ProviderUID: "y/v1"}
	if routes, msg := gatewayMCPRoutesOf(nil, mcpDispatch{gatewayRoutes: []proxy.GatewayIdentityRoute{tools, docs}}); routes != nil ||
		!strings.Contains(msg, `"docs"`) || !strings.Contains(msg, `MCP server "tools"`) {
		t.Errorf("another server's audience: %v %q", routes, msg)
	}
	docs.Audience = "docs-aud"
	if routes, msg := gatewayMCPRoutesOf([]proxy.GatewayIdentityRoute{prov}, mcpDispatch{gatewayRoutes: []proxy.GatewayIdentityRoute{tools, docs}}); msg != "" || len(routes) != 3 {
		t.Errorf("audiences apart: %v %q", routes, msg)
	}
}

// A revive that drops an MCP server for a withheld host drops its identity route with it.
func TestGatewayMCPReviveCeilingDropsTheRoute(t *testing.T) {
	run := types.AgentRun{ID: uuid.New()}
	prov, _ := gatewayRouteOf(testGatewayProvider())
	rt := proxy.GatewayIdentityRoute{Host: "mcp.corp.example", Port: 443, PathPrefix: "/tools", MCPKey: "tools", Audience: "mcp-aud", ProviderUID: "x/v1"}
	cfg := &proxy.Config{RunID: run.ID,
		MCPServers:      []proxy.MCPServerConfig{{Key: "tools", URL: "https://mcp.corp.example/tools"}},
		MCPBindings:     []proxy.MCPBinding{{Harness: "claude-code", Servers: []string{"tools"}}},
		GatewayIdentity: []proxy.GatewayIdentityRoute{prov, rt},
	}
	if _, rerr := reassertProxyCeiling(run, cfg, ownerCeiling{deny: []string{"mcp.corp.example"}}, nil); rerr != nil {
		t.Fatalf("refused: %+v", rerr)
	}
	if len(cfg.GatewayIdentity) != 1 || cfg.GatewayIdentity[0].ProviderID != "corp-gw" {
		t.Errorf("routes %+v: the dropped server kept its route, or the provider's went", cfg.GatewayIdentity)
	}
}
