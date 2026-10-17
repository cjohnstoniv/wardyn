// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The MCP broker route: a run's remote MCP servers reached only through the
// proxy. Each harness is configured with
// http://<proxy>/wardyn/mcp/<token>/<server key>, where the token is that
// harness's own, minted at dispatch and known here only by its SHA-256 digest.
// The broker reads the harness from the token (never from the request), refuses
// a server not turned on for that harness, holds every tools/call to the
// harness's tool rules, adds the server's credential and re-originates over the
// same guarded transport as the git broker. Nothing else reaches the server: its
// host is in no allowlist, a direct request to it is refused while it is
// attached (mcpDirectOrigin), and its credential is never an Injection rule.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	routeMCPBroker = "/wardyn/mcp/"
	// maxMCPBody caps a POST the broker reads to check its tool calls.
	maxMCPBody = 1 << 20

	ruleSourceMCP           = "brokered:mcp"
	ruleSourceMCPUnbound    = "brokered:mcp:unbound"
	ruleSourceMCPNotEnabled = "brokered:mcp:not-enabled"
	ruleSourceMCPToolDeny   = "brokered:mcp:tool-deny"
	ruleSourceMCPEncoding   = "brokered:mcp:encoding"
	ruleSourceMCPRequest    = "brokered:mcp:request"
	ruleSourceMCPCredential = "brokered:mcp:credential"
	ruleSourceMCPRedirect   = "brokered:mcp:redirect"
	// ruleSourceMCPDirect refuses a request that names an attached MCP
	// server's host on any lane but the broker's (evaluate).
	ruleSourceMCPDirect = "brokered:mcp:direct"

	// mcpBlockedCode and mcpBlockedMessage are the JSON-RPC error a tools/call
	// the run's rules block is answered with.
	mcpBlockedCode    = -32001
	mcpBlockedMessage = "Blocked by this run's tool rules"
)

// mcpServerKeyRE is a server key: the <server> of mcp__<server>__<tool>, so
// never two underscores, and safe in a URL path segment.
var mcpServerKeyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// mcpBroker is the run's MCP servers and each harness's binding to them.
type mcpBroker struct {
	servers  map[string]*mcpServer
	bindings []MCPBinding
}

// mcpServer is one server and its credential, resolved on first use.
type mcpServer struct {
	key     string
	url     string
	host    string
	port    int
	grantID *uuid.UUID

	mu        sync.Mutex
	header    injectedHeader
	expiresAt int64 // unix ms; 0 is a credential that never expires
}

// newMCPBroker builds the broker from a validated config; nil without servers.
func newMCPBroker(servers []MCPServerConfig, bindings []MCPBinding) *mcpBroker {
	if len(servers) == 0 {
		return nil
	}
	b := &mcpBroker{servers: map[string]*mcpServer{}, bindings: bindings}
	for _, s := range servers {
		u, _ := url.Parse(s.URL)
		port := 443
		if u.Port() != "" {
			port, _ = strconv.Atoi(u.Port())
		}
		host := strings.ToLower(strings.TrimRight(u.Hostname(), "."))
		b.servers[s.Key] = &mcpServer{key: s.Key, url: s.URL, host: host, port: port, grantID: s.GrantID}
	}
	return b
}

// mcpDirectOrigin reports whether host is an attached MCP server's host, at any
// port: the broker is the only way to it while the server is attached, whatever
// the policy, an added host or another component allows.
func (b *mcpBroker) mcpDirectOrigin(host string) bool {
	if b == nil {
		return false
	}
	for _, s := range b.servers {
		if s.host == host {
			return true
		}
	}
	return false
}

// bind names the harness a token was minted for, compared by digest in
// constant time, and the servers turned on for it. "" is no binding.
func (b *mcpBroker) bind(token string) (string, []string) {
	if b == nil || token == "" {
		return "", nil
	}
	sum := sha256.Sum256([]byte(token))
	got := hex.EncodeToString(sum[:])
	var harness string
	var servers []string
	for _, bd := range b.bindings {
		// No early exit: the loop's length must not depend on which one matched.
		if subtle.ConstantTimeCompare([]byte(got), []byte(bd.SHA256)) == 1 {
			harness, servers = bd.Harness, bd.Servers
		}
	}
	return harness, servers
}

