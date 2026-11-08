// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMCPBroker_ThinSandboxEmptyAllowlist proves the thin-sandbox recipe at the
// proxy: a run whose allowlist is empty and whose first-use posture is
// always_deny reaches its MCP server only through the broker. The server's
// credential is the broker's, a denied tool never leaves, a direct request to
// the server or any other host is refused, and every request leaves a decision
// record for the run.
func TestMCPBroker_ThinSandboxEmptyAllowlist(t *testing.T) {
	f := newMCPFixture(t, types.RunPolicySpec{
		FirstUseApproval: types.FirstUseAlwaysDeny,
		ToolRules:        []types.ToolRule{{Tool: "mcp__linear__delete_issue", Effect: types.ToolDeny}},
	}, nil, nil)

	// The broker serves the server with nothing on the allowlist.
	rec := f.post(t, mcpToken("claude-code"), "linear", mcpCall(1, "list_issues"))
	if rec.Code != http.StatusOK || f.up.hitCount() != 1 {
		t.Fatalf("broker call: status %d, hits %d; want 200 and one", rec.Code, f.up.hitCount())
	}
	if f.up.auth != mcpServerSecret {
		t.Fatalf("upstream saw Authorization %q, want the broker's credential", f.up.auth)
	}
	d := wantDecision(t, f.buf, egress.Allow, ruleSourceMCP)
	if d.Request.RunID != f.p.runID {
		t.Fatalf("decision run %s, want %s", d.Request.RunID, f.p.runID)
	}

	// A tool the rules deny is refused by the broker and never sent.
	rec = f.post(t, mcpToken("claude-code"), "linear", mcpCall(2, "delete_issue"))
	if !strings.Contains(rec.Body.String(), mcpBlockedMessage) || f.up.hitCount() != 1 {
		t.Fatalf("denied tool: %q, hits %d; want refused and not sent", rec.Body, f.up.hitCount())
	}
	wantDecision(t, f.buf, egress.Deny, ruleSourceMCPToolDeny)

	// The server's own host is refused on every other lane.
	req := httptest.NewRequest(http.MethodPost, "http://mcp.linear.test/mcp", strings.NewReader(mcpCall(3, "list_issues")))
	direct := httptest.NewRecorder()
	f.p.ServeHTTP(direct, req)
	if direct.Code < 400 || f.up.hitCount() != 1 {
		t.Fatalf("direct request: status %d, hits %d; want refused and not sent", direct.Code, f.up.hitCount())
	}
	wantDecision(t, f.buf, egress.Deny, ruleSourceMCPDirect)
	if d, _, _ := f.p.evaluate(context.Background(), "mcp.linear.test", 443, http.MethodConnect, ""); d != egress.Deny {
		t.Fatalf("CONNECT to the server: %s, want deny", d)
	}

	// Any other host is refused: nothing is on the allowlist.
	req = httptest.NewRequest(http.MethodGet, "http://api.example.test/", nil)
	other := httptest.NewRecorder()
	f.p.ServeHTTP(other, req)
	if other.Code < 400 || f.up.hitCount() != 1 {
		t.Fatalf("other host: status %d, hits %d; want refused and not sent", other.Code, f.up.hitCount())
	}
	if d := lastDecision(t, f.buf); d.Decision != egress.Deny || d.Request.Host != "api.example.test" {
		t.Fatalf("other host decision %+v, want deny for api.example.test", d)
	}
	if d, _, _ := f.p.evaluate(context.Background(), "api.example.test", 443, http.MethodConnect, ""); d != egress.Deny {
		t.Fatalf("CONNECT to another host: %s, want deny", d)
	}

	// A harness the server is not turned on for gets nothing through the broker.
	if rec := f.post(t, mcpToken("codex-cli"), "github", mcpInit); rec.Code != http.StatusForbidden || f.up.hitCount() != 1 {
		t.Fatalf("server not on for the harness: status %d, hits %d; want 403 and none", rec.Code, f.up.hitCount())
	}
	wantDecision(t, f.buf, egress.Deny, ruleSourceMCPNotEnabled)
}
