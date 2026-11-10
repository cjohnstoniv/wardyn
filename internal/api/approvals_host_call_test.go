// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// hostCallScopeJSON is the requested_scope the proxy writes for one host rule hold, carrying its true key.
func hostCallScopeJSON(t *testing.T, method, path string) json.RawMessage {
	t.Helper()
	sc := types.HostCallScope{Host: "api.example.com", Method: method, Path: path, HoldSeconds: 90}
	sc.CallSHA256 = sc.ComputeKey()
	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func seedHostCall(t *testing.T, f *scopeFixture, withGrant bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ap := types.ApprovalRequest{ID: id, RunID: f.runID, Kind: types.ApprovalToolCall,
		RequestedScope: hostCallScopeJSON(t, "POST", "/admin"), State: types.ApprovalPending, RequestedAt: time.Now().UTC()}
	if withGrant {
		g := uuid.New()
		ap.GrantID = &g
	}
	f.approval.mu.Lock()
	f.approval.byID[id] = ap
	f.approval.mu.Unlock()
	return id
}

// A host rule hold is its own row: an exact call row, never an MCP call or kubectl write row, and the
// tally key agrees.
func TestHostCallRowIsItsOwnRow(t *testing.T) {
	ap := types.ApprovalRequest{Kind: types.ApprovalToolCall, RequestedScope: hostCallScopeJSON(t, "POST", "/admin")}
	if !isHostCallRow(ap) || isMCPCallRow(ap) || isKubeCallRow(ap) || !isExactCallRow(ap) {
		t.Fatalf("host %v mcp %v kube %v exact %v", isHostCallRow(ap), isMCPCallRow(ap), isKubeCallRow(ap), isExactCallRow(ap))
	}
	if got := approval.TallyKey(ap); got != approval.TallyToolCallHost {
		t.Fatalf("TallyKey = %q", got)
	}
	if name, _ := secondHumanGate(ap); name != envCapabilitySecondHuman {
		t.Fatalf("secondHumanGate = %q", name)
	}
	g := uuid.New()
	ap.GrantID = &g
	if isHostCallRow(ap) || approval.TallyKey(ap) == approval.TallyToolCallHost {
		t.Fatal("a row with a grant id is not a host rule hold")
	}
	egress := types.ApprovalRequest{Kind: types.ApprovalEgressDomain, RequestedScope: json.RawMessage(`{"host":"api.example.com"}`)}
	if isHostCallRow(egress) || isExactCallRow(egress) {
		t.Fatal("an egress_domain row is not a host rule hold")
	}
}

// A host rule hold is decided for the run, as the proxy keeps it: a bodyless decision or "run" stores run,
// any other scope is refused.
func TestHostCallDecisionIsForTheRun(t *testing.T) {
	for _, tc := range []struct {
		scope types.ApprovalScope
		want  int
	}{
		{"", http.StatusOK},
		{types.ScopeRun, http.StatusOK},
		{types.ScopeOnce, http.StatusBadRequest},
		{types.ScopeUntil, http.StatusBadRequest},
		{types.ScopeAlways, http.StatusBadRequest},
	} {
		t.Run("scope="+string(tc.scope), func(t *testing.T) {
			f := newScopeFixture(t)
			id := seedHostCall(t, f, false)
			w := mcpDecide(t, f, id, "approve", ssoSession(t, f.memberID, "owner@corp.example", oidc.RoleUser), tc.scope)
			if w.Code != tc.want {
				t.Fatalf("status %d body %s, want %d", w.Code, w.Body.String(), tc.want)
			}
			f.approval.mu.Lock()
			got := f.approval.byID[id]
			f.approval.mu.Unlock()
			if tc.want == http.StatusOK {
				if got.State != types.ApprovalApproved || got.DecisionScope != types.ScopeRun {
					t.Fatalf("row = %s/%q, want APPROVED/run", got.State, got.DecisionScope)
				}
				return
			}
			var body errorBody
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if body.Reason != "decision_scope_invalid_for_kind" || !strings.Contains(body.Error, "host request") {
				t.Errorf("refusal = %q %q", body.Reason, body.Error)
			}
			if got.State != types.ApprovalPending {
				t.Errorf("a refused decision moved the row to %s", got.State)
			}
		})
	}
}

// The run's owner or an administrator decides one host rule hold; an unrelated member gets the 404 a row
// that does not exist gets, and a row carrying a grant id is not one.
func TestHostCallOwnerDecides(t *testing.T) {
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
			id := seedHostCall(t, f, tc.withGrant)
			w := mcpDecide(t, f, id, tc.verb, tc.who(t, f), "")
			if w.Code != tc.want {
				t.Fatalf("status %d body %s, want %d", w.Code, w.Body.String(), tc.want)
			}
			if st := approvalState(f, id); (tc.want == http.StatusOK) == (st == types.ApprovalPending) {
				t.Errorf("state %s after status %d", st, w.Code)
			}
		})
	}
}

