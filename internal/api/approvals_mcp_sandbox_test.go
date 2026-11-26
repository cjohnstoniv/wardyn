// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mcpHoldScope is the requested_scope of server's hold on host, as mcpSandboxScope writes it.
func mcpHoldScope(host, server string) json.RawMessage {
	return json.RawMessage(`{"host":` + strconv.Quote(host) + `,"mcp_sandbox":` + strconv.Quote(server) + `,"mode":"wait_for_review"}`)
}

// TestMCPSandboxLabelIsExact: the label is the exact key mcpSandboxScope writes; a row the agent raised, a key
// spelled otherwise and another kind carry none, and each caller matches only its own label.
func TestMCPSandboxLabelIsExact(t *testing.T) {
	for _, c := range []struct {
		name  string
		ap    types.ApprovalRequest
		label string
		row   bool
	}{
		{"an MCP hold", types.ApprovalRequest{Kind: types.ApprovalEgressDomain, RequestedScope: mcpHoldScope("a.test", "files")}, "files", true},
		{"the agent's hold", types.ApprovalRequest{Kind: types.ApprovalEgressDomain, RequestedScope: json.RawMessage(`{"host":"a.test"}`)}, "", false},
		{"another spelling", types.ApprovalRequest{Kind: types.ApprovalEgressDomain, RequestedScope: json.RawMessage(`{"host":"a.test","MCP_Sandbox":"files"}`)}, "", false},
		{"another kind", types.ApprovalRequest{Kind: types.ApprovalToolCall, RequestedScope: mcpHoldScope("a.test", "files")}, "files", false},
		{"not an object", types.ApprovalRequest{Kind: types.ApprovalEgressDomain, RequestedScope: json.RawMessage(`"a.test"`)}, "", false},
	} {
		if got := mcpSandboxKeyOf(c.ap); got != c.label {
			t.Errorf("%s: label %q, want %q", c.name, got, c.label)
		}
		if got := isMCPSandboxRow(c.ap); got != c.row {
			t.Errorf("%s: an MCP hold = %v, want %v", c.name, got, c.row)
		}
		if mcpRowMatchesCaller(audit.WithMCPSandbox(context.Background(), "files"), c.ap) != (c.label == "files") ||
			mcpRowMatchesCaller(context.Background(), c.ap) != (c.label == "") {
			t.Errorf("%s: a caller matches a row of another label", c.name)
		}
	}
}

// TestMCPSandboxDecisionScope: an MCP hold is decided with no scope, once, until or run, and any other scope is
// refused; every other row passes the rule untouched.
func TestMCPSandboxDecisionScope(t *testing.T) {
	hold := types.ApprovalRequest{Kind: types.ApprovalEgressDomain, RequestedScope: mcpHoldScope("a.test", "files")}
	agent := types.ApprovalRequest{Kind: types.ApprovalEgressDomain, RequestedScope: json.RawMessage(`{"host":"a.test"}`)}
	for _, scope := range []types.ApprovalScope{"", types.ScopeOnce, types.ScopeUntil, types.ScopeRun, types.ScopeAlways, "forever"} {
		w := httptest.NewRecorder()
		want := scope != types.ScopeAlways && scope != "forever"
		if got := mcpSandboxDecisionRule(w, hold, scope); got != want {
			t.Errorf("MCP hold, scope %q: passes %v, want %v", scope, got, want)
		}
		if !want && (w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonDecisionScopeInvalidForKind) {
			t.Errorf("MCP hold, scope %q: %d %s", scope, w.Code, w.Body.String())
		}
		if !mcpSandboxDecisionRule(httptest.NewRecorder(), agent, scope) {
			t.Errorf("the agent's hold, scope %q: refused by the MCP rule", scope)
		}
	}
}

