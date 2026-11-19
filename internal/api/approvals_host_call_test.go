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

// acceptingOnce is scope as the control plane stores it for a proxy that accepts once.
func acceptingOnce(t *testing.T, scope json.RawMessage) json.RawMessage {
	t.Helper()
	out, ok := hostCallAcceptingOnce(scope)
	if !ok {
		t.Fatalf("not a host rule hold's scope: %s", scope)
	}
	return out
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

// A hold whose proxy accepts once also takes "once", and the row records it; a bodyless decision or "run"
// stores run, and a wider scope is refused with a reason that offers once. A hold without accepts_once refuses
// once (TestHostCallDecisionIsForTheRun): its proxy would read it as the run.
func TestHostCallOnceForAProxyThatAcceptsIt(t *testing.T) {
	for _, tc := range []struct {
		verb  string
		scope types.ApprovalScope
		want  int
		store types.ApprovalScope
	}{
		{"approve", "", http.StatusOK, types.ScopeRun},
		{"approve", types.ScopeRun, http.StatusOK, types.ScopeRun},
		{"approve", types.ScopeOnce, http.StatusOK, types.ScopeOnce},
		{"deny", types.ScopeOnce, http.StatusOK, types.ScopeOnce},
		{"approve", types.ScopeUntil, http.StatusBadRequest, ""},
		{"approve", types.ScopeAlways, http.StatusBadRequest, ""},
	} {
		t.Run(tc.verb+"/scope="+string(tc.scope), func(t *testing.T) {
			f := newScopeFixture(t)
			id := seedHostCall(t, f, false)
			f.approval.mu.Lock()
			ap := f.approval.byID[id]
			ap.RequestedScope = acceptingOnce(t, ap.RequestedScope)
			f.approval.byID[id] = ap
			f.approval.mu.Unlock()
			w := mcpDecide(t, f, id, tc.verb, ssoSession(t, f.memberID, "owner@corp.example", oidc.RoleUser), tc.scope)
			if w.Code != tc.want {
				t.Fatalf("status %d body %s, want %d", w.Code, w.Body.String(), tc.want)
			}
			f.approval.mu.Lock()
			got := f.approval.byID[id]
			f.approval.mu.Unlock()
			if tc.want == http.StatusOK {
				if got.State == types.ApprovalPending || got.DecisionScope != tc.store {
					t.Fatalf("row = %s/%q, want decided/%q", got.State, got.DecisionScope, tc.store)
				}
				return
			}
			var body errorBody
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if body.Reason != "decision_scope_invalid_for_kind" || !strings.Contains(body.Error, `"once"`) || got.State != types.ApprovalPending {
				t.Errorf("refusal = %q %q, state %s", body.Reason, body.Error, got.State)
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

// Either four-eyes switch governs a host rule hold, which is both an egress decision and one exact call.
func TestHostCallSecondHumanGate(t *testing.T) {
	ap := types.ApprovalRequest{Kind: types.ApprovalToolCall, RequestedScope: hostCallScopeJSON(t, "POST", "/admin")}
	for _, tc := range []struct {
		egress, capability string
		name               string
		on                 bool
	}{
		{"", "", envEgressSecondHuman, false},
		{"1", "", envEgressSecondHuman, true},
		{"", "1", envCapabilitySecondHuman, true},
		{"1", "1", envCapabilitySecondHuman, true},
	} {
		t.Setenv(envEgressSecondHuman, tc.egress)
		t.Setenv(envCapabilitySecondHuman, tc.capability)
		if name, on := secondHumanGate(ap); name != tc.name || on != tc.on {
			t.Errorf("egress=%q capability=%q: secondHumanGate = %q %v, want %q %v", tc.egress, tc.capability, name, on, tc.name, tc.on)
		}
	}
}

// Either switch refuses the run's creator a host rule hold of their own run.
func TestHostCallSecondHumanSwitch(t *testing.T) {
	for _, sw := range []string{envCapabilitySecondHuman, envEgressSecondHuman} {
		t.Run(sw, func(t *testing.T) { hostCallSecondHumanRefusesOwner(t, sw) })
	}
}

func hostCallSecondHumanRefusesOwner(t *testing.T, sw string) {
	t.Setenv(sw, "1")
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
		"accepts_once":     with(func(m map[string]any) { m["accepts_once"] = true }),
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

// A raise saying its proxy accepts once stores the validated scope stamped accepts_once, and accepts_once on
// any other raise is refused. A scope that carries the stamp itself is refused (TestHostCallScopeHashMismatchRefused).
func TestInternalRaiseStampsAcceptsOnce(t *testing.T) {
	scope := hostCallScopeJSON(t, "DELETE", "/admin/users/7")
	h := newHarness(t)
	w := do(t, h.srv, http.MethodPost, "/api/v1/internal/approvals", h.mintRunToken(t, uuid.New()),
		`{"kind":"tool_call","requested_scope":`+string(scope)+`,"accepts_once":true}`)
	if w.Code != http.StatusCreated || len(h.approvals.requested) != 1 {
		t.Fatalf("status %d body %s, %d rows; want 201 and one row", w.Code, w.Body.String(), len(h.approvals.requested))
	}
	stored := h.approvals.requested[0]
	if got, want := string(stored.RequestedScope), string(acceptingOnce(t, scope)); got != want || !hostCallAcceptsOnce(stored) {
		t.Fatalf("stored scope\n got %s\nwant %s", got, want)
	}

	other := newHarness(t)
	w = do(t, other.srv, http.MethodPost, "/api/v1/internal/approvals", other.mintRunToken(t, uuid.New()),
		`{"kind":"egress_domain","requested_scope":{"host":"api.example.com"},"accepts_once":true}`)
	if w.Code != http.StatusBadRequest || wireReason(t, w) != reasonInternalApprovalRequestInvalid || len(other.approvals.requested) != 0 {
		t.Fatalf("accepts_once on an egress_domain raise: %d %s, %d rows", w.Code, w.Body.String(), len(other.approvals.requested))
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

// The adopter's rule, an egress_domain rule naming the owner and unit admins capped at max_scope once, decides a
// hold whose proxy accepts once: a bodyless approve or deny is stored as once, as for an egress_domain request,
// "once" is decided, and "run" is refused (decision_scope_above_rule).
func TestHostCallDecidersOnceCapDecidesOnce(t *testing.T) {
	f := newDCFix(t)
	rule := dcRule(types.DeciderKindEgressDomain, types.DeciderOwner, types.DeciderUnitAdmins)
	rule.MaxScope = types.ScopeOnce
	f.st.setRules(f.root.ID, rule)
	for _, tc := range []struct {
		name, verb, body string
		code             int
		reason           string
	}{
		{"bodyless approve", "approve", "", http.StatusOK, ""},
		{"bodyless deny", "deny", "", http.StatusOK, ""},
		{"once", "approve", `{"reason":"t","decision_scope":"once"}`, http.StatusOK, ""},
		{"run", "approve", `{"reason":"t","decision_scope":"run"}`, http.StatusForbidden, "decision_scope_above_rule"},
	} {
		ap := f.hostHold()
		f.aps.mu.Lock()
		ap.RequestedScope = acceptingOnce(t, ap.RequestedScope)
		f.aps.byID[ap.ID] = ap
		f.aps.mu.Unlock()
		f.wantStatus(tc.name, f.decide(dcMember(t, dcAdmin), ap, tc.verb, tc.body), tc.code, tc.reason, ap)
		if got := f.row(ap).DecisionScope; tc.code == http.StatusOK && got != types.ScopeOnce {
			t.Errorf("%s: decision_scope = %q, want once", tc.name, got)
		}
	}
}