// parseMCPBrokerPath splits /wardyn/mcp/<token>/<key>. Nothing may follow the
// key: a streamable HTTP server has one endpoint.
func parseMCPBrokerPath(path string) (token, key string, ok bool) {
	token, key, ok = strings.Cut(strings.TrimPrefix(path, routeMCPBroker), "/")
	return token, key, ok && token != "" && mcpServerKeyRE.MatchString(key)
}

// mcpRedactedPath is the path every record of a broker request carries: the
// token replaced, the harness it named kept, never a value derived from the
// token. key is written only when it is a key at all.
func mcpRedactedPath(harness, key string) string {
	seg := "<redacted>"
	if harness != "" {
		seg = "<redacted:" + harness + ">"
	}
	if !mcpServerKeyRE.MatchString(key) {
		return routeMCPBroker + seg
	}
	return routeMCPBroker + seg + "/" + key
}

// mcpExchange is one broker request as its records describe it.
type mcpExchange struct {
	p       *Proxy
	r       *http.Request
	harness string
	server  *mcpServer
	req     egress.Request
}

func (x *mcpExchange) emit(decision egress.Decision, ruleSource string) {
	if x.p.sink != nil {
		x.p.sink.emit(decisionLog(x.req, decision, ruleSource))
	}
}

// deny records the refusal and answers it. msg never carries the token.
func (x *mcpExchange) deny(w http.ResponseWriter, ruleSource, msg string, status int) {
	x.emit(egress.Deny, ruleSource)
	http.Error(w, msg, status)
}

// handleMCPBroker serves /wardyn/mcp/<token>/<key>.
func (p *Proxy) handleMCPBroker(w http.ResponseWriter, r *http.Request) {
	token, key, _ := parseMCPBrokerPath(r.URL.Path)
	harness, enabled := p.mcp.bind(token)
	x := &mcpExchange{p: p, r: r, harness: harness}
	host, port := splitHostPort(r.Host, 80)
	if s := p.mcp.server(key); s != nil {
		x.server, host, port = s, s.host, s.port
	}
	x.req = egress.Request{AuditReservation: spoolTicket(r.Context()), RunID: p.runID, Host: host, Port: port,
		Method: strings.ToUpper(r.Method), Path: mcpRedactedPath(harness, key), Time: p.now()}
	switch {
	case harness == "":
		x.deny(w, ruleSourceMCPUnbound, "this MCP address is not bound to a harness of this run", http.StatusForbidden)
		return
	case x.server == nil || !slices.Contains(enabled, key):
		x.deny(w, ruleSourceMCPNotEnabled, "this MCP server is not turned on for this harness", http.StatusForbidden)
		return
	case r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodDelete:
		x.deny(w, ruleSourceMCPRequest, "unsupported MCP request method", http.StatusMethodNotAllowed)
		return
	}
	var body []byte
	if r.Method == http.MethodPost {
		var ok bool
		if body, ok = x.readBody(w); !ok {
			return
		}
		if x.refuseBlockedCalls(w, body) {
			return
		}
	}
	x.forward(w, body)
}

// server is the server a key names, or nil.
func (b *mcpBroker) server(key string) *mcpServer {
	if b == nil {
		return nil
	}
	return b.servers[key]
}

// readBody reads a POST the broker must inspect: identity-encoded, at most
// maxMCPBody, valid UTF-8. A body it cannot read is refused, never forwarded.
func (x *mcpExchange) readBody(w http.ResponseWriter) ([]byte, bool) {
	if enc := strings.TrimSpace(x.r.Header.Get("Content-Encoding")); enc != "" && !strings.EqualFold(enc, "identity") {
		x.deny(w, ruleSourceMCPEncoding, "an encoded MCP request body cannot be checked against this run's tool rules; send it uncompressed", http.StatusUnsupportedMediaType)
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(x.r.Body, maxMCPBody+1))
	switch {
	case err != nil:
		x.deny(w, ruleSourceMCPRequest, "read the MCP request body", http.StatusBadRequest)
		return nil, false
	case len(body) > maxMCPBody:
		x.deny(w, ruleSourceMCPRequest, "the MCP request body is larger than 1 MiB", http.StatusRequestEntityTooLarge)
		return nil, false
	case !utf8.Valid(body):
		x.deny(w, ruleSourceMCPRequest, "the MCP request body is not UTF-8 JSON", http.StatusBadRequest)
		return nil, false
	}
	return body, true
}

