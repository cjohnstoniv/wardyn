// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mcpCallScopeJSON is the requested_scope the proxy writes for one MCP call of server's rawTool with
// args (canonical), carrying its true key.
func mcpCallScopeJSON(t *testing.T, server, rawTool, args string) json.RawMessage {
	t.Helper()
	key, err := types.MCPCallKey(server, rawTool, json.RawMessage(args), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(types.MCPCallScope{Tool: "mcp__" + server + "__" + rawTool, MCPServer: server,
		MCPHost: server + ".example", Arguments: json.RawMessage(args), CallSHA256: key})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// seedMCPCall puts a pending MCP call row on the fixture's run. withGrant sets a grant id, which no MCP
// call row carries: such a row is neither an MCP call nor an Azure DevOps escalation.
func seedMCPCall(t *testing.T, f *scopeFixture, withGrant bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ap := types.ApprovalRequest{ID: id, RunID: f.runID, Kind: types.ApprovalToolCall,
		RequestedScope: mcpCallScopeJSON(t, "jira", "delete_issue", `{"id":1}`),
		State:          types.ApprovalPending, RequestedAt: time.Now().UTC()}
	if withGrant {
		g := uuid.New()
		ap.GrantID = &g
	}
	f.approval.mu.Lock()
	f.approval.byID[id] = ap
	f.approval.mu.Unlock()
	return id
}

func mcpDecide(t *testing.T, f *scopeFixture, id uuid.UUID, verb string, who *http.Cookie, scope types.ApprovalScope) *httptest.ResponseRecorder {
	t.Helper()
	body := ""
	if scope != "" {
		var exp *time.Time
		if scope == types.ScopeUntil {
			e := time.Now().Add(time.Hour)
			exp = &e
		}
		body = decideBody(t, scope, exp)
	}
	return doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/"+verb, who, body)
}

// An MCP call row is decided once: bodyless or "once" stores "once"; any wider scope is 400 and
// leaves the row PENDING.
func TestDecideMCPCallStoresOnce(t *testing.T) {
	for _, tc := range []struct {
		scope types.ApprovalScope
		want  int
	}{
		{"", http.StatusOK},
		{types.ScopeOnce, http.StatusOK},
		{types.ScopeRun, http.StatusBadRequest},
		{types.ScopeUntil, http.StatusBadRequest},
		{types.ScopeAlways, http.StatusBadRequest},
	} {
		t.Run("scope="+string(tc.scope), func(t *testing.T) {
			f := newScopeFixture(t)
			admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
			id := seedMCPCall(t, f, false)
			w := mcpDecide(t, f, id, "approve", admin, tc.scope)
			if w.Code != tc.want {
				t.Fatalf("status %d body %s, want %d", w.Code, w.Body.String(), tc.want)
			}
			f.approval.mu.Lock()
			got := f.approval.byID[id]
			f.approval.mu.Unlock()
			if tc.want == http.StatusOK {
				if got.State != types.ApprovalApproved || got.DecisionScope != types.ScopeOnce {
					t.Fatalf("row = %s/%q, want APPROVED/once", got.State, got.DecisionScope)
				}
				return
			}
			var body errorBody
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if body.Reason != "decision_scope_invalid_for_kind" || !strings.Contains(body.Error, "MCP call") {
				t.Errorf("refusal = %q %q, want decision_scope_invalid_for_kind with an MCP call sentence", body.Reason, body.Error)
			}
			if got.State != types.ApprovalPending {
				t.Errorf("a refused decision moved the row to %s", got.State)
			}
		})
	}
}

// The run's owner or an administrator decides one MCP call; an unrelated member gets the 404 a row
// that does not exist gets, and a row carrying a grant id is not an MCP call, so the owner cannot.
func TestMCPCallOwnerOrAdminDecides(t *testing.T) {
	owner := func(t *testing.T, f *scopeFixture) *http.Cookie {
		return ssoSession(t, f.memberID, "owner@corp.example", oidc.RoleUser)
	}
	stranger := func(t *testing.T, _ *scopeFixture) *http.Cookie {
		return ssoSession(t, "sub-stranger", "stranger@corp.example", oidc.RoleUser)
	}
	admin := func(t *testing.T, _ *scopeFixture) *http.Cookie {
		return ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	}
	for _, tc := range []struct {
		name      string
		who       func(*testing.T, *scopeFixture) *http.Cookie
		verb      string
		withGrant bool
		want      int
	}{
		{"owner approves", owner, "approve", false, http.StatusOK},
		{"owner denies", owner, "deny", false, http.StatusOK},
		{"admin approves", admin, "approve", false, http.StatusOK},
		{"unrelated member", stranger, "approve", false, http.StatusNotFound},
		{"owner, row with a grant id", owner, "approve", true, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newScopeFixture(t)
			id := seedMCPCall(t, f, tc.withGrant)
			w := mcpDecide(t, f, id, tc.verb, tc.who(t, f), "")
			if w.Code != tc.want {
				t.Fatalf("status %d body %s, want %d", w.Code, w.Body.String(), tc.want)
			}
			st := approvalState(f, id)
			if tc.want != http.StatusOK && st != types.ApprovalPending {
				t.Errorf("a refused decision moved the row to %s", st)
			}
			if tc.want == http.StatusOK && st == types.ApprovalPending {
				t.Errorf("an accepted decision left the row PENDING")
			}
		})
	}
}

// WARDYN_CAPABILITY_SECOND_HUMAN governs an MCP call row: the run's creator is refused on both verbs,
// the row stays PENDING, and an administrator who did not create the run decides it.
func TestMCPCallSecondHumanSwitch(t *testing.T) {
	for _, verb := range []string{"approve", "deny"} {
		t.Run(verb, func(t *testing.T) {
			t.Setenv(envCapabilitySecondHuman, "1")
			f := newScopeFixture(t)
			id := seedMCPCall(t, f, false)
			w := mcpDecide(t, f, id, verb, ssoSession(t, f.memberID, "owner@corp.example", oidc.RoleUser), "")
			if w.Code != http.StatusForbidden {
				t.Fatalf("creator self-%s: status %d body %s, want 403", verb, w.Code, w.Body.String())
			}
			var body errorBody
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if body.Reason != "second_human_required" || !strings.Contains(body.Error, "MCP call") {
				t.Errorf("refusal = %q %q, want second_human_required naming the MCP call", body.Reason, body.Error)
			}
			if st := approvalState(f, id); st != types.ApprovalPending {
				t.Fatalf("state = %s after a refused decision, want PENDING", st)
			}
			w = mcpDecide(t, f, id, verb, ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin), "")
			if w.Code != http.StatusOK {
				t.Fatalf("another administrator: status %d body %s, want 200", w.Code, w.Body.String())
			}
		})
	}
}

