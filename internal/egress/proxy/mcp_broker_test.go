// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mcpUpstream is one TLS server standing in for the control plane's credential
// resolve and the MCP server, routed by path as the git broker's fixture is.
type mcpUpstream struct {
	srv      *httptest.Server
	mu       sync.Mutex
	hits     int
	auth     string
	smuggled string
	body     []byte
	status   int
	location string
}

const mcpServerSecret = "Bearer mcp-server-secret-value"

func newMCPUpstream(t *testing.T) *mcpUpstream {
	t.Helper()
	u := &mcpUpstream{status: http.StatusOK}
	u.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		defer u.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/api/v1/internal/injection/") {
			_ = json.NewEncoder(w).Encode(types.ResolvedInjection{Header: "Authorization", Value: mcpServerSecret, JTI: "j"})
			return
		}
		u.hits++
		u.auth, u.smuggled = r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")
		u.body, _ = io.ReadAll(r.Body)
		if u.location != "" {
			w.Header().Set("Location", u.location)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.status)
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *mcpUpstream) hitCount() int { u.mu.Lock(); defer u.mu.Unlock(); return u.hits }

// mcpToken is a harness's token in these tests, and mcpBindingFor its digest.
func mcpToken(harness string) string { return "tok-" + harness + "-0123456789abcdef0123456789abcdef" }

func mcpBindingFor(harness string, servers ...string) MCPBinding {
	sum := sha256.Sum256([]byte(mcpToken(harness)))
	return MCPBinding{Harness: harness, SHA256: hex.EncodeToString(sum[:]), Servers: servers}
}

// mcpFixture builds a proxy with two servers: linear (credentialed, on for
// both harnesses) and github (on for claude-code only). dial records every
// target the transport dialled.
type mcpFixture struct {
	p       *Proxy
	buf     *bytes.Buffer
	up      *mcpUpstream
	mu      sync.Mutex
	targets []string
}

func newMCPFixture(t *testing.T, spec types.RunPolicySpec, res resolver, urls map[string]string) *mcpFixture {
	t.Helper()
	f := &mcpFixture{up: newMCPUpstream(t), buf: &bytes.Buffer{}}
	if res == nil {
		res = publicResolver{}
	}
	sink := &decisionSink{out: f.buf, ch: make(chan egress.DecisionLog, 64)}
	addr := upstreamAddr(f.up.srv)
	f.p = newProxy(Options{
		RunID: uuid.New(), Policy: CompilePolicy(spec), Sink: sink, Resolver: res,
		Dial: func(ctx context.Context, network, target string) (net.Conn, error) {
			f.mu.Lock()
			f.targets = append(f.targets, target)
			f.mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		ControlPlaneURL: "https://wardynd.test:8080", RunToken: newTokenSource("RUNTOK"),
		TLSClientConfig: testInsecureTLSConfig, ControlTLS: testInsecureTLSConfig,
	})
	grant := uuid.New()
	servers := []MCPServerConfig{
		{Key: "linear", URL: cmpURL(urls, "linear", "https://mcp.linear.test/mcp"), GrantID: &grant},
		{Key: "github", URL: cmpURL(urls, "github", "https://mcp.github.test/mcp")},
	}
	f.p.mcp = newMCPBroker(servers, []MCPBinding{
		mcpBindingFor("claude-code", "linear", "github"),
		mcpBindingFor("codex-cli", "linear"),
	})
	return f
}

func cmpURL(m map[string]string, k, def string) string {
	if v, ok := m[k]; ok {
		return v
	}
	return def
}

func (f *mcpFixture) post(t *testing.T, harnessToken, key, body string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := mustLocalReq(t, http.MethodPost, "/wardyn/mcp/"+harnessToken+"/"+key, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	f.p.ServeHTTP(rec, req)
	return rec
}

func (f *mcpFixture) dialled() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.targets...)
}