// mcpMessage is one JSON-RPC message of a request body: its id, and the tool a
// tools/call names.
type mcpMessage struct {
	id   json.RawMessage
	tool string
}

// parseMCPMessages reads a JSON-RPC body, one message or a batch. Keys are
// matched exactly, and a key that differs from method, params or name only by
// case is refused: a server reading the exact key must see the call the broker
// checked.
func parseMCPMessages(body []byte) (msgs []mcpMessage, batch bool, err error) {
	var raws []json.RawMessage
	trimmed := bytes.TrimSpace(body)
	if batch = len(trimmed) > 0 && trimmed[0] == '['; batch {
		if err := json.Unmarshal(trimmed, &raws); err != nil || len(raws) == 0 {
			return nil, true, errors.New("not a JSON-RPC batch")
		}
	} else {
		raws = []json.RawMessage{trimmed}
	}
	for _, raw := range raws {
		m, err := parseMCPMessage(raw)
		if err != nil {
			return nil, batch, err
		}
		msgs = append(msgs, m)
	}
	return msgs, batch, nil
}

func parseMCPMessage(raw json.RawMessage) (mcpMessage, error) {
	obj, err := exactKeys(raw, "method", "params", "id")
	if err != nil {
		return mcpMessage{}, err
	}
	m := mcpMessage{id: obj["id"]}
	var method string
	if v, ok := obj["method"]; ok {
		if err := json.Unmarshal(v, &method); err != nil {
			return mcpMessage{}, errors.New("method is not a string")
		}
	}
	if method != "tools/call" {
		return m, nil
	}
	params, err := exactKeys(obj["params"], "name")
	if err != nil {
		return mcpMessage{}, err
	}
	if err := json.Unmarshal(params["name"], &m.tool); err != nil || m.tool == "" {
		return mcpMessage{}, errors.New("a tools/call names no tool")
	}
	return m, nil
}

// exactKeys decodes a JSON object, refusing one that spells any of names
// with different case or gives it twice: a server reading the other spelling,
// or the first of two, would see a call the broker did not check.
func exactKeys(raw json.RawMessage, names ...string) (map[string]json.RawMessage, error) {
	notObject := errors.New("not a JSON object")
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, notObject
	}
	obj := map[string]json.RawMessage{}
	for dec.More() {
		t, err := dec.Token()
		k, ok := t.(string)
		var v json.RawMessage
		if err != nil || !ok || dec.Decode(&v) != nil {
			return nil, notObject
		}
		for _, n := range names {
			if _, dup := obj[k]; strings.EqualFold(k, n) && (k != n || dup) {
				return nil, fmt.Errorf("ambiguous key %q", k)
			}
		}
		obj[k] = v
	}
	// The object ends, and the body with it: a server that reads a second
	// value from the same body would run a call the broker never checked.
	if t, err := dec.Token(); err != nil || t != json.Delim('}') {
		return nil, notObject
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, notObject
	}
	return obj, nil
}

// refuseBlockedCalls answers a body carrying a tools/call the run's rules
// block, and reports whether it did. A hold passes: the harness asked before it
// called (Wardyn's gate on the gated lane, the harness's own prompt when
// attended), and a direct call that skipped the harness is the residual the
// threat model states. A batch with a blocked call is refused whole, every
// request in it answered with the error, since part of a batch cannot be sent.
func (x *mcpExchange) refuseBlockedCalls(w http.ResponseWriter, body []byte) bool {
	msgs, batch, err := parseMCPMessages(body)
	if err != nil {
		x.emit(egress.Deny, ruleSourceMCPRequest)
		writeJSONRPC(w, jsonRPCError(nil, -32700, "Wardyn could not read this MCP request: "+err.Error()))
		return true
	}
	if !slices.ContainsFunc(msgs, func(m mcpMessage) bool {
		return m.tool != "" && slices.ContainsFunc(mcpToolNames(x.server.key, m.tool), func(name string) bool {
			return x.p.mcpToolEffect(x.harness, name) == types.ToolDeny
		})
	}) {
		return false
	}
	x.emit(egress.Deny, ruleSourceMCPToolDeny)
	var out []map[string]any
	for _, m := range msgs {
		if m.id != nil {
			out = append(out, jsonRPCError(m.id, mcpBlockedCode, mcpBlockedMessage))
		}
	}
	switch {
	case len(out) == 0:
		w.WriteHeader(http.StatusAccepted) // notifications only: nothing to answer
	case batch:
		writeJSONRPC(w, out)
	default:
		writeJSONRPC(w, out[0])
	}
	return true
}