// With the switch on and an enforce rule naming the call's tool, a fresh MCP call row stays PENDING:
// the approver returns before it reads a rule, so it writes no decision and no skip row.
func TestApprovalRulesNeverDecideMCPCall(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	f := newRuleFix(t)
	w, created := f.raise(types.ApprovalToolCall, string(mcpCallScopeJSON(t, "github", "create_pull_request_review", `{"pr":7}`)))
	f.answerIsTheCreatedRow(w, created)
	if skips := f.pending(created.ID); len(skips) != 0 {
		t.Fatalf("approval.policy.skip rows = %d, want none: the approver never reads rules for an MCP call", len(skips))
	}
}

// decide() refuses a rule as the decider of an MCP call row, whatever matched.
func TestPolicyDecisionRefusesMCPCall(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	f := newRuleFix(t)
	ap := types.ApprovalRequest{ID: uuid.New(), RunID: f.run.ID, Kind: types.ApprovalToolCall, State: types.ApprovalPending,
		RequestedAt: time.Now(), RequestedScope: mcpCallScopeJSON(t, "github", "create_pull_request_review", `{"pr":7}`)}
	f.aps.byID[ap.ID] = ap
	rule := f.st.rules[0]
	d := &policyDecider{rule: rule, m: types.ApprovalPolicyMatch{RuleID: rule.ID, RuleName: rule.Name, RuleVersion: rule.Version,
		Matched: map[string]string{"tool": "mcp__github__create_pull_request_review", "repo": "org/repo"}}}
	code, body := f.srv.policyDecide(ap.ID, d)
	if code != http.StatusForbidden || !strings.Contains(body, "approval_rule_not_applicable") {
		t.Fatalf("policyDecide = %d %s, want 403 approval_rule_not_applicable", code, body)
	}
	f.pending(ap.ID)
}

// raiseMCPRefused posts a raise on the internal route and asserts a 400 with reason, nothing raised, and an
// auth.fail row naming the reason.
func raiseMCPRefused(t *testing.T, kind types.ApprovalKind, scope, reason string) {
	t.Helper()
	h := newHarness(t)
	tok := h.mintRunToken(t, uuid.New())
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok,
		`{"kind":"`+string(kind)+`","requested_scope":`+scope+`}`)
	var body errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusBadRequest || body.Reason != reason || len(h.approvals.requested) != 0 {
		t.Fatalf("status %d reason %q, %d rows; want 400 %s and nothing raised", w.Code, body.Reason, len(h.approvals.requested), reason)
	}
	if ev := lastAuditEvent(t, h.audit.snapshot(), "auth.fail"); !strings.Contains(string(ev.Data), reason) {
		t.Errorf("auth.fail row %s does not name %s", ev.Data, reason)
	}
}