const mcpInit = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`

func mcpCall(id int, tool string) string {
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": map[string]any{}}})
	return string(b)
}

func wantDecision(t *testing.T, buf *bytes.Buffer, decision egress.Decision, source string) egress.DecisionLog {
	t.Helper()
	d := lastDecision(t, buf)
	if d.Decision != decision || d.RuleSource != source {
		t.Fatalf("decision = %s %q, want %s %q", d.Decision, d.RuleSource, decision, source)
	}
	return d
}

// TestMCPBroker_UnboundTokenRefused: a token no harness was given reaches
// nothing, and the server is never dialled.
func TestMCPBroker_UnboundTokenRefused(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
	rec := f.post(t, "tok-guessed", "linear", mcpInit)
	if rec.Code != http.StatusForbidden || f.up.hitCount() != 0 {
		t.Fatalf("status %d, upstream hits %d: want 403 and none", rec.Code, f.up.hitCount())
	}
	wantDecision(t, f.buf, egress.Deny, ruleSourceMCPUnbound)
}

// TestMCPBroker_ServerNotEnabledForHarness: the harness is the token's, never
// what the request claims; a server not turned on for it answers 403.
func TestMCPBroker_ServerNotEnabledForHarness(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
	claim := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"_meta":{"harness":"claude-code"}},"harness":"claude-code"}`
	rec := f.post(t, mcpToken("codex-cli"), "github", claim, "X-Wardyn-Harness", "claude-code")
	if rec.Code != http.StatusForbidden || f.up.hitCount() != 0 {
		t.Fatalf("codex on github: status %d, hits %d; want 403 and none", rec.Code, f.up.hitCount())
	}
	wantDecision(t, f.buf, egress.Deny, ruleSourceMCPNotEnabled)
	// An unknown key is the same refusal.
	if rec := f.post(t, mcpToken("codex-cli"), "nosuch", mcpInit); rec.Code != http.StatusForbidden {
		t.Fatalf("unknown key: status %d, want 403", rec.Code)
	}
	// The harness it is on for reaches it.
	if rec := f.post(t, mcpToken("claude-code"), "github", mcpInit); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
		t.Fatalf("claude-code on github: status %d, hits %d; want 200 and one", rec.Code, f.up.hitCount())
	}
	wantDecision(t, f.buf, egress.Allow, ruleSourceMCP)
}

// TestMCPBroker_ToolsCallDenyIsJSONRPCError: a blocked tools/call is answered
// by the broker as a JSON-RPC error, alone or in a batch, and never sent. The
// rules are the harness's own when the run carries a set per harness, and the
// run-wide rules otherwise.
func TestMCPBroker_ToolsCallDenyIsJSONRPCError(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
	f.p.harnessRules = []types.HarnessToolRules{
		{Harness: "codex-cli", Default: types.ToolAllow, Rules: []types.ToolRule{{Tool: "mcp__linear__delete_issue", Effect: types.ToolDeny}}},
		{Harness: "claude-code", Default: types.ToolAllow},
	}
	rec := f.post(t, mcpToken("codex-cli"), "linear", mcpCall(7, "delete_issue"))
	var one struct {
		ID    int `json:"id"`
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &one); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("status %d body %q: %v", rec.Code, rec.Body, err)
	}
	if one.ID != 7 || one.Error.Code != mcpBlockedCode || one.Error.Message != mcpBlockedMessage || f.up.hitCount() != 0 {
		t.Fatalf("got %+v with %d upstream hits; want the -32001 error for id 7 and none", one, f.up.hitCount())
	}
	wantDecision(t, f.buf, egress.Deny, ruleSourceMCPToolDeny)

	batch := "[" + mcpCall(1, "list_issues") + "," + mcpCall(2, "delete_issue") + "]"
	rec = f.post(t, mcpToken("codex-cli"), "linear", batch)
	var many []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &many); err != nil || len(many) != 2 || f.up.hitCount() != 0 {
		t.Fatalf("batch: %q (%v), hits %d; want two errors and none sent", rec.Body, err, f.up.hitCount())
	}
	// Another harness's set does not apply: claude-code's call is sent.
	if rec := f.post(t, mcpToken("claude-code"), "linear", mcpCall(3, "delete_issue")); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
		t.Fatalf("claude-code: status %d hits %d; want it sent", rec.Code, f.up.hitCount())
	}

	// One harness: the run-wide rules decide.
	g := newMCPFixture(t, types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "mcp__linear__delete_issue", Effect: types.ToolDeny}}}, nil, nil)
	if rec := g.post(t, mcpToken("codex-cli"), "linear", mcpCall(4, "delete_issue")); !strings.Contains(rec.Body.String(), mcpBlockedMessage) || g.up.hitCount() != 0 {
		t.Fatalf("run-wide deny: %q, hits %d", rec.Body, g.up.hitCount())
	}
}