// identToolNameRE is what Codex CLI's code mode replaces with "_" when it
// exposes a tool as an identifier.
var identToolNameRE = regexp.MustCompile(`[^A-Za-z0-9_]`)

// claudeToolName is a name as Claude Code (2.1.231) shows it: every UTF-16 code
// unit outside [A-Za-z0-9_-] becomes "_" (a JavaScript regex without the u
// flag), so a character outside the Basic Multilingual Plane becomes "__".
func claudeToolName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'):
			b.WriteRune(r)
		case r > 0xFFFF:
			b.WriteString("__")
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func identToolName(s string) string { return identToolNameRE.ReplaceAllString(s, "_") }

// mcpToolNames is every name a rule may give one server's tool: as the server
// names it, and as each harness shows it. A call is blocked when a Block names
// any of them.
func mcpToolNames(key, tool string) []string {
	names := []string{"mcp__" + key + "__" + tool}
	for _, norm := range []func(string) string{claudeToolName, identToolName} {
		name := "mcp__" + norm(key) + "__" + norm(tool)
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

// MCPToolPrefixes is every "mcp__<server>__" prefix a rule may name one of a
// server's tools under (mcpToolNames): its key as the server names it, and as
// each harness shows it.
func MCPToolPrefixes(key string) []string {
	out := []string{"mcp__" + key + "__"}
	for _, norm := range []func(string) string{claudeToolName, identToolName} {
		if prefix := "mcp__" + norm(key) + "__"; !slices.Contains(out, prefix) {
			out = append(out, prefix)
		}
	}
	return out
}

// mcpToolEffect is the run's rule for one MCP tool of harness: its own set when
// the run carries one per harness, else the run-wide rules. No rule is a hold.
func (p *Proxy) mcpToolEffect(harness, tool string) types.ToolEffect {
	eff, ok := p.policy.ToolEffectFor(tool)
	if len(p.harnessRules) > 0 {
		eff, ok = types.ToolEffectForHarness(p.harnessRules, harness, tool)
	}
	if !ok {
		return types.ToolHold
	}
	return eff
}

func jsonRPCError(id json.RawMessage, code int, msg string) map[string]any {
	if id == nil {
		id = json.RawMessage("null")
	}
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}}
}

