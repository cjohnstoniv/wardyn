// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The relay branch of the MCP broker: a server in a sandbox of its own is
// reached on the run's relay link, at its MCP proxy's relay listener, and
// nowhere else. Everything before the send is the broker's as for a remote
// server (the harness binding, the tool rules, approve_each, the body scan);
// the send itself differs. There is no host policy or address vetting to apply,
// because the target is not an internet host but a literal address on a link
// only proxies are on; no credential is added, because the server's secrets
// live in its own sandbox; and no gateway identity route can cover it. The
// relay token rides only in the path, and no record carries it.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

const (
	// routeMCPRelay is the one path an MCP proxy's relay listener serves:
	// /wardyn/mcp-relay/<relay token>.
	routeMCPRelay = "/wardyn/mcp-relay/"

	// ruleSourceMCPRelayDown is a relayed call whose MCP proxy could not be
	// reached; ruleSourceMCPRelayRefused one that proxy refused the relay token
	// for; ruleSourceMCPHeaderMismatch a request whose Mcp-Method or Mcp-Name
	// header names another call than its body.
	ruleSourceMCPRelayDown      = "brokered:mcp:relay-down"
	ruleSourceMCPRelayRefused   = "brokered:mcp:relay-refused"
	ruleSourceMCPHeaderMismatch = "brokered:mcp:header-mismatch"

	// mcpRelayDownCode and mcpRelayDownMessage answer each message of a call
	// that could not reach the server's sandbox.
	mcpRelayDownCode    = -32002
	mcpRelayDownMessage = "This MCP server's sandbox is not running"

	// mcpMethodHeader and mcpNameHeader name the call a streamable HTTP
	// request carries, so a server or a meter can route by them unparsed.
	mcpMethodHeader = "Mcp-Method"
	mcpNameHeader   = "Mcp-Name"
)