// seedMCPHold puts a pending MCP hold on the fixture's run.
func (f *scopeFixture) seedMCPHold(host string) uuid.UUID {
	id := uuid.New()
	f.approval.mu.Lock()
	f.approval.byID[id] = types.ApprovalRequest{ID: id, RunID: f.runID, Kind: types.ApprovalEgressDomain,
		RequestedScope: mcpHoldScope(host, "files"), State: types.ApprovalPending, RequestedAt: time.Now().UTC()}
	f.approval.mu.Unlock()
	return id
}

// TestMCPHoldApprovalNeverWritesBack: deciding an MCP hold never writes the host into a workspace. "always",
// either way, is refused and leaves the row PENDING and the workspace's lists as they were; an approve on a
// workspace record run writes no requirement; the four run-bounded scopes decide.
func TestMCPHoldApprovalNeverWritesBack(t *testing.T) {
	const host = "registry.npmjs.org"
	t.Run("always", func(t *testing.T) {
		for _, verb := range []string{"approve", "deny"} {
			f := newScopeFixture(t)
			admin := ssoSession(t, "sub-admin-mcp", "admin@corp.example", oidc.RoleAdmin)
			wsID := f.seedWorkspace(t, []string{"pypi.org"}, []string{"evil.test"})
			id := f.seedMCPHold(host)
			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/"+verb, admin, decideBody(t, types.ScopeAlways, nil))
			if w.Code != http.StatusBadRequest || reasonOf(t, w) != reasonDecisionScopeInvalidForKind {
				t.Fatalf("%s always: %d %s, want 400 %s", verb, w.Code, w.Body.String(), reasonDecisionScopeInvalidForKind)
			}
			approved, denied := f.egressLists(t, wsID)
			if !slices.Equal(approved, []string{"pypi.org"}) || !slices.Equal(denied, []string{"evil.test"}) {
				t.Fatalf("%s always: workspace lists became %v / %v", verb, approved, denied)
			}
			if got := f.approval.byID[id].State; got != types.ApprovalPending {
				t.Fatalf("%s always: row %s, want PENDING", verb, got)
			}
		}
	})
	t.Run("run-bounded scopes decide", func(t *testing.T) {
		exp := time.Now().Add(time.Hour)
		for _, c := range []struct {
			scope types.ApprovalScope
			exp   *time.Time
		}{{"", nil}, {types.ScopeOnce, nil}, {types.ScopeUntil, &exp}, {types.ScopeRun, nil}} {
			f := newScopeFixture(t)
			admin := ssoSession(t, "sub-admin-mcp", "admin@corp.example", oidc.RoleAdmin)
			wsID := f.seedWorkspace(t, nil, nil)
			id := f.seedMCPHold(host)
			body := `{"reason":"test"}`
			if c.scope != "" {
				body = decideBody(t, c.scope, c.exp)
			}
			if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", admin, body); w.Code != http.StatusOK {
				t.Fatalf("scope %q: %d %s", c.scope, w.Code, w.Body.String())
			}
			if approved, denied := f.egressLists(t, wsID); len(approved)+len(denied) != 0 {
				t.Fatalf("scope %q: workspace lists became %v / %v", c.scope, approved, denied)
			}
		}
	})
	t.Run("record run", func(t *testing.T) {
		h := newHarness(t)
		runID, wsID := uuid.New(), uuid.New()
		fake := &recordStore{
			run:             types.AgentRun{ID: runID, WorkspaceID: &wsID, Task: "workspace record", State: types.RunRunning},
			importStateFake: importStateFake{ws: types.Workspace{ID: wsID}},
		}
		cfg := baseTestConfig(h, fake)
		cfg.Approvals = h.approvals
		srv := New(cfg)
		id := uuid.New()
		h.approvals.byID[id] = types.ApprovalRequest{ID: id, RunID: runID, Kind: types.ApprovalEgressDomain,
			RequestedScope: mcpHoldScope(host, "files"), State: types.ApprovalPending}
		if w := do(t, srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", adminToken, ""); w.Code != http.StatusOK {
			t.Fatalf("approve: %d %s", w.Code, w.Body.String())
		}
		if len(fake.ws.Requirements) != 0 {
			t.Fatalf("requirements = %+v, want none", fake.ws.Requirements)
		}
		for _, ev := range h.audit.snapshot() {
			if ev.Action == "workspace.requirement.write" {
				t.Fatalf("a workspace.requirement.write row: %+v", ev)
			}
		}
	})
}