func writeJSONRPC(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

// forward re-originates the request to the server over the guarded transport
// the git broker uses: the literal-IP guard, then egressTarget (private-address
// handling, the resolved IP pinned on a direct dial, the operator's upstream
// and internal hosts as configured). A redirect is refused, never followed.
func (x *mcpExchange) forward(w http.ResponseWriter, body []byte) {
	p, s := x.p, x.server
	if verdict, err := p.evaluator.EvaluateHost(x.r.Context(), x.req); err != nil || verdict == egress.VerdictDeny {
		x.deny(w, "policy:denied", "this MCP server's host is blocked for this run", http.StatusForbidden)
		return
	}
	if _, denied := p.literalIPGuard(x.req, s.host, s.port); denied != nil {
		x.deny(w, denied.RuleSource, "this MCP server's address is not reachable from a run", http.StatusForbidden)
		return
	}
	target, _, err := p.egressTarget(s.host, s.port)
	if err != nil {
		src := "builtin:private-ip"
		if errors.Is(err, errHostUnresolved) {
			src = "builtin:resolve-failed"
		}
		x.emit(egress.Deny, src)
		p.httpError(w, "MCP upstream vet failed", err, http.StatusBadGateway)
		return
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	out, err := http.NewRequestWithContext(context.WithValue(x.r.Context(), vettedIPKey{}, target), x.r.Method, s.url, rdr)
	if err != nil {
		x.deny(w, ruleSourceMCPRequest, "build the MCP request", http.StatusBadGateway)
		return
	}
	copyHeader(out.Header, x.r.Header)
	removeHopByHop(out.Header)
	stripSandboxCredentials(out.Header, "")
	if s.grantID != nil {
		hdr, err := p.mcpCredential(x.r.Context(), s)
		if err != nil {
			x.emit(egress.Deny, ruleSourceMCPCredential)
			p.httpError(w, "the MCP server's credential is unavailable", err, http.StatusBadGateway)
			return
		}
		out.Header.Set(hdr.name, hdr.value)
	}
	resp, err := p.roundTripUpstream(out)
	if err != nil {
		p.failUpstream(w, err, &egress.DecisionLog{Request: x.req}, s.host, "MCP upstream error")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.StatusCode != http.StatusNotModified {
		x.deny(w, ruleSourceMCPRedirect, "the MCP server answered with a redirect, which Wardyn does not follow", http.StatusBadGateway)
		return
	}
	p.emitDialledAllow(decisionLog(x.req, egress.Allow, ruleSourceMCP), s.host)
	relayStreaming(w, resp)
}

// relayStreaming is relay, flushing each chunk: a streamable HTTP server may
// answer with an event stream the harness reads as it arrives.
func relayStreaming(w http.ResponseWriter, resp *http.Response) {
	dst := w.Header()
	copyHeader(dst, resp.Header)
	removeHopByHop(dst)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// mcpCredential is the server's header credential, resolved from its grant by
// this broker alone (the generic injector never holds an MCP rule) and kept
// until near its expiry. Every value is registered for masking.
func (p *Proxy) mcpCredential(ctx context.Context, s *mcpServer) (injectedHeader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.header.value != "" && (s.expiresAt == 0 || time.Now().Before(time.UnixMilli(s.expiresAt).Add(-injectRefreshMargin))) {
		return s.header, nil
	}
	resp, err := p.forwardToControlPlane(ctx, http.MethodGet, "/api/v1/internal/injection/"+s.grantID.String(), nil, "")
	if err != nil {
		return injectedHeader{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return injectedHeader{}, fmt.Errorf("credential resolve answered %d", resp.StatusCode)
	}
	var ri types.ResolvedInjection
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBrokeredBody)).Decode(&ri); err != nil {
		return injectedHeader{}, fmt.Errorf("decode the credential: %w", err)
	}
	if ri.Header == "" || ri.Value == "" || !egress.ValidHeaderName(ri.Header) {
		return injectedHeader{}, errors.New("the credential resolve returned no usable header")
	}
	registerHeaderCredentialFor(p.maskKey, ri.Value)
	s.header, s.expiresAt = injectedHeader{name: ri.Header, value: ri.Value, jti: ri.JTI}, ri.ExpiresAt
	return s.header, nil
}

// validateMCP checks the MCP servers and bindings: keys and URLs as dispatch
// writes them, every binding naming servers that exist, and no Injection rule
// on a server's host — its credential is the broker's alone.
func (c *Config) validateMCP() error {
	hosts := map[string]bool{}
	for i, s := range c.MCPServers {
		if !mcpServerKeyRE.MatchString(s.Key) || slices.ContainsFunc(c.MCPServers[:i], func(o MCPServerConfig) bool { return o.Key == s.Key }) {
			return fmt.Errorf("config: mcp_servers[%d].key: %q is not a unique server key", i, s.Key)
		}
		d, err := types.MCPURLDestination(s.URL)
		if err != nil {
			return fmt.Errorf("config: mcp_servers[%d].url: %w", i, err)
		}
		if s.GrantID != nil && *s.GrantID == uuid.Nil {
			return fmt.Errorf("config: mcp_servers[%d].grant_id: a nil grant", i)
		}
		hosts[d.Host] = true
	}
	for _, in := range c.Injection {
		if hosts[strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.Host), "."))] {
			return fmt.Errorf("config: injection rule host %q is an MCP server's: its credential is the MCP broker's alone", in.Host)
		}
	}
	for i, b := range c.MCPBindings {
		if b.Harness == "" || len(b.SHA256) != sha256.Size*2 || strings.ToLower(b.SHA256) != b.SHA256 {
			return fmt.Errorf("config: mcp_bindings[%d]: a harness and a lower-case SHA-256 hex digest are required", i)
		}
		if _, err := hex.DecodeString(b.SHA256); err != nil {
			return fmt.Errorf("config: mcp_bindings[%d].sha256: %w", i, err)
		}
		for _, k := range b.Servers {
			if !slices.ContainsFunc(c.MCPServers, func(s MCPServerConfig) bool { return s.Key == k }) {
				return fmt.Errorf("config: mcp_bindings[%d].servers: %q is no MCP server of this run", i, k)
			}
		}
	}
	return nil
}