// WARDYN_CAPABILITY_SECOND_HUMAN governs a host rule hold: the run's creator is refused.
func TestHostCallSecondHumanSwitch(t *testing.T) {
	t.Setenv(envCapabilitySecondHuman, "1")
	f := newScopeFixture(t)
	id := seedHostCall(t, f, false)
	w := mcpDecide(t, f, id, "approve", ssoSession(t, f.memberID, "owner@corp.example", oidc.RoleUser), "")
	var body errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusForbidden || body.Reason != "second_human_required" {
		t.Fatalf("status %d %q %q, want 403 second_human_required", w.Code, body.Reason, body.Error)
	}
	if st := approvalState(f, id); st != types.ApprovalPending {
		t.Fatalf("state = %s, want PENDING", st)
	}
}

// An enforce approval rule never decides a host rule hold (the policy-decide gates read isExactCallRow).
func TestApprovalRulesNeverDecideHostCall(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	f := newRuleFix(t)
	w, created := f.raise(types.ApprovalToolCall, string(hostCallScopeJSON(t, "POST", "/admin")))
	f.answerIsTheCreatedRow(w, created)
	if skips := f.pending(created.ID); len(skips) != 0 {
		t.Fatalf("approval.policy.skip rows = %d, want none", len(skips))
	}
}

