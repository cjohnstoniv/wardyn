// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMCPRelayNoCredentialNoVetTarget: a call to a server in its own sandbox is sent to the relay
// address with the relay path and nothing of the sandbox's credentials, no host policy is asked and
// no name is resolved, even on a run whose allowlist is empty and whose resolver resolves nothing.
func TestMCPRelayNoCredentialNoVetTarget(t *testing.T) {
	res := &mcpResolver{answer: func(string, int) []net.IP { return nil }}
	f := newMCPFixture(t, types.RunPolicySpec{FirstUseApproval: types.FirstUseAlwaysDeny, DeniedDomains: []string{"10.77.0.5"}}, res, nil)
	rec := f.post(t, mcpToken("claude-code"), "docs", mcpCall(1, "search"),
		"Authorization", "Bearer sandbox-secret", "X-Api-Key", "sandbox-key", "Proxy-Authorization", "Basic c2VjcmV0")
	if rec.Code != http.StatusOK || f.relay.hitCount() != 1 {
		t.Fatalf("answer %d %q, relay hits %d; want the server's answer", rec.Code, rec.Body.String(), f.relay.hitCount())
	}
	if f.relay.path != routeMCPRelay+mcpRelayToken {
		t.Fatalf("relay path %q, want the relay route with the token", f.relay.path)
	}
	for _, h := range []string{"Authorization", "X-Api-Key", "Proxy-Authorization"} {
		if v := f.relay.hdr.Get(h); v != "" {
			t.Fatalf("relay got %s %q, want no credential", h, v)
		}
	}
	if got := f.dialled(); !slices.Equal(got, []string{mcpRelayAddr}) {
		t.Fatalf("dialled %v, want the relay address alone", got)
	}
	if len(res.calls) != 0 || f.up.hitCount() != 0 {
		t.Fatalf("resolver calls %v, credential or server hits %d; want none", res.calls, f.up.hitCount())
	}
	d := wantDecision(t, f.buf, egress.Allow, ruleSourceMCP)
	if d.Request.Host != "10.77.0.5" || d.Request.Port != 3130 || d.Request.Path != "/wardyn/mcp/<redacted:claude-code>/docs" || d.Cause != `tool "search"` {
		t.Fatalf("row %+v, want the relay address, the redacted path and the tool", d.Request)
	}
	if d.GatewayCallID == "" || d.GatewayCallID != f.relay.callID {
		t.Fatalf("row call id %q, relay got %q; want one id that joins both", d.GatewayCallID, f.relay.callID)
	}
	if strings.Contains(f.buf.String(), mcpRelayToken) {
		t.Fatal("a decision row carries the relay token")
	}
}