// The control plane recomputes an MCP call's key from the scope's own fields: a scope whose
// call_sha256 is not that hash, or that cannot be hashed, is refused and recorded.
func TestInternalRaiseRefusesMCPCallHashMismatch(t *testing.T) {
	good := mcpCallScopeJSON(t, "jira", "delete_issue", `{"id":1}`)
	var sc map[string]any
	if err := json.Unmarshal(good, &sc); err != nil {
		t.Fatal(err)
	}
	with := func(edit func(map[string]any)) string {
		m := map[string]any{}
		for k, v := range sc {
			m[k] = v
		}
		edit(m)
		raw, _ := json.Marshal(m)
		return string(raw)
	}
	// Hashed truly, but its display name names another server than the one it is sent to.
	offKey, err := types.MCPCallKey("jira", "mcp__other__delete_issue", json.RawMessage(`{"id":1}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	offServer := with(func(m map[string]any) { m["tool"], m["call_sha256"] = "mcp__other__delete_issue", offKey })
	for name, scope := range map[string]string{
		"other arguments":   with(func(m map[string]any) { m["arguments"] = map[string]any{"id": 2} }),
		"other server":      with(func(m map[string]any) { m["mcp_server"] = "confluence" }),
		"raw tool differs":  with(func(m map[string]any) { m["mcp_tool"] = "delete.issue" }),
		"extra params":      with(func(m map[string]any) { m["extra"] = map[string]any{"task": true} }),
		"tool off server":   offServer,
		"upper-case key":    with(func(m map[string]any) { delete(m, "call_sha256"); m["CALL_SHA256"] = strings.Repeat("0", 64) }),
		"repeated arg key":  strings.Replace(string(good), `"arguments":{"id":1}`, `"arguments":{"id":1,"id":2}`, 1),
		"arguments not obj": with(func(m map[string]any) { m["arguments"] = "x" }),
	} {
		t.Run(name, func(t *testing.T) {
			raiseMCPRefused(t, types.ApprovalToolCall, scope, "mcp_call_scope_invalid")
		})
	}
	t.Run("not a tool_call", func(t *testing.T) {
		raiseMCPRefused(t, types.ApprovalEgressDomain, with(func(m map[string]any) { m["host"] = "jira.example" }), "mcp_call_scope_invalid")
	})
	t.Run("the true key is accepted", func(t *testing.T) {
		h := newHarness(t)
		tok := h.mintRunToken(t, uuid.New())
		w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", tok, `{"kind":"tool_call","requested_scope":`+string(good)+`}`)
		if w.Code != http.StatusCreated || len(h.approvals.requested) != 1 {
			t.Fatalf("status %d body %s, %d rows; want 201 and one row", w.Code, w.Body.String(), len(h.approvals.requested))
		}
	})
}

// An MCP call larger than a person can be shown is refused, never truncated: arguments over 32 KiB,
// and a whole scope over the 64 KiB requested_scope cap.
func TestInternalRaiseRefusesOversizeMCPCall(t *testing.T) {
	scopeWith := func(n int) string {
		big := `{"text":"` + strings.Repeat("a", n) + `"}`
		raw, _ := json.Marshal(types.MCPCallScope{Tool: "mcp__jira__create_issue", MCPServer: "jira",
			Arguments: json.RawMessage(big), CallSHA256: strings.Repeat("0", 64)})
		return string(raw)
	}
	t.Run("arguments over 32 KiB", func(t *testing.T) {
		raiseMCPRefused(t, types.ApprovalToolCall, scopeWith(types.MaxMCPCallBytes+1), "mcp_call_too_large")
	})
	t.Run("scope over 64 KiB", func(t *testing.T) {
		raiseMCPRefused(t, types.ApprovalToolCall, scopeWith(maxSidecarBody+1), "mcp_call_too_large")
	})
}

// The approval.decide row of an MCP call names decision_scope "once", the tool, the server and the
// call hash, so the raise, the decision and the forwarded call join on the audit trail.
func TestDecideAuditCarriesCallSHA(t *testing.T) {
	f := newRuleFix(t)
	scope := mcpCallScopeJSON(t, "jira", "delete_issue", `{"id":1}`)
	_, created := f.raise(types.ApprovalToolCall, string(scope))
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+created.ID.String()+"/approve", admin, "")
	if w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	rows := f.rows("approval.decide")
	if len(rows) != 1 {
		t.Fatalf("approval.decide rows = %d, want 1", len(rows))
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	var sc types.MCPCallScope
	_ = json.Unmarshal(scope, &sc)
	if data["decision_scope"] != "once" || data["tool"] != "mcp__jira__delete_issue" || data["mcp_server"] != "jira" ||
		data["call_sha256"] != sc.CallSHA256 {
		t.Fatalf("approval.decide data = %s", rows[0].Data)
	}
	if ap, _ := f.aps.Get(context.Background(), created.ID); ap.DecisionScope != types.ScopeOnce {
		t.Fatalf("stored decision_scope = %q, want once", ap.DecisionScope)
	}
}

// A scope must spell each of its own keys once, in lower case: encoding/json matches keys in any
// case and keeps the last, so a variant or a repeat could hash one tool while the console shows
// another. And the display name must be the one the raw tool name maps to.
func TestInternalRaiseRefusesAmbiguousMCPCallScope(t *testing.T) {
	del, err := types.MCPCallKey("corp", "delete", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, scope := range map[string]string{
		"case variant":                  `{"tool":"mcp__corp__read","Tool":"mcp__corp__delete","mcp_server":"corp","arguments":{},"call_sha256":"` + del + `"}`,
		"duplicate":                     `{"tool":"mcp__corp__read","tool":"mcp__corp__delete","mcp_server":"corp","arguments":{},"call_sha256":"` + del + `"}`,
		"variant only":                  `{"Tool":"mcp__corp__delete","mcp_server":"corp","arguments":{},"call_sha256":"` + del + `"}`,
		"display is not the raw tool's": `{"tool":"mcp__corp__read","mcp_tool":"delete","mcp_server":"corp","arguments":{},"call_sha256":"` + del + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			raiseMCPRefused(t, types.ApprovalToolCall, scope, "mcp_call_scope_invalid")
		})
	}
	// A raw name the display maps: the display is that mapping, so it passes.
	dot, err := types.MCPCallKey("corp", "delete.issue", json.RawMessage(`{}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", h.mintRunToken(t, uuid.New()),
		`{"kind":"tool_call","requested_scope":{"tool":"mcp__corp__delete_issue","mcp_tool":"delete.issue","mcp_server":"corp","arguments":{},"call_sha256":"`+dot+`"}}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("a display name mapped from its raw name: %d %s, want 201", w.Code, w.Body.String())
	}
}

// The row stores the scope as validated, re-marshalled from its typed form with canonical
// arguments: never the raw bytes, so an unknown key or another spelling never reaches a reader.
func TestInternalRaiseStoresTheValidatedMCPCallScope(t *testing.T) {
	key, err := types.MCPCallKey("jira", "delete_issue", json.RawMessage(`{"b":1,"a":2}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", h.mintRunToken(t, uuid.New()),
		`{"kind":"tool_call","requested_scope":{"tool":"mcp__jira__delete_issue","mcp_server":"jira","arguments":{ "b": 1, "a": 2.0 },"note":"x","call_sha256":"`+key+`"}}`)
	if w.Code != http.StatusCreated || len(h.approvals.requested) != 1 {
		t.Fatalf("status %d body %s, %d rows; want 201 and one row", w.Code, w.Body.String(), len(h.approvals.requested))
	}
	want, _ := json.Marshal(types.MCPCallScope{Tool: "mcp__jira__delete_issue", MCPServer: "jira",
		Arguments: json.RawMessage(`{"a":2,"b":1}`), CallSHA256: key})
	if got := string(h.approvals.requested[0].RequestedScope); got != string(want) {
		t.Fatalf("stored scope\n got %s\nwant %s", got, want)
	}
}

// Any key that folds to call_sha256, mcp_server or mcp_tool puts a scope on the MCP call path,
// whatever its value decodes to: a later variant that clears the hash, a null or a non-string hash,
// or a repeat that breaks the decode is refused, never stored as a plain tool_call.
func TestInternalRaiseMCPCallDiscriminatorKeysAreStrict(t *testing.T) {
	for name, scope := range map[string]string{
		"variant clears the hash": `{"tool":"mcp__corp__read","mcp_server":"corp","arguments":{},"call_sha256":"H","CALL_SHA256":""}`,
		"null hash":               `{"tool":"mcp__corp__read","mcp_server":"corp","arguments":{},"call_sha256":null}`,
		"number hash":             `{"tool":"mcp__corp__read","mcp_server":"corp","arguments":{},"call_sha256":7}`,
		"empty hash":              `{"tool":"mcp__corp__read","mcp_server":"corp","arguments":{},"call_sha256":""}`,
		"repeat breaks decode":    `{"tool":"mcp__corp__read","mcp_server":"corp","arguments":{},"call_sha256":"H","call_sha256":{}}`,
		"server, no hash":         `{"tool":"mcp__corp__read","mcp_server":"corp","arguments":{}}`,
		"variant server only":     `{"tool":"mcp__corp__read","MCP_SERVER":"corp","arguments":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			raiseMCPRefused(t, types.ApprovalToolCall, scope, "mcp_call_scope_invalid")
		})
	}
}