// TestMCPBroker_EncodedBodyRefused: what the broker cannot read it cannot hold
// to the rules, so it is refused rather than forwarded.
func TestMCPBroker_EncodedBodyRefused(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
	rec := f.post(t, mcpToken("codex-cli"), "linear", mcpCall(1, "delete_issue"), "Content-Encoding", "gzip")
	if rec.Code != http.StatusUnsupportedMediaType || f.up.hitCount() != 0 {
		t.Fatalf("gzip: status %d hits %d; want 415 and none", rec.Code, f.up.hitCount())
	}
	wantDecision(t, f.buf, egress.Deny, ruleSourceMCPEncoding)

	for name, body := range map[string]string{
		"case-variant key": `{"jsonrpc":"2.0","id":1,"Method":"ping","method":"tools/call","params":{"name":"x"}}`,
		"not json":         `{"jsonrpc":`,
		"no tool name":     `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`,
		"too large":        `{"x":"` + strings.Repeat("a", maxMCPBody) + `"}`,
	} {
		before := f.up.hitCount()
		rec := f.post(t, mcpToken("codex-cli"), "linear", body)
		if rec.Code == http.StatusOK && !strings.Contains(rec.Body.String(), `"error"`) || f.up.hitCount() != before {
			t.Errorf("%s: status %d body %.80q forwarded=%v", name, rec.Code, rec.Body, f.up.hitCount() != before)
		}
	}
}

// TestMCPBroker_DirectClientHoldCall pins the hold boundary chosen for 0.9.0:
// "Ask first" on an MCP tool is the harness's prompt, so the broker passes a
// held call, here from a client that skipped the harness. A deny is the
// broker's own and holds against the same client.
func TestMCPBroker_DirectClientHoldCall(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{ToolRules: []types.ToolRule{
		{Tool: "mcp__linear__create_issue", Effect: types.ToolHold},
		{Tool: "mcp__linear__delete_issue", Effect: types.ToolDeny},
	}}, nil, nil)
	if rec := f.post(t, mcpToken("codex-cli"), "linear", mcpCall(1, "create_issue")); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
		t.Fatalf("held call: status %d hits %d; want it sent (the harness asks)", rec.Code, f.up.hitCount())
	}
	if rec := f.post(t, mcpToken("codex-cli"), "linear", mcpCall(2, "delete_issue")); !strings.Contains(rec.Body.String(), mcpBlockedMessage) || f.up.hitCount() != 1 {
		t.Fatalf("denied call: %q hits %d; want refused", rec.Body, f.up.hitCount())
	}
}

// TestMCPBroker_CredentialAddedAndSandboxCredentialStripped: the server's
// credential is the broker's, resolved from its grant; a credential the
// sandbox sent is never forwarded.
func TestMCPBroker_CredentialAddedAndSandboxCredentialStripped(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
	f.post(t, mcpToken("codex-cli"), "linear", mcpInit, "Authorization", "Bearer smuggled", "X-Api-Key", "smuggled")
	if f.up.auth != mcpServerSecret || f.up.smuggled != "" {
		t.Fatalf("upstream saw Authorization %q, X-Api-Key %q", f.up.auth, f.up.smuggled)
	}
	// A server with no grant gets no credential.
	f.post(t, mcpToken("claude-code"), "github", mcpInit, "Authorization", "Bearer smuggled")
	if f.up.auth != "" {
		t.Fatalf("credential-less server saw Authorization %q", f.up.auth)
	}
}

