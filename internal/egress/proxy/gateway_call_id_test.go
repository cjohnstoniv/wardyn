// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gwcidRows decodes the decision rows a test's sink mirrored, in order.
func gwcidRows(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("row %q: %v", line, err)
		}
		rows = append(rows, row)
	}
	return rows
}

// gwcidOf returns the call id of the one row with this decision, failing on none or several.
func gwcidOf(t *testing.T, rows []map[string]any, decision egress.Decision, ruleSource string) any {
	t.Helper()
	var found []map[string]any
	for _, row := range rows {
		if row["decision"] == string(decision) && row["rule_source"] == ruleSource {
			found = append(found, row)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one %s %s row, got %d in %v", decision, ruleSource, len(found), rows)
	}
	return found[0]["gateway_call_id"]
}

// The model route's allow row carries the id the gateway was sent, fresh for every call
// and never the sandbox's own, on both lanes. REVERT PROOF: dropping the GatewayCallID
// stamp in forwardInspectedLLM's send leaves the row without the key.
func TestGatewayCallID_ModelRouteAllowRowCarriesTheSentID(t *testing.T) {
	for lane, send := range map[string]func(*gwidRig, *testing.T) *httptest.ResponseRecorder{
		"brokered": func(g *gwidRig, t *testing.T) *httptest.ResponseRecorder { return gwidLanes(g, t)["brokered"]() },
		"mitm":     func(g *gwidRig, t *testing.T) *httptest.ResponseRecorder { return gwidLanes(g, t)["mitm"]() },
	} {
		t.Run(lane, func(t *testing.T) {
			g := newGwidRig(t, gwidRoutes(), false)
			sent := map[string]bool{}
			for range 2 {
				if rec := send(g, t); rec.Code != http.StatusOK {
					t.Fatalf("status %d: %s", rec.Code, rec.Body)
				}
				id := g.gw.last(t).Header.Get(gatewayCallIDHeader)
				if _, err := uuid.Parse(id); err != nil || id == gwidCallID {
					t.Fatalf("the gateway was sent call id %q, want one the proxy made", id)
				}
				sent[id] = true
			}
			if len(sent) != 2 {
				t.Errorf("two calls shared a call id: %v", sent)
			}
			rowIDs := map[string]bool{}
			for _, row := range gwcidRows(t, g.buf) {
				if row["decision"] != string(egress.Allow) || row["rule_source"] != ruleSourceLLM && row["rule_source"] != ruleSourceLLMMITM {
					continue
				}
				id, _ := row["gateway_call_id"].(string)
				rowIDs[id] = true
			}
			for id := range sent {
				if !rowIDs[id] {
					t.Errorf("no allow row carries the sent id %s; rows: %v", id, g.buf)
				}
			}
			if rowIDs[gwidCallID] || rowIDs[""] {
				t.Errorf("an allow row carries the sandbox's id or none: %v", rowIDs)
			}
		})
	}
}

// A dial that fails after the id was made still leaves a deny row that names it, so a
// call that never got an answer joins to a gateway log line that may exist anyway.
func TestGatewayCallID_DialFailedRowCarriesIt(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	prev := g.p.dial
	g.p.dial = func(ctx context.Context, network, target string) (net.Conn, error) {
		if target == "93.184.216.10:443" {
			return nil, errors.New("connection refused")
		}
		return prev(ctx, network, target)
	}
	if rec := gwidLanes(g, t)["brokered"](); rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if id, _ := gwcidOf(t, gwcidRows(t, g.buf), egress.Deny, "builtin:dial-failed").(string); id == "" || id == gwidCallID {
		t.Errorf("dial-failed row call id = %q, want one the proxy made", id)
	} else if _, err := uuid.Parse(id); err != nil {
		t.Errorf("dial-failed row call id %q: %v", id, err)
	}
}

// A request the proxy cannot build is refused with a deny row that names the attempt's id.
func TestGatewayCallID_BuildFailureDenyRowCarriesIt(t *testing.T) {
	g := newGwidRig(t, gwidRoutes(), false)
	req := httptest.NewRequest(http.MethodPost, "https://"+gwidHost+"/v1/messages", strings.NewReader(`{"hi":1}`))
	req.Method = "BAD METHOD" // not a token: http.NewRequestWithContext refuses it
	rec := httptest.NewRecorder()
	g.p.forwardInspectedLLM(rec, req, gwidHost, 443, "v1/messages", "93.184.216.10:443", nil, "", ruleSourceLLMMITM, false, strings.NewReader(`{"hi":1}`), nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if id, _ := gwcidOf(t, gwcidRows(t, g.buf), egress.Deny, ruleSourceLLMMITM).(string); id == "" {
		t.Errorf("the build-failure deny row carries no call id")
	}
	if n := g.gw.count(); n != 0 {
		t.Errorf("the gateway was reached %d times", n)
	}
}

// With no gateway route nothing changes: the sandbox's own header passes as any other does,
// and no row carries the field.
func TestGatewayCallID_AbsentWithNoRoute(t *testing.T) {
	g := newGwidRig(t, nil, false)
	for lane, send := range gwidLanes(g, t) {
		g.buf.Reset()
		if rec := send(); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", lane, rec.Code, rec.Body)
		}
		if got := g.gw.last(t).Header.Get(gatewayCallIDHeader); got != gwidCallID {
			t.Errorf("%s: off changed the sandbox's header: %q", lane, got)
		}
		for _, row := range gwcidRows(t, g.buf) {
			if _, ok := row["gateway_call_id"]; ok {
				t.Errorf("%s: a row carries gateway_call_id with no route: %v", lane, row)
			}
		}
	}
}

// An MCP server of a run with a route is sent a call id and the allow row records it.
func TestGatewayCallID_MCPAllowRowCarriesTheSentID(t *testing.T) {
	seen := make(chan http.Header, 1)
	f := newMCPFixture(t, types.RunPolicySpec{AllowedDomains: []string{"mcp.github.test"}}, nil, nil)
	f.p.mcp = newMCPBroker([]MCPServerConfig{{Key: "github", URL: "https://mcp.github.test/mcp"}}, []MCPBinding{mcpBindingFor("claude-code", "github")})
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(backend.Close)
	f.p.dial = redirectDial(upstreamAddr(backend))
	f.p.gatewayRoutes = gwidRoutes()
	if rec := f.post(t, mcpToken("claude-code"), "github", mcpInit, gatewayCallIDHeader, gwidCallID); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	sent := (<-seen).Get(gatewayCallIDHeader)
	if _, err := uuid.Parse(sent); err != nil || sent == gwidCallID {
		t.Fatalf("the MCP server was sent call id %q", sent)
	}
	if got, _ := gwcidOf(t, gwcidRows(t, f.buf), egress.Allow, ruleSourceMCP).(string); got != sent {
		t.Errorf("allow row call id = %q, want the sent %q", got, sent)
	}
}