// TestMCPRelayRefusesHostName: a relay server's address is a literal private IP and port, never a
// host name, with no URL, no grant and a token of 43 or more base64url characters.
func TestMCPRelayRefusesHostName(t *testing.T) {
	grant := uuid.New()
	for _, c := range []struct {
		name  string
		s     MCPServerConfig
		wants string
	}{
		{"host name", MCPServerConfig{Key: "docs", Relay: &MCPRelayTarget{Addr: "wardyn-mcp-proxy:3130", Token: mcpRelayToken}}, "literal private IP"},
		{"public IP", MCPServerConfig{Key: "docs", Relay: &MCPRelayTarget{Addr: "93.184.216.34:3130", Token: mcpRelayToken}}, "literal private IP"},
		{"loopback", MCPServerConfig{Key: "docs", Relay: &MCPRelayTarget{Addr: "127.0.0.1:3130", Token: mcpRelayToken}}, "literal private IP"},
		{"no port", MCPServerConfig{Key: "docs", Relay: &MCPRelayTarget{Addr: "10.77.0.5", Token: mcpRelayToken}}, "literal private IP"},
		{"port zero", MCPServerConfig{Key: "docs", Relay: &MCPRelayTarget{Addr: "10.77.0.5:0", Token: mcpRelayToken}}, "literal private IP"},
		{"url too", MCPServerConfig{Key: "docs", URL: "https://mcp.docs.test/mcp", Relay: &MCPRelayTarget{Addr: mcpRelayAddr, Token: mcpRelayToken}}, "no url"},
		{"grant", MCPServerConfig{Key: "docs", GrantID: &grant, Relay: &MCPRelayTarget{Addr: mcpRelayAddr, Token: mcpRelayToken}}, "no url and no grant_id"},
		{"short token", MCPServerConfig{Key: "docs", Relay: &MCPRelayTarget{Addr: mcpRelayAddr, Token: "short"}}, "relay.token"},
		{"token not base64url", MCPServerConfig{Key: "docs", Relay: &MCPRelayTarget{Addr: mcpRelayAddr, Token: strings.Repeat("a", 42) + "+="}}, "relay.token"},
		{"neither url nor relay", MCPServerConfig{Key: "docs"}, "mcp_servers[0].url"},
	} {
		cfg := &Config{MCPServers: []MCPServerConfig{c.s}}
		if err := cfg.validateMCP(); err == nil || !strings.Contains(err.Error(), c.wants) {
			t.Errorf("%s: err %v, want it to name %q", c.name, err, c.wants)
		}
	}
	for _, addr := range []string{mcpRelayAddr, "172.18.0.3:3130", "192.168.5.2:3130", "[fd00::5]:3130"} {
		cfg := &Config{MCPServers: []MCPServerConfig{{Key: "docs", Relay: &MCPRelayTarget{Addr: addr, Token: mcpRelayToken}}}}
		if err := cfg.validateMCP(); err != nil {
			t.Errorf("relay at %s refused: %v", addr, err)
		}
	}
}

// TestMCPRelayDownAnswersEachMessage: when the server's MCP proxy cannot be reached, each request
// of the body gets JSON-RPC -32002 and the plain sentence, the row says relay-down, and neither the
// row nor the log carries the relay token.
func TestMCPRelayDownAnswersEachMessage(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
	f.relay.srv.Close()
	var logs lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	rec := f.post(t, mcpToken("claude-code"), "docs", "["+mcpCall(1, "search")+`,{"jsonrpc":"2.0","id":2,"method":"tools/list"}]`)
	var answers []struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &answers); err != nil || len(answers) != 2 {
		t.Fatalf("answer %d %q, want two JSON-RPC errors", rec.Code, rec.Body.String())
	}
	for _, a := range answers {
		if a.Error.Code != mcpRelayDownCode || a.Error.Message != "This MCP server's sandbox is not running" {
			t.Fatalf("answer %+v, want -32002 and the sentence", a)
		}
	}
	wantDecision(t, f.buf, egress.Deny, ruleSourceMCPRelayDown)
	if strings.Contains(f.buf.String()+logs.String(), mcpRelayToken) {
		t.Fatalf("the relay token is in a row or the log: %s", logs.String())
	}
	if rec := f.serveMethod(t, http.MethodGet, "docs"); rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), mcpRelayDownMessage) {
		t.Fatalf("stream open: %d %q, want 502 and the sentence", rec.Code, rec.Body.String())
	}
}

// TestMCPRelayRefusedTokenRecorded: an MCP proxy that refuses the relay token is recorded as
// relay-refused and its answer is not passed on.
func TestMCPRelayRefusedTokenRecorded(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
	f.relay.status = http.StatusForbidden
	rec := f.post(t, mcpToken("claude-code"), "docs", mcpCall(1, "search"))
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "result") {
		t.Fatalf("answer %d %q, want 502 and not the refusal's body", rec.Code, rec.Body.String())
	}
	wantDecision(t, f.buf, egress.Deny, ruleSourceMCPRelayRefused)
}

