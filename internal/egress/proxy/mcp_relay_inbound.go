// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// An MCP sandbox proxy's relay listener: the one way the run's own proxy
// reaches the server. It is bound to the run's relay-link address only, and it
// is its own http.Server with its own handler, never the forward handler: it
// serves /wardyn/mcp-relay/<token> and answers 404 to anything else, a CONNECT
// or an absolute-URI request included. The token is checked by its SHA-256 in
// constant time, stripped, and the call goes to wardyn-mcp-host on the MCP
// sandbox's network. Every answer is scrubbed of the sandbox's own secrets,
// whatever the run's response-scrub setting.

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

const (
	// ruleSourceRelayMCP is a relayed call this proxy sent to its server;
	// ruleSourceRelayMCPUnbound one it refused for a wrong relay token.
	ruleSourceRelayMCP        = "relay:mcp"
	ruleSourceRelayMCPUnbound = "relay:mcp:unbound"

	// mcpHostPath is wardyn-mcp-host's one endpoint.
	mcpHostPath = "/mcp"
)

// mcpRelayInbound serves the relay listener.
type mcpRelayInbound struct {
	p        *Proxy
	digest   string
	upstream netip.AddrPort
	// corpus is the server's own secret values (relay_scrub_values), with each
	// multi-line value's renderings and lines: scrubbed from every answer with
	// the credentials this proxy holds. It is not the scan corpus: the LLM scan
	// switch never empties it, and it never reaches the outbound scanners.
	corpus [][]byte
}

// newMCPRelayInbound is the relay listener's server for a validated MCP sandbox
// config, closed when srv shuts down; nil for any other proxy.
func newMCPRelayInbound(p *Proxy, cfg *Config, srv *http.Server) *http.Server {
	in := cfg.MCPRelayInbound
	if in == nil {
		return nil
	}
	h := &mcpRelayInbound{p: p, digest: in.TokenSHA256}
	h.upstream, _ = netip.ParseAddrPort(in.Upstream)
	for _, v := range cfg.RelayScrubValues {
		h.corpus = append(append(h.corpus, []byte(v)), secretmask.MultilineRenderings([]byte(v))...)
	}
	relay := &http.Server{Addr: in.Listen, Handler: h, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second}
	srv.RegisterOnShutdown(func() { _ = relay.Close() })
	return relay
}

func (h *mcpRelayInbound) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token, ok := strings.CutPrefix(r.URL.Path, routeMCPRelay)
	if r.Method == http.MethodConnect || r.URL.Host != "" || !ok || token == "" || strings.Contains(token, "/") {
		http.NotFound(w, r)
		return
	}
	p := h.p
	req := egress.Request{RunID: p.runID, Host: h.upstream.Addr().String(), Port: int(h.upstream.Port()),
		Method: strings.ToUpper(r.Method), Path: routeMCPRelay + "<redacted>", Time: p.now()}
	emit := func(d egress.Decision, source, cause string) {
		if p.sink != nil {
			row := decisionLog(req, d, source)
			row.GatewayCallID, row.Cause = r.Header.Get(gatewayCallIDHeader), cause
			p.sink.emit(row)
		}
	}
	sum := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(h.digest)) != 1 {
		emit(egress.Deny, ruleSourceRelayMCPUnbound, "")
		http.Error(w, "this relay address is not bound to this MCP server", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodDelete {
		emit(egress.Deny, ruleSourceRelayMCP, "unsupported method")
		http.Error(w, "unsupported MCP request method", http.StatusMethodNotAllowed)
		return
	}
	ctx := context.WithValue(r.Context(), vettedIPKey{}, h.upstream.String())
	out, err := http.NewRequestWithContext(ctx, r.Method, "http://"+h.upstream.String()+mcpHostPath, r.Body)
	if err != nil {
		emit(egress.Deny, ruleSourceRelayMCP, "build the request")
		http.Error(w, "build the MCP request", http.StatusBadGateway)
		return
	}
	copyHeader(out.Header, r.Header)
	removeHopByHop(out.Header)
	stripSandboxCredentials(out.Header, "")
	out.ContentLength = r.ContentLength
	resp, err := p.controlTransport.RoundTrip(out)
	if err != nil {
		slog.Warn("wardyn-proxy: the MCP server's host did not answer", slog.Any("err", err))
		emit(egress.Deny, ruleSourceRelayMCP, "the MCP server's host did not answer")
		http.Error(w, mcpRelayDownMessage+".", http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	emit(egress.Allow, ruleSourceRelayMCP, "")
	sw, done := h.scrubbed(w)
	defer done()
	flushRelay(sw, resp)
}

// scrubbed wraps w with a masking writer over the sandbox's own secrets: the
// relay scrub list and every credential this proxy registered, each also as
// it appears inside a JSON string, since an MCP answer is JSON. Always on.
func (h *mcpRelayInbound) scrubbed(w http.ResponseWriter) (http.ResponseWriter, func()) {
	secrets := append(append([][]byte(nil), h.corpus...), procRegistry.Masker(h.p.maskKey).Secrets()...)
	m := secretmask.NewMasker(secretmask.JSONEscapedVariants(secrets))
	if len(m.Secrets()) == 0 {
		return w, func() {}
	}
	s := &scrubbedWriter{ResponseWriter: w, m: m, mw: secretmask.NewMaskingWriter(w, m)}
	return s, func() { _ = s.mw.Close() }
}