// TestMCPBroker_CredentialNotInGenericInjector: a config that hands an MCP
// server's host to the generic injector is refused at load.
func TestMCPBroker_CredentialNotInGenericInjector(t *testing.T) {
	grant := uuid.New()
	cfg := Config{RunID: uuid.New(), ControlPlaneURL: "http://127.0.0.1:8080", RunToken: "t",
		MCPServers: []MCPServerConfig{{Key: "linear", URL: "https://mcp.linear.test/mcp", GrantID: &grant}},
		Injection:  []InjectionConfig{{GrantID: grant, InjectionRule: egress.InjectionRule{Host: "MCP.linear.test.", Header: "Authorization"}}}}
	if err := cfg.applyDefaultsAndValidate(); err == nil || !strings.Contains(err.Error(), "MCP broker's alone") {
		t.Fatalf("want the injection refusal, got %v", err)
	}
	cfg.Injection = nil
	if err := cfg.applyDefaultsAndValidate(); err != nil {
		t.Fatalf("without the rule: %v", err)
	}
}

// TestMCPBroker_DirectOriginDeniedEvenWhenHostAllowedElsewhere: while the
// server is attached its host is reached only through the broker, though the
// policy and a second component both allow it.
func TestMCPBroker_DirectOriginDeniedEvenWhenHostAllowedElsewhere(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{AllowedDomains: []string{"mcp.linear.test", "mcp.linear.test:443", "api.other.test"}}, nil, nil)
	// Plain lane.
	req := httptest.NewRequest(http.MethodPost, "http://mcp.linear.test/mcp", strings.NewReader(mcpCall(1, "delete_issue")))
	rec := httptest.NewRecorder()
	f.p.ServeHTTP(rec, req)
	if rec.Code < 400 || f.up.hitCount() != 0 {
		t.Fatalf("plain lane: status %d hits %d; want refused", rec.Code, f.up.hitCount())
	}
	wantDecision(t, f.buf, egress.Deny, ruleSourceMCPDirect)
	// CONNECT.
	if d, _, _ := f.p.evaluate(context.Background(), "mcp.linear.test", 443, http.MethodConnect, ""); d != egress.Deny {
		t.Fatalf("CONNECT decision %s, want deny", d)
	}
	// Another allowed host is untouched.
	if d, _, _ := f.p.evaluate(context.Background(), "api.other.test", 443, http.MethodConnect, ""); d != egress.Allow {
		t.Fatalf("other host decision %s, want allow", d)
	}
}

// A harness that ignores NO_PROXY sends its broker address through the forward lane as an absolute-form
// request: the record carries the broker's redacted path, never the token.
func TestMCPBroker_ForwardLaneRecordNeverCarriesTheToken(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
	tok := mcpToken("claude-code")
	req := httptest.NewRequest(http.MethodPost, "http://wardyn-proxy.test:3128/wardyn/mcp/"+tok+"/linear", strings.NewReader(mcpInit))
	rec := httptest.NewRecorder()
	f.p.ServeHTTP(rec, req)
	if strings.Contains(f.buf.String(), tok) {
		t.Fatalf("the harness's MCP token reached a decision record: %s", f.buf.String())
	}
	if d := lastDecision(t, f.buf); d.Request.Path != mcpRedactedPath("claude-code", "linear") {
		t.Fatalf("path = %q, want the broker's redacted form", d.Request.Path)
	}
}

// mcpResolver answers per host and per call, counting the calls.
type mcpResolver struct {
	mu     sync.Mutex
	answer func(host string, n int) []net.IP
	calls  map[string]int
}

func (c *mcpResolver) LookupIP(host string) ([]net.IP, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[host]++
	return c.answer(host, c.calls[host]), nil
}

func privateAnswer(_ string, _ int) []net.IP { return ips("10.0.0.5") }