// TestApprovalRuleSkipsMCPSandboxHold: no approval rule matches an MCP hold, though it matches the agent's hold
// for the same host, and a raise-time pass leaves the MCP hold PENDING.
func TestApprovalRuleSkipsMCPSandboxHold(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	f := newRuleFix(t)
	rule := f.st.rules[0]
	ctx := context.Background()
	agent := types.ApprovalRequest{ID: uuid.New(), RunID: f.run.ID, Kind: types.ApprovalEgressDomain, RequestedScope: json.RawMessage(ruleEgressScope)}
	if _, ok, err := f.srv.matchApprovalRule(ctx, rule, agent, f.run); err != nil || !ok {
		t.Fatalf("the agent's hold: match %v, %v; want a match", ok, err)
	}
	id := uuid.New()
	hold := types.ApprovalRequest{ID: id, RunID: f.run.ID, Kind: types.ApprovalEgressDomain, State: types.ApprovalPending,
		RequestedScope: mcpHoldScope("registry.npmjs.org", "files"), RequestedAt: time.Now()}
	if _, ok, err := f.srv.matchApprovalRule(ctx, rule, hold, f.run); err != nil || ok {
		t.Fatalf("the MCP hold: match %v, %v; want none", ok, err)
	}
	f.aps.byID[id] = hold
	f.srv.runApprovalRules(hold)
	if got := f.state(id); got != types.ApprovalPending {
		t.Fatalf("after the rule pass the MCP hold is %s, want PENDING", got)
	}
}

// TestMCPApproveDoesNotUndoAgentDeny: a person's approve of an MCP server's hold, newer than their deny of the
// agent's hold for the same host, does not undo that deny, so a rule still does not decide the agent's next
// hold; and the agent's deny does not bind the MCP server's rows.
func TestMCPApproveDoesNotUndoAgentDeny(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	const host = "registry.npmjs.org"
	f := newRuleFix(t)
	deniedAt, approvedAt := time.Now().Add(-2*time.Minute), time.Now().Add(-time.Minute)
	denyID, approveID := uuid.New(), uuid.New()
	f.aps.byID[denyID] = types.ApprovalRequest{ID: denyID, RunID: f.run.ID, Kind: types.ApprovalEgressDomain, State: types.ApprovalDenied,
		DecisionScope: types.ScopeRun, RequestedScope: json.RawMessage(ruleEgressScope), DecidedAt: &deniedAt, DecidedBy: "sub-person", RequestedAt: deniedAt}
	f.aps.byID[approveID] = types.ApprovalRequest{ID: approveID, RunID: f.run.ID, Kind: types.ApprovalEgressDomain, State: types.ApprovalApproved,
		DecisionScope: types.ScopeRun, RequestedScope: mcpHoldScope(host, "files"), DecidedAt: &approvedAt, DecidedBy: "sub-person", RequestedAt: approvedAt}
	ctx := context.Background()
	if denied, err := f.srv.personDeniedHost(ctx, f.run.ID, host, ""); err != nil || !denied {
		t.Fatalf("the agent's deny: %v, %v; want it to stand", denied, err)
	}
	if denied, err := f.srv.personDeniedHost(ctx, f.run.ID, host, "files"); err != nil || denied {
		t.Fatalf("the MCP server's rows: %v, %v; want no deny", denied, err)
	}
	_, created := f.raise(types.ApprovalEgressDomain, ruleEgressScope)
	skips := f.pending(created.ID)
	if len(skips) != 1 || skipReason(t, skips[0]) != reasonApprovalRuleHostDenied {
		t.Fatalf("skip rows = %+v, want one naming %s", skips, reasonApprovalRuleHostDenied)
	}
}