// The control plane recomputes a hold's key from the scope's own fields: a call_sha256 that is not that
// hash, a field not written the way a rule names it, or a key that is not the scope's own, is refused.
func TestHostCallScopeHashMismatchRefused(t *testing.T) {
	good := hostCallScopeJSON(t, "POST", "/admin")
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
		b, _ := json.Marshal(m)
		return string(b)
	}
	for name, scope := range map[string]string{
		"another host":     with(func(m map[string]any) { m["host"] = "api.example.org" }),
		"another method":   with(func(m map[string]any) { m["method"] = "GET" }),
		"another path":     with(func(m map[string]any) { m["path"] = "/admin/x" }),
		"no hash":          with(func(m map[string]any) { m["call_sha256"] = "" }),
		"an MCP key too":   with(func(m map[string]any) { m["mcp_server"] = "jira" }),
		"an unknown key":   with(func(m map[string]any) { m["query"] = "a=1" }),
		"a host with port": with(func(m map[string]any) { m["host"] = "api.example.com:443" }),
		"a dot segment":    with(func(m map[string]any) { m["path"] = "/x/../admin" }),
		"case variant":     strings.Replace(string(good), `"host"`, `"Host"`, 1),
		"repeated key":     strings.Replace(string(good), `"host":"api.example.com"`, `"host":"api.example.com","host":"other.example"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			raiseMCPRefused(t, types.ApprovalToolCall, scope, "host_call_scope_invalid")
		})
	}
}

// A hold's scope is refused on any kind but tool_call; an egress_domain scope naming only a host passes on.
func TestHostCallScopeOnOtherKindRefused(t *testing.T) {
	for _, kind := range []types.ApprovalKind{types.ApprovalEgressDomain, types.ApprovalPushContent} {
		raiseMCPRefused(t, kind, string(hostCallScopeJSON(t, "POST", "/admin")), "host_call_scope_invalid")
	}
	if hostCallKeysPresent(json.RawMessage(`{"host":"api.example.com","mode":"wait_for_review"}`)) {
		t.Fatal("an egress_domain scope picked the host call path")
	}
}

// The row stores the scope as validated, re-marshalled from its typed form.
func TestInternalRaiseStoresTheValidatedHostCallScope(t *testing.T) {
	scope := hostCallScopeJSON(t, "DELETE", "/admin/users/7")
	h := newHarness(t)
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", h.mintRunToken(t, uuid.New()),
		`{"kind":"tool_call","requested_scope":`+string(scope)+`}`)
	if w.Code != http.StatusCreated || len(h.approvals.requested) != 1 {
		t.Fatalf("status %d body %s, %d rows; want 201 and one row", w.Code, w.Body.String(), len(h.approvals.requested))
	}
	if got := string(h.approvals.requested[0].RequestedScope); got != string(scope) {
		t.Fatalf("stored scope\n got %s\nwant %s", got, scope)
	}
}

// hostHold seeds a PENDING host rule hold on the deciders fixture's run.
func (f *dcFix) hostHold() types.ApprovalRequest {
	f.t.Helper()
	ap := types.ApprovalRequest{ID: uuid.New(), RunID: f.run.ID, State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
		Kind: types.ApprovalToolCall, RequestedScope: hostCallScopeJSON(f.t, "POST", "/admin")}
	f.aps.mu.Lock()
	f.aps.byID[ap.ID] = ap
	f.aps.mu.Unlock()
	return ap
}

// A host rule hold is governed by the unit's egress_domain rule, never a kube_call or mcp_call rule: under an
// egress_domain rule naming unit admins with a second human, the owner is refused and a unit admin decides it.
func TestHostCallDecidersEgressRule(t *testing.T) {
	f := newDCFix(t)
	if got := deciderKindOf(f.hostHold()); got != types.DeciderKindEgressDomain {
		t.Fatalf("deciderKindOf = %q, want egress_domain", got)
	}
	rule := dcRule(types.DeciderKindEgressDomain, types.DeciderUnitAdmins)
	rule.SecondHuman = true
	f.st.setRules(f.root.ID, rule)
	ap := f.hostHold()
	if w := f.decide(dcMember(t, dcOwner), ap, "approve", ""); w.Code == http.StatusOK {
		t.Fatalf("the owner decided a hold the rule takes from them: %d %s", w.Code, w.Body)
	}
	if f.row(ap).State != types.ApprovalPending {
		t.Fatalf("state %s, want PENDING", f.row(ap).State)
	}
	f.wantStatus("unit admin", f.decide(dcMember(t, dcAdmin), ap, "approve", ""), http.StatusOK, "", ap)
	if got := f.row(ap).DecisionScope; got != types.ScopeRun {
		t.Errorf("decision_scope = %q, want run", got)
	}

	g := newDCFix(t)
	g.st.setRules(g.root.ID, dcRule(types.DeciderKindKubeCall, types.DeciderUnitAdmins))
	held := g.hostHold()
	g.wantStatus("a kube_call rule's unit admin", g.decide(dcMember(t, dcAdmin), held, "deny", ""), http.StatusNotFound, reasonApprovalNotFound, held)
	g.wantStatus("owner under a kube_call rule", g.decide(dcMember(t, dcOwner), held, "deny", ""), http.StatusOK, "", held)
}

// A unit's egress_domain rule capped at max_scope once refuses a host rule hold's decision, which reaches the
// rest of the run (decision_scope_above_rule), and the row stays pending.
func TestHostCallDecidersOnceCapRefuses(t *testing.T) {
	f := newDCFix(t)
	rule := dcRule(types.DeciderKindEgressDomain, types.DeciderUnitAdmins)
	rule.MaxScope = types.ScopeOnce
	f.st.setRules(f.root.ID, rule)
	ap := f.hostHold()
	f.wantStatus("unit admin under a once cap", f.decide(dcMember(t, dcAdmin), ap, "approve", ""), http.StatusForbidden, "decision_scope_above_rule", ap)
	if f.row(ap).State != types.ApprovalPending {
		t.Fatalf("state %s, want PENDING", f.row(ap).State)
	}
}