// TestMCPBroker_DNSToPrivateRefused: a server name that resolves into private
// space is refused by the guarded transport and never dialled. The name is
// localhost, which an unguarded transport would reach.
func TestMCPBroker_DNSToPrivateRefused(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{}, &mcpResolver{answer: privateAnswer}, nil)
	f.p.mcp.servers["linear"].url = "https://localhost:" + portOf(t, f.up.srv) + "/mcp"
	f.p.mcp.servers["linear"].host, f.p.mcp.servers["linear"].port = "localhost", atoiPort(t, f.up.srv)
	rec := f.post(t, mcpToken("codex-cli"), "linear", mcpInit)
	if rec.Code != http.StatusBadGateway || f.up.hitCount() != 0 {
		t.Fatalf("status %d hits %d; want 502 and none", rec.Code, f.up.hitCount())
	}
	wantDecision(t, f.buf, egress.Deny, "builtin:private-ip")
}

func portOf(t *testing.T, s *httptest.Server) string {
	t.Helper()
	_, port, err := net.SplitHostPort(upstreamAddr(s))
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func atoiPort(t *testing.T, s *httptest.Server) int {
	_, port := splitHostPort(upstreamAddr(s), 0)
	return port
}

// TestMCPBroker_PrivateLiteralRefused: a server URL naming a private or
// loopback literal, in canonical or non-canonical spelling, is refused before
// any dial.
func TestMCPBroker_PrivateLiteralRefused(t *testing.T) {
	for _, u := range []string{
		"https://10.0.0.5/mcp", "https://127.0.0.1/mcp", "https://[fd00::1]/mcp", "https://169.254.169.254/mcp",
		"https://0x7f000001/mcp", "https://2130706433/mcp", "https://127.1/mcp", "https://012.0.0.5/mcp",
	} {
		f := newMCPFixture(t, types.RunPolicySpec{}, nil, map[string]string{"linear": u})
		rec := f.post(t, mcpToken("codex-cli"), "linear", mcpInit)
		if rec.Code < 400 || f.up.hitCount() != 0 || len(f.dialled()) != 0 {
			t.Errorf("%s: status %d hits %d dials %v; want refused before any dial", u, rec.Code, f.up.hitCount(), f.dialled())
			continue
		}
		if d := lastDecision(t, f.buf); d.Decision != egress.Deny || d.RuleSource != "builtin:private-ip" {
			t.Errorf("%s: decision %s %q", u, d.Decision, d.RuleSource)
		}
	}
}

// TestMCPBroker_RebindingPinned: the dial goes to the address the guard
// vetted, never a second resolve; a name that answers private next time is
// refused next time.
func TestMCPBroker_RebindingPinned(t *testing.T) {
	res := &mcpResolver{answer: func(host string, n int) []net.IP {
		if host == "mcp.linear.test" && n > 1 {
			return ips("10.0.0.5")
		}
		return ips("93.184.216.34")
	}}
	f := newMCPFixture(t, types.RunPolicySpec{}, res, nil)
	if rec := f.post(t, mcpToken("codex-cli"), "linear", mcpInit); rec.Code != http.StatusOK {
		t.Fatalf("first: status %d %q", rec.Code, rec.Body)
	}
	if got := f.dialled(); len(got) == 0 || got[len(got)-1] != "93.184.216.34:443" || res.calls["mcp.linear.test"] != 1 {
		t.Fatalf("dials %v after %d resolves; want the pinned public address from one resolve", got, res.calls["mcp.linear.test"])
	}
	if rec := f.post(t, mcpToken("codex-cli"), "linear", mcpInit); rec.Code != http.StatusBadGateway || f.up.hitCount() != 1 {
		t.Fatalf("rebound: status %d hits %d; want 502", rec.Code, f.up.hitCount())
	}
	for _, d := range f.dialled() {
		if strings.HasPrefix(d, "10.") {
			t.Fatalf("dialled the rebound address %s", d)
		}
	}
}

// TestMCPBroker_RedirectToPrivateRefused: a redirect is refused, never
// followed, wherever it points.
func TestMCPBroker_RedirectToPrivateRefused(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
	f.up.status, f.up.location = http.StatusFound, "https://10.0.0.5/mcp"
	rec := f.post(t, mcpToken("codex-cli"), "linear", mcpInit)
	if rec.Code != http.StatusBadGateway || rec.Header().Get("Location") != "" || f.up.hitCount() != 1 {
		t.Fatalf("status %d location %q hits %d; want 502, no Location, one hit", rec.Code, rec.Header().Get("Location"), f.up.hitCount())
	}
	for _, d := range f.dialled() {
		if strings.HasPrefix(d, "10.") {
			t.Fatalf("dialled the redirect target %s", d)
		}
	}
	wantDecision(t, f.buf, egress.Deny, ruleSourceMCPRedirect)
}

// TestMCPBroker_ConfiguredUpstreamUnchanged: under an operator upstream the
// broker CONNECTs the server by name through it, as the git broker does.
func TestMCPBroker_ConfiguredUpstreamUnchanged(t *testing.T) {
	fu := startFakeUpstream(t)
	up, err := parseUpstreamProxy("http://" + fu.addr())
	if err != nil {
		t.Fatal(err)
	}
	buf := &bytes.Buffer{}
	p := newProxy(Options{RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}),
		Sink: &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 8)}, Resolver: publicResolver{}, Upstream: up})
	p.mcp = newMCPBroker([]MCPServerConfig{{Key: "linear", URL: "https://mcp.linear.test/mcp"}}, []MCPBinding{mcpBindingFor("codex-cli", "linear")})
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, mustLocalReq(t, http.MethodPost, "/wardyn/mcp/"+mcpToken("codex-cli")+"/linear", strings.NewReader(mcpInit)))
	if got, _ := fu.snapshot(); got != "CONNECT mcp.linear.test:443" {
		t.Fatalf("corp proxy saw %q, want CONNECT mcp.linear.test:443", got)
	}
}

