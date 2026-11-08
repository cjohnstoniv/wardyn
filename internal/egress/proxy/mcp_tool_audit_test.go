// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func allowAllMCPFixture(t *testing.T) *mcpFixture {
	t.Helper()
	f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
	f.p.harnessRules = []types.HarnessToolRules{
		{Harness: "claude-code", Default: types.ToolAllow},
		{Harness: "codex-cli", Default: types.ToolAllow},
	}
	return f
}

// The allow row of a forwarded tools/call names the tool in cause; other
// messages leave cause empty.
func TestMCPBrokerAllowRowNamesTools(t *testing.T) {
	f := allowAllMCPFixture(t)
	if rec := f.post(t, mcpToken("claude-code"), "linear", mcpCall(1, "create_issue")); rec.Code != http.StatusOK {
		t.Fatalf("one call: status %d, want 200", rec.Code)
	}
	if d := wantDecision(t, f.buf, egress.Allow, ruleSourceMCP); d.Cause != `tool "create_issue"` {
		t.Errorf("one call: cause = %q, want %q", d.Cause, `tool "create_issue"`)
	}

	batch := "[" + mcpCall(2, "list_issues") + "," + mcpInit + "," + mcpCall(3, "create_issue") + "," + mcpCall(4, "list_issues") + "]"
	if rec := f.post(t, mcpToken("claude-code"), "linear", batch); rec.Code != http.StatusOK {
		t.Fatalf("batch: status %d, want 200", rec.Code)
	}
	want := `tools "list_issues", "create_issue"`
	if d := wantDecision(t, f.buf, egress.Allow, ruleSourceMCP); d.Cause != want {
		t.Errorf("batch: cause = %q, want %q", d.Cause, want)
	}

	if rec := f.post(t, mcpToken("claude-code"), "linear", mcpInit); rec.Code != http.StatusOK {
		t.Fatalf("initialize: status %d, want 200", rec.Code)
	}
	if d := wantDecision(t, f.buf, egress.Allow, ruleSourceMCP); d.Cause != "" {
		t.Errorf("initialize: cause = %q, want empty", d.Cause)
	}
}

// The row never carries a call's arguments, anywhere.
func TestMCPBrokerAllowRowNeverCarriesArguments(t *testing.T) {
	f := allowAllMCPFixture(t)
	const secret = "s3cret-argument-value"
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "restart_service", "arguments": map[string]any{"host": secret}}})
	if rec := f.post(t, mcpToken("claude-code"), "linear", string(body)); rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", rec.Code)
	}
	d := wantDecision(t, f.buf, egress.Allow, ruleSourceMCP)
	if d.Cause != `tool "restart_service"` {
		t.Errorf("cause = %q, want the tool named", d.Cause)
	}
	row, _ := json.Marshal(d)
	if strings.Contains(string(row), secret) {
		t.Errorf("row carries the call's arguments: %s", row)
	}
}
