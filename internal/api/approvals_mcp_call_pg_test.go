// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_MCPCallCaseVariantNeverBecomesAnMCPRow: Postgres JSONB keeps both case variants of a key and
// sorts them, upper case first, so a scope whose last call_sha256 variant was empty on the way in
// reads back with the other one last. The raise refuses such a scope, so no row exists to read
// back; a plain tool_call with case-variant keys of its own is stored and reads back as no MCP row.
func TestPG_MCPCallCaseVariantNeverBecomesAnMCPRow(t *testing.T) {
	e := newRuleRowsEnv(t)
	run := e.newRun()
	ctx := context.Background()
	rows := func() []types.ApprovalRequest {
		t.Helper()
		all, err := pgApprovalService{PG: e.pg}.List(ctx, types.ApprovalPending)
		if err != nil {
			t.Fatal(err)
		}
		var out []types.ApprovalRequest
		for _, ap := range all {
			if ap.RunID == run.id {
				out = append(out, ap)
			}
		}
		return out
	}

	w := do(t, e.srv, http.MethodPost, "/api/v1/internal/approvals", run.tok,
		`{"kind":"tool_call","requested_scope":{"tool":"mcp__corp__read","mcp_server":"corp","arguments":{},"call_sha256":"H","CALL_SHA256":""}}`)
	if w.Code != http.StatusBadRequest || len(rows()) != 0 {
		t.Fatalf("variant raise: %d %s, %d rows; want 400 and no row", w.Code, w.Body.String(), len(rows()))
	}

	w = do(t, e.srv, http.MethodPost, "/api/v1/internal/approvals", run.tok,
		`{"kind":"tool_call","requested_scope":{"tool":"Bash","TOOL":"Read","cmd":"ls"}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("plain raise: %d %s, want 201", w.Code, w.Body.String())
	}
	got := rows()
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	back, err := pgApprovalService{PG: e.pg}.Get(ctx, got[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if isMCPCallRow(back) || types.IsMCPCallScope(back.RequestedScope) {
		t.Fatalf("a plain tool_call read back from Postgres as an MCP call row: %s", back.RequestedScope)
	}
}