// TestMCPBroker_TokenNeverLogged: no record of a broker request — decision
// rows, the sidecar's own log, a refusal or an error body — carries the token,
// on success, both 403s, a blocked call or an upstream error.
func TestMCPBroker_TokenNeverLogged(t *testing.T) {
	var logBuf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	f := newMCPFixture(t, types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "mcp__linear__delete_issue", Effect: types.ToolDeny}}}, nil, nil)
	unbound := "tok-unbound-secret-0123456789abcdef"
	var bodies []string
	for _, c := range []struct{ tok, key, body string }{
		{mcpToken("codex-cli"), "linear", mcpInit},                    // success
		{unbound, "linear", mcpInit},                                  // 403 unbound
		{mcpToken("codex-cli"), "github", mcpInit},                    // 403 not enabled
		{mcpToken("codex-cli"), "linear", mcpCall(1, "delete_issue")}, // -32001
	} {
		bodies = append(bodies, f.post(t, c.tok, c.key, c.body).Body.String())
	}
	// An upstream error: the server's address no longer answers.
	f.up.srv.Close()
	bodies = append(bodies, f.post(t, mcpToken("codex-cli"), "linear", mcpInit).Body.String())

	records := f.buf.String()
	redacted, attributed := 0, false
	for _, line := range strings.Split(strings.TrimSpace(records), "\n") {
		var d egress.DecisionLog
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(d.Request.Path, "/wardyn/mcp/<redacted") {
			redacted++
		}
		attributed = attributed || d.Request.Path == "/wardyn/mcp/<redacted:codex-cli>/linear"
	}
	for _, tok := range []string{mcpToken("codex-cli"), unbound} {
		for name, text := range map[string]string{"decision rows": records, "sidecar log": logBuf.String(), "bodies": strings.Join(bodies, "\n")} {
			if strings.Contains(text, tok) {
				t.Errorf("%s carry a token", name)
			}
		}
	}
	if redacted != 5 || !attributed {
		t.Fatalf("decision rows lack the redacted, attributed path:\n%s", records)
	}
}

