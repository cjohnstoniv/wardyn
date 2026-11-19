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

// GET /approvals gives every held kubectl write a call_text: its body as text, and the empty text when the
// write has none, so the console can tell a bodiless DELETE from a server that sent no text.
func TestListApprovalsCarriesKubeCallText(t *testing.T) {
	const sub = "sub-admin-kubetext"
	for name, tc := range map[string]struct {
		method, body string
		want         string
	}{
		"PATCH with a body":   {"PATCH", `{"spec":{"replicas":3}}`, `"replicas": 3`},
		"DELETE with no body": {"DELETE", "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMDFixture(t, types.ApprovalToolCall, kubeCallScopeJSON(t, tc.method, "", tc.body), nil, sub, nil, nil)
			w := doSSO(t, f.srv, http.MethodGet, "/api/v1/approvals?state=PENDING", ssoSession(t, sub, sub+"@corp.example", oidc.RoleAdmin), "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET /approvals = %d: %s", w.Code, w.Body.String())
			}
			var rows []struct {
				CallText *struct {
					Arguments string `json:"arguments"`
				} `json:"call_text"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &rows); err != nil || len(rows) != 1 {
				t.Fatalf("decode %v rows=%d: %s", err, len(rows), w.Body.String())
			}
			ct := rows[0].CallText
			if ct == nil {
				t.Fatalf("no call_text on a kubectl write row: %s", w.Body.String())
			}
			if tc.body == "" && ct.Arguments != "" {
				t.Errorf("call_text.arguments = %q, want empty", ct.Arguments)
			}
			if tc.want != "" && !strings.Contains(ct.Arguments, tc.want) {
				t.Errorf("call_text.arguments lacks %s:\n%s", tc.want, ct.Arguments)
			}
		})
	}
}
