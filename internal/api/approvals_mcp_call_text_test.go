// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GET /approvals gives an MCP call row a call_text: the canonical arguments and extra params as text,
// so the console and the CLI show and copy the exact numbers the approval binds without parsing them.
// A row that is not an MCP call has none.
func TestListApprovalsCarriesMCPCallText(t *testing.T) {
	const sub = "sub-admin-calltext"
	args := `{"ids":[9007199254740993,9007199254740995],"price":1.0000000000000001}`
	for name, tc := range map[string]struct {
		kind  types.ApprovalKind
		scope json.RawMessage
		want  bool
	}{
		"MCP call row":        {types.ApprovalToolCall, mcpCallScopeJSON(t, "jira", "delete_issue", args), true},
		"plain tool_call row": {types.ApprovalToolCall, json.RawMessage(`{"tool":"Bash","cmd":"ls"}`), false},
		"egress_domain row":   {types.ApprovalEgressDomain, json.RawMessage(`{"host":"a.example"}`), false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMDFixture(t, tc.kind, tc.scope, nil, sub, nil, nil)
			w := doSSO(t, f.srv, http.MethodGet, "/api/v1/approvals?state=PENDING", ssoSession(t, sub, sub+"@corp.example", oidc.RoleAdmin), "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET /approvals = %d: %s", w.Code, w.Body.String())
			}
			var rows []struct {
				CallText *struct {
					Arguments string `json:"arguments"`
					Extra     string `json:"extra"`
				} `json:"call_text"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 1 {
				t.Fatalf("decode %v rows=%d: %s", err, len(rows), w.Body.String())
			}
			ct := rows[0].CallText
			if !tc.want {
				if ct != nil {
					t.Fatalf("call_text = %+v on a row that is not an MCP call", ct)
				}
				return
			}
			if ct == nil {
				t.Fatalf("no call_text on an MCP call row: %s", w.Body.String())
			}
			for _, digits := range []string{"9007199254740993", "9007199254740995", "1.0000000000000001"} {
				if !strings.Contains(ct.Arguments, digits) {
					t.Errorf("call_text.arguments lost %s:\n%s", digits, ct.Arguments)
				}
			}
		})
	}
}
