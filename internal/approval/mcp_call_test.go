// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package approval

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// callKey stands in for a call's sha256 hex digest.
var callKey = strings.Repeat("0123456789abcdef", 4)

func mcpCallRow(t *testing.T, extra map[string]any, requested time.Time) types.ApprovalRequest {
	t.Helper()
	scope := map[string]any{"tool": "mcp__jira__delete_issue", "mcp_server": "jira", "arguments": map[string]any{"id": 1}, "call_sha256": callKey}
	for k, v := range extra {
		scope[k] = v
	}
	return types.ApprovalRequest{Kind: types.ApprovalToolCall, State: types.ApprovalPending, RequestedAt: requested, RequestedScope: mustScope(t, scope)}
}

// TestTallyKeyMCPCall: an MCP call row is a tool_call known structurally by
// call_sha256 with no grant, and an Azure DevOps escalation keeps its own key.
func TestTallyKeyMCPCall(t *testing.T) {
	now := time.Now().UTC()
	if got := TallyKey(mcpCallRow(t, nil, now)); got != TallyToolCallMCP {
		t.Fatalf("TallyKey(mcp call) = %q, want %q", got, TallyToolCallMCP)
	}
	if TallyToolCallMCP != "tool_call:mcp_call" {
		t.Errorf("TallyToolCallMCP = %q", TallyToolCallMCP)
	}
	g := uuid.New()
	withGrant := mcpCallRow(t, nil, now)
	withGrant.GrantID = &g
	if got := TallyKey(withGrant); got == TallyToolCallMCP {
		t.Error("a row carrying a grant is not an MCP call row")
	}
	ado := types.ApprovalRequest{Kind: types.ApprovalToolCall, State: types.ApprovalPending, GrantID: &g, RequestedScope: adoScope(t, g)}
	if got := TallyKey(ado); got != TallyToolCallADO {
		t.Errorf("TallyKey(ado) = %q, want %q", got, TallyToolCallADO)
	}
	plain := types.ApprovalRequest{Kind: types.ApprovalToolCall, State: types.ApprovalPending, RequestedScope: mustScope(t, map[string]any{"tool": "Bash", "cmd": "ls"})}
	if got := TallyKey(plain); got != string(types.ApprovalToolCall) {
		t.Errorf("TallyKey(plain tool_call) = %q, want the bare kind", got)
	}
	empty := mcpCallRow(t, map[string]any{"call_sha256": ""}, now)
	if got := TallyKey(empty); got == TallyToolCallMCP {
		t.Error("an empty call_sha256 is not an MCP call row")
	}
}

// TestHoldMCPCallBounded: a path-B row carries hold_seconds and is held only
// for that window from the later of requested_at and the last rejoin; a path-G
// row has none and stays live while PENDING, like any tool_call.
func TestHoldMCPCallBounded(t *testing.T) {
	now := time.Now().UTC()
	bounded := mcpCallRow(t, map[string]any{"hold_seconds": 45}, now.Add(-10*time.Second))
	held, until := Hold(bounded, now)
	if !held || !until.Equal(bounded.RequestedAt.Add(45*time.Second)) {
		t.Fatalf("fresh bounded row: held=%v until=%v, want held until requested_at+45s", held, until)
	}
	if held, _ := Hold(bounded, now.Add(40*time.Second)); held {
		t.Error("a bounded row is still held after its window")
	}
	rejoined := bounded
	rejoin := now.Add(30 * time.Second)
	rejoined.LastRejoinedAt = &rejoin
	if held, _ := Hold(rejoined, now.Add(60*time.Second)); !held {
		t.Error("a rejoin must start the window again")
	}
	over := mcpCallRow(t, map[string]any{"hold_seconds": 100000}, now.Add(-time.Hour))
	if held, _ := Hold(over, now); held {
		t.Error("a hold_seconds above the ceiling must not make an hour-old row live")
	}
	live := mcpCallRow(t, nil, now.Add(-3*time.Hour))
	if held, until := Hold(live, now); !held || !until.IsZero() {
		t.Errorf("gate-raised row: held=%v until=%v, want live with no until", held, until)
	}
	decided := bounded
	decided.State = types.ApprovalApproved
	if held, _ := Hold(decided, now); held {
		t.Error("a decided row is never held")
	}
}

func kubeCallRow(t *testing.T, extra map[string]any, requested time.Time) types.ApprovalRequest {
	t.Helper()
	scope := map[string]any{"cluster": "k8s.example.com", "namespace": "team-a", "method": "PATCH",
		"path": "/apis/apps/v1/namespaces/team-a/deployments/web", "call_sha256": callKey, "hold_seconds": 90}
	for k, v := range extra {
		scope[k] = v
	}
	return types.ApprovalRequest{Kind: types.ApprovalToolCall, State: types.ApprovalPending, RequestedAt: requested, RequestedScope: mustScope(t, scope)}
}

// TestTallyKeyKubeCall: a held kubectl write is its own tally key, never an MCP call's, and a
// grant id makes it neither.
func TestTallyKeyKubeCall(t *testing.T) {
	now := time.Now().UTC()
	if got := TallyKey(kubeCallRow(t, nil, now)); got != "tool_call:kube_call" {
		t.Fatalf("TallyKey(kube call) = %q, want tool_call:kube_call", got)
	}
	g := uuid.New()
	withGrant := kubeCallRow(t, nil, now)
	withGrant.GrantID = &g
	if got := TallyKey(withGrant); strings.HasPrefix(got, "tool_call:") {
		t.Errorf("TallyKey(kube call with a grant) = %q", got)
	}
	if got := TallyKey(mcpCallRow(t, nil, now)); got != TallyToolCallMCP {
		t.Errorf("TallyKey(mcp call) = %q", got)
	}
}

// TestHoldKubeCallBounded: a held kubectl write is held for its window, like an MCP call the proxy holds.
func TestHoldKubeCallBounded(t *testing.T) {
	now := time.Now().UTC()
	row := kubeCallRow(t, nil, now.Add(-10*time.Second))
	if held, until := Hold(row, now); !held || !until.Equal(row.RequestedAt.Add(90*time.Second)) {
		t.Fatalf("held=%v until=%v, want held until requested_at+90s", held, until)
	}
	if held, _ := Hold(row, now.Add(100*time.Second)); held {
		t.Error("held after its window")
	}
}
