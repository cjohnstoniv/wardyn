// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_MCPSandboxApprovalSQLMatchesGo: the store's SQL test for an MCP server's egress hold (the run's cap
// leaves it out, the server's cap counts it) and isMCPSandboxRow agree on every edge of the label, each row read
// back from Postgres: an empty, null, numeric or object mcp_sandbox, none at all, a key on another kind, and a
// real key, the one MCP row.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
func TestPG_MCPSandboxApprovalSQLMatchesGo(t *testing.T) {
	pool := throwawayPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	for _, c := range []struct {
		name  string
		kind  types.ApprovalKind
		scope string
		mcp   bool
	}{
		{"empty", types.ApprovalEgressDomain, `{"host":"a.test","mcp_sandbox":""}`, false},
		{"null", types.ApprovalEgressDomain, `{"host":"a.test","mcp_sandbox":null}`, false},
		{"number", types.ApprovalEgressDomain, `{"host":"a.test","mcp_sandbox":1}`, false},
		{"object", types.ApprovalEgressDomain, `{"host":"a.test","mcp_sandbox":{"key":"files"}}`, false},
		{"missing", types.ApprovalEgressDomain, `{"host":"a.test"}`, false},
		{"another kind", types.ApprovalToolCall, `{"tool":"Bash","mcp_sandbox":"files"}`, false},
		{"real key", types.ApprovalEgressDomain, `{"host":"a.test","mcp_sandbox":"files"}`, true},
	} {
		now, id := time.Now().UTC(), uuid.New()
		if _, err := pg.CreateRun(ctx, types.AgentRun{
			ID: id, CreatedAt: now, UpdatedAt: now, CreatedBy: "sub-mcp-label", Agent: "claude-code",
			Task: "mcp label", ConfinementClass: types.CC2, State: types.RunRunning,
			SPIFFEID: "spiffe://wardyn.local/agent-run/" + id.String(), RunnerTarget: "docker",
		}); err != nil {
			t.Fatalf("%s: create run: %v", c.name, err)
		}
		ap, err := pg.CreateApproval(ctx, types.ApprovalRequest{ID: uuid.New(), RunID: id, Kind: c.kind,
			RequestedScope: json.RawMessage(c.scope), State: types.ApprovalPending, RequestedAt: now})
		if err != nil {
			t.Fatalf("%s: create approval: %v", c.name, err)
		}
		back, err := pg.GetApproval(ctx, ap.ID)
		if err != nil {
			t.Fatalf("%s: read the approval back: %v", c.name, err)
		}
		if got := isMCPSandboxRow(back); got != c.mcp {
			t.Errorf("%s: isMCPSandboxRow(%s) = %v, want %v", c.name, back.RequestedScope, got, c.mcp)
		}
		mcp := 0
		if isMCPSandboxRow(back) {
			mcp = 1
		}
		run, err := pg.CountApprovalsForRun(ctx, id)
		if err != nil || run != 1-mcp {
			t.Errorf("%s: the run's count = %d, %v; want %d, as isMCPSandboxRow says", c.name, run, err, 1-mcp)
		}
		server, err := pg.CountMCPSandboxApprovals(ctx, id, "files")
		if err != nil || server != mcp {
			t.Errorf("%s: the server's count = %d, %v; want %d, as isMCPSandboxRow says", c.name, server, err, mcp)
		}
	}
}