// TestMCPRelayHeaders: the proxy decides the call from the body and writes Mcp-Method and Mcp-Name
// itself; a harness header that names another call is refused and nothing is sent.
func TestMCPRelayHeaders(t *testing.T) {
	batch := "[" + mcpCall(1, "search") + "," + mcpCall(2, "fetch") + "]"
	mixed := "[" + mcpCall(1, "search") + "," + `{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "]"
	for _, c := range []struct {
		name, body       string
		hdr              []string
		refused          bool
		wantMeth, wantNm string
	}{
		{"absent: the body decides", mcpCall(1, "search"), nil, false, "tools/call", "search"},
		{"agreeing headers kept", mcpCall(1, "search"), []string{"Mcp-Method", "tools/call", "Mcp-Name", "search"}, false, "tools/call", "search"},
		{"other method", mcpCall(1, "search"), []string{"Mcp-Method", "tools/list"}, true, "", ""},
		{"other tool", mcpCall(1, "search"), []string{"Mcp-Name", "delete_all"}, true, "", ""},
		{"a name on a batch", batch, []string{"Mcp-Name", "search"}, true, "", ""},
		{"batch: method only", batch, nil, false, "tools/call", ""},
		{"mixed batch: neither", mixed, nil, false, "", ""},
		{"not a call: method only", mcpInit, nil, false, "initialize", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
			rec := f.post(t, mcpToken("claude-code"), "docs", c.body, c.hdr...)
			if c.refused {
				if rec.Code != http.StatusBadRequest || f.relay.hitCount() != 0 {
					t.Fatalf("answer %d, relay hits %d; want 400 and nothing sent", rec.Code, f.relay.hitCount())
				}
				wantDecision(t, f.buf, egress.Deny, ruleSourceMCPHeaderMismatch)
				return
			}
			if rec.Code != http.StatusOK || f.relay.hitCount() != 1 {
				t.Fatalf("answer %d %q, relay hits %d", rec.Code, rec.Body.String(), f.relay.hitCount())
			}
			if got := f.relay.hdr.Get(mcpMethodHeader); got != c.wantMeth {
				t.Fatalf("Mcp-Method %q, want %q", got, c.wantMeth)
			}
			if got, ok := f.relay.hdr[mcpNameHeader]; (c.wantNm == "" && ok) || (c.wantNm != "" && (len(got) != 1 || got[0] != c.wantNm)) {
				t.Fatalf("Mcp-Name %v, want %q", got, c.wantNm)
			}
		})
	}
	// A bodiless request names no call: a harness header naming one is refused, and none is written.
	f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
	if rec := f.serveMethod(t, http.MethodGet, "docs", "Mcp-Method", "tools/call"); rec.Code != http.StatusBadRequest {
		t.Fatalf("stream open with a method header: %d, want refused", rec.Code)
	}
	if rec := f.serveMethod(t, http.MethodGet, "docs"); rec.Code != http.StatusOK || f.relay.hdr.Get(mcpMethodHeader) != "" {
		t.Fatalf("stream open: %d, Mcp-Method %q; want sent with no header", rec.Code, f.relay.hdr.Get(mcpMethodHeader))
	}
}

// TestMCPRelayCodeModeCallsCheckedOneByOne: calls a harness's code mode makes in one execution
// arrive as separate requests under identifier names, and each is checked on its own.
func TestMCPRelayCodeModeCallsCheckedOneByOne(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "mcp__docs__delete_page", Effect: types.ToolDeny}}}, nil, nil)
	for i, tool := range []string{"search", "delete-page", "fetch.page"} {
		rec := f.post(t, mcpToken("claude-code"), "docs", mcpCall(i+1, tool))
		blocked := strings.Contains(rec.Body.String(), mcpBlockedMessage)
		if blocked != (tool == "delete-page") {
			t.Fatalf("%s: answer %q, blocked %v", tool, rec.Body.String(), blocked)
		}
	}
	if f.relay.hitCount() != 2 {
		t.Fatalf("relay hits %d, want the two allowed calls", f.relay.hitCount())
	}
}

// serveMethod sends a bodiless broker request for key as claude-code.
func (f *mcpFixture) serveMethod(t *testing.T, method, key string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := mustLocalReq(t, method, "/wardyn/mcp/"+mcpToken("claude-code")+"/"+key, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	f.p.ServeHTTP(rec, req)
	return rec
}