// TestMCPBroker_HarnessNormalisedNameBlocked: a Block written as a harness
// shows the tool is enforced on the tool's wire name. Claude Code shows
// admin.delete as admin_delete; Codex CLI's code mode, which keeps no "-", shows
// admin-purge as admin_purge.
func TestMCPBroker_HarnessNormalisedNameBlocked(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{ToolRules: []types.ToolRule{
		{Tool: "mcp__linear__admin_delete", Effect: types.ToolDeny},
		{Tool: "mcp__linear__admin_purge", Effect: types.ToolDeny},
	}}, nil, nil)
	for _, tool := range []string{"admin.delete", "admin-purge"} {
		if rec := f.post(t, mcpToken("codex-cli"), "linear", mcpCall(1, tool)); !strings.Contains(rec.Body.String(), mcpBlockedMessage) || f.up.hitCount() != 0 {
			t.Fatalf("%s: %q, hits %d; want blocked", tool, rec.Body, f.up.hitCount())
		}
	}
	if rec := f.post(t, mcpToken("codex-cli"), "linear", mcpCall(2, "admin.list")); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
		t.Fatalf("unblocked tool: status %d hits %d", rec.Code, f.up.hitCount())
	}
}

// TestMCPBroker_DuplicateKeysRefused: a body that gives method, params or name
// twice is refused, since a server reading the first would see a call the
// broker did not check.
func TestMCPBroker_DuplicateKeysRefused(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "mcp__linear__delete_issue", Effect: types.ToolDeny}}}, nil, nil)
	for name, body := range map[string]string{
		"method": `{"jsonrpc":"2.0","id":1,"method":"tools/call","method":"ping","params":{"name":"delete_issue"}}`,
		"params": `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_issue"},"params":{"name":"ok"}}`,
		"name":   `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"delete_issue","name":"ok"}}`,
	} {
		rec := f.post(t, mcpToken("codex-cli"), "linear", body)
		if !strings.Contains(rec.Body.String(), "ambiguous key") || f.up.hitCount() != 0 {
			t.Errorf("duplicate %s: %q forwarded=%v", name, rec.Body, f.up.hitCount() != 0)
		}
	}
	if rec := f.post(t, mcpToken("codex-cli"), "linear", mcpCall(1, "list_issues")); rec.Code != http.StatusOK {
		t.Fatalf("plain call: %d", rec.Code)
	}
}

// TestMCPBroker_TrailingDataRefused: a single message must be the whole body.
// A second value after it would reach a server that reads a stream of values
// without the broker having checked it; whitespace after it is fine.
func TestMCPBroker_TrailingDataRefused(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "mcp__linear__delete_issue", Effect: types.ToolDeny}}}, nil, nil)
	for name, body := range map[string]string{
		"concatenated objects": mcpCall(1, "list_issues") + mcpCall(2, "delete_issue"),
		"trailing garbage":     mcpCall(1, "list_issues") + " xyz",
	} {
		if rec := f.post(t, mcpToken("codex-cli"), "linear", body); !strings.Contains(rec.Body.String(), `"error"`) || f.up.hitCount() != 0 {
			t.Errorf("%s: %q forwarded=%v", name, rec.Body, f.up.hitCount() != 0)
		}
	}
	if rec := f.post(t, mcpToken("codex-cli"), "linear", mcpCall(1, "list_issues")+" \n\t"); rec.Code != http.StatusOK || f.up.hitCount() != 1 {
		t.Fatalf("trailing whitespace: status %d hits %d; want forwarded", rec.Code, f.up.hitCount())
	}
}

// TestMCPBroker_ClaudeAstralNameBlocked: Claude Code shows a character outside
// the Basic Multilingual Plane as two underscores (one per UTF-16 code unit), and
// a Block written that way holds.
func TestMCPBroker_ClaudeAstralNameBlocked(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "mcp__linear__admin__delete", Effect: types.ToolDeny}}}, nil, nil)
	if rec := f.post(t, mcpToken("codex-cli"), "linear", mcpCall(1, "admin\U0001F600delete")); !strings.Contains(rec.Body.String(), mcpBlockedMessage) || f.up.hitCount() != 0 {
		t.Fatalf("%q hits %d; want blocked", rec.Body, f.up.hitCount())
	}
}