// mcpRelayTokenRE is a relay token: 43 or more base64url characters (256 bits or more).
var mcpRelayTokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{43,}$`)

// newMCPRelayServer is a server in its own sandbox: its records name the relay
// address, and mcpDirectOrigin refuses that address on every other lane. The
// config is validated (validateRelay), so the address parses.
func newMCPRelayServer(s MCPServerConfig) *mcpServer {
	ap, _ := netip.ParseAddrPort(s.Relay.Addr)
	relay := *s.Relay
	return &mcpServer{key: s.Key, host: ap.Addr().Unmap().String(), port: int(ap.Port()), relay: &relay}
}

// validateRelay checks a relay server: no URL and no grant, its address a
// literal IP and port (relayAddrOK), never a host name, and a relay token.
func (s MCPServerConfig) validateRelay() error {
	switch ap, err := netip.ParseAddrPort(s.Relay.Addr); {
	case s.URL != "" || s.GrantID != nil:
		return errors.New("a relay server has no url and no grant_id")
	case err != nil || !relayAddrOK(ap):
		return fmt.Errorf("relay.addr %q is not a literal IP and port (never loopback, unspecified, multicast or link-local)", s.Relay.Addr)
	case !mcpRelayTokenRE.MatchString(s.Relay.Token):
		return errors.New("relay.token is not 43 or more base64url characters")
	}
	return nil
}

// relayAddrOK is the address rule for both ends of a relay link: a port, and an
// IP that is not loopback, unspecified, multicast or link-local (which holds
// the cloud metadata address). Any other range is allowed, so a pool outside
// RFC 1918, such as 100.64.0.0/10, works.
func relayAddrOK(ap netip.AddrPort) bool {
	a := ap.Addr().Unmap()
	return ap.Port() != 0 && a.IsValid() && !a.IsLoopback() && !a.IsUnspecified() && !a.IsMulticast() &&
		!a.IsLinkLocalUnicast() && !a.IsLinkLocalMulticast()
}

// forwardRelay sends the call to the server's MCP proxy on the relay link.
func (x *mcpExchange) forwardRelay(w http.ResponseWriter, body []byte) {
	p, s := x.p, x.server
	scan, ok := x.scanBody(w, body)
	if !ok {
		return
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	// The control-plane transport dials the address on the context and nothing else, and never chains
	// through an upstream proxy: the relay link is not the internet.
	ctx := context.WithValue(x.r.Context(), vettedIPKey{}, s.relay.Addr)
	out, err := http.NewRequestWithContext(ctx, x.r.Method, "http://"+s.relay.Addr+routeMCPRelay+s.relay.Token, rdr)
	if err != nil {
		x.deny(w, ruleSourceMCPRequest, "build the MCP request", http.StatusBadGateway)
		return
	}
	copyHeader(out.Header, x.r.Header)
	removeHopByHop(out.Header)
	stripSandboxCredentials(out.Header, "")
	p.stripGatewayHeaders(out.Header)
	if !x.mcpCallHeaders(w, out.Header, body) {
		return
	}
	// Rows on both proxies join on the call id.
	callID := uuid.NewString()
	out.Header.Set(gatewayCallIDHeader, callID)
	stripSandboxRequestIDs(out.Header)
	listActive := p.mcpListFilterActive(x.harness, s.key)
	if listActive {
		out.Header.Set("Accept-Encoding", "identity")
	}
	resp, err := p.controlTransport.RoundTrip(out)
	if err == nil && resp.StatusCode == http.StatusBadGateway {
		// The MCP proxy is up but its host did not answer, or the server exited before answering:
		// the server is down all the same.
		_ = resp.Body.Close()
		err = errors.New("the MCP proxy answered 502: its host did not answer, or the server exited before answering")
	}
	if err != nil {
		x.relayDown(w, body, callID, scan, err)
		return
	}
	x.upstreamAnswered = true
	defer func() { _ = resp.Body.Close() }()
	refused := ""
	switch {
	case resp.StatusCode == http.StatusForbidden:
		refused = ruleSourceMCPRelayRefused
	case resp.StatusCode >= 300 && resp.StatusCode < 400 && resp.StatusCode != http.StatusNotModified:
		refused = ruleSourceMCPRedirect
	}
	row := decisionLog(x.req, egress.Allow, ruleSourceMCP)
	if refused != "" {
		row = decisionLog(x.req, egress.Deny, refused)
	} else if x.approvalID != nil {
		row.RuleSource, row.ApprovalID = ruleSourceMCPApproved, x.approvalID
	}
	row.GatewayCallID, row.Cause, row.Scan = callID, mcpToolCause(body), scan
	if p.sink != nil {
		p.sink.emit(row)
	}
	if refused != "" {
		http.Error(w, "the MCP server's sandbox refused this call", http.StatusBadGateway)
		return
	}
	if listActive && !x.filterToolLists(w, resp, callID) {
		return
	}
	p.relayStreaming(w, resp)
}

// relayDown answers a call the server's MCP proxy could not take, or answered
// 502 for (its host did not answer, or the server exited before answering):
// each request of the body gets the JSON-RPC error, and a
// body with no request (notifications only, or none) a 502. The error
// is logged without its URL, which holds the relay token.
func (x *mcpExchange) relayDown(w http.ResponseWriter, body []byte, callID string, scan *egress.ScanSummary, err error) {
	if ue := (*url.Error)(nil); errors.As(err, &ue) {
		err = ue.Err
	}
	slog.Warn("wardyn-proxy: an MCP server's sandbox did not answer the relay", slog.String("server", x.server.key), slog.Any("err", err))
	if x.p.sink != nil {
		row := decisionLog(x.req, egress.Deny, ruleSourceMCPRelayDown)
		row.GatewayCallID, row.Scan = callID, scan
		x.p.sink.emit(row)
	}
	msgs, batch, perr := parseMCPMessages(body)
	if body == nil || perr != nil {
		http.Error(w, mcpRelayDownMessage+".", http.StatusBadGateway)
		return
	}
	var outs []map[string]any
	for _, m := range msgs {
		if m.id != nil {
			outs = append(outs, jsonRPCError(m.id, mcpRelayDownCode, mcpRelayDownMessage))
		}
	}
	switch {
	case len(outs) == 0:
		http.Error(w, mcpRelayDownMessage+".", http.StatusBadGateway)
	case batch:
		writeJSONRPC(w, outs)
	default:
		writeJSONRPC(w, outs[0])
	}
}

// mcpCallHeaders applies the header rule to an outbound MCP request: the
// proxy decides from the body, then writes the headers itself. A header the
// harness sent that names another call than the body is refused, since a server
// reading it must never see a call other than the one that was checked. Mcp-Method
// is written when every message of the body names the same method, Mcp-Name
// only for a single tools/call. A bodiless request (a stream open, a session
// end) carries neither.
func (x *mcpExchange) mcpCallHeaders(w http.ResponseWriter, h http.Header, body []byte) bool {
	method, name := mcpBodyCall(body)
	for _, hv := range []struct{ header, want string }{{mcpMethodHeader, method}, {mcpNameHeader, name}} {
		if vs, sent := h[http.CanonicalHeaderKey(hv.header)]; sent && (len(vs) != 1 || vs[0] != hv.want) {
			x.deny(w, ruleSourceMCPHeaderMismatch, "the request's "+hv.header+" header does not match its body", http.StatusBadRequest)
			return false
		}
		h.Del(hv.header)
		// A name that folds to it ("_" for "-", any case) reads as it to some servers: never sent.
		for k := range h {
			if strings.EqualFold(strings.ReplaceAll(k, "_", "-"), hv.header) {
				delete(h, k)
			}
		}
		if hv.want != "" {
			h.Set(hv.header, hv.want)
		}
	}
	return true
}

// mcpBodyCall is the method every message of a JSON-RPC body names ("" when
// they differ, or one names none), and the tool of a single tools/call. The
// body was already parsed with exact keys (refuseBlockedCalls), so a body this
// cannot read names nothing.
func mcpBodyCall(body []byte) (method, name string) {
	if len(body) == 0 {
		return "", ""
	}
	trimmed := bytes.TrimSpace(body)
	batch := len(trimmed) > 0 && trimmed[0] == '['
	raws := []json.RawMessage{trimmed}
	if batch {
		// A fresh slice: decoding into raws itself would write over the body it aliases.
		raws = nil
		if json.Unmarshal(trimmed, &raws) != nil || len(raws) == 0 {
			return "", ""
		}
	}
	var methods []string
	for _, raw := range raws {
		obj, err := exactKeys(raw, "method", "params", "id")
		var m string
		if err != nil || json.Unmarshal(obj["method"], &m) != nil || m == "" {
			return "", ""
		}
		methods = append(methods, m)
	}
	if slices.ContainsFunc(methods, func(m string) bool { return m != methods[0] }) {
		return "", ""
	}
	if !batch && methods[0] == "tools/call" {
		if msgs, _, err := parseMCPMessages(body); err == nil && len(msgs) == 1 {
			name = msgs[0].tool
		}
	}
	return methods[0], name
}
