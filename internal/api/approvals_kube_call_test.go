// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
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

const kcTestPath = "/apis/apps/v1/namespaces/team-a/deployments/web"

// kubeCallScopeJSON is the requested_scope the proxy writes for one held kubectl write of the web
// deployment, carrying its true key.
func kubeCallScopeJSON(t *testing.T, method, query, body string) json.RawMessage {
	t.Helper()
	sc := types.KubeCallScope{Cluster: "k8s.example.com", Namespace: "team-a", Method: method, Path: kcTestPath, Query: query,
		HoldSeconds: 90}
	if body != "" {
		sc.MediaType, sc.Body = "application/merge-patch+json", json.RawMessage(body)
	}
	key, err := sc.ComputeKey()
	if err != nil {
		t.Fatal(err)
	}
	sc.CallSHA256 = key
	raw, err := json.Marshal(sc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func seedKubeCall(t *testing.T, f *scopeFixture, withGrant bool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ap := types.ApprovalRequest{ID: id, RunID: f.runID, Kind: types.ApprovalToolCall,
		RequestedScope: kubeCallScopeJSON(t, "PATCH", "", `{"spec":{"replicas":3}}`),
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

// A held kubectl write is its own row: never an MCP call row, and the tally key agrees.
func TestKubeCallRowIsNotMCPCallRow(t *testing.T) {
	ap := types.ApprovalRequest{Kind: types.ApprovalToolCall, RequestedScope: kubeCallScopeJSON(t, "PATCH", "", `{"spec":{"replicas":3}}`)}
	if !isKubeCallRow(ap) || isMCPCallRow(ap) || !isExactCallRow(ap) {
		t.Fatalf("kube %v mcp %v exact %v", isKubeCallRow(ap), isMCPCallRow(ap), isExactCallRow(ap))
	}
	if got := approval.TallyKey(ap); got != approval.TallyToolCallKube {
		t.Fatalf("TallyKey = %q", got)
	}
	if name, _ := secondHumanGate(ap); name != envCapabilitySecondHuman {
		t.Fatalf("secondHumanGate = %q", name)
	}
	g := uuid.New()
	ap.GrantID = &g
	if isKubeCallRow(ap) || approval.TallyKey(ap) == approval.TallyToolCallKube {
		t.Fatal("a row with a grant id is not a held kubectl write")
	}
	rows := []types.ApprovalRequest{{Kind: types.ApprovalToolCall, RequestedScope: kubeCallScopeJSON(t, "DELETE", "", "")}}
	projectCallText(rows)
	// Its own call text, never the MCP arm's: a write with no body is the empty text, with no extra.
	if ct := rows[0].CallText; ct == nil || ct.Arguments != "" || ct.Extra != "" {
		t.Fatalf("a bodiless kubectl write's call text = %+v, want the empty body", ct)
	}
}

// A held kubectl write is decided once: bodyless or "once" stores "once"; any wider scope is 400 and
// leaves the row PENDING.
func TestKubeCallDecisionAlwaysOnce(t *testing.T) {
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
			id := seedKubeCall(t, f, false)
			w := mcpDecide(t, f, id, "approve", ssoSession(t, f.memberID, "owner@corp.example", oidc.RoleUser), tc.scope)
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
			if body.Reason != "decision_scope_invalid_for_kind" || !strings.Contains(body.Error, "kubectl") {
				t.Errorf("refusal = %q %q", body.Reason, body.Error)
			}
			if got.State != types.ApprovalPending {
				t.Errorf("a refused decision moved the row to %s", got.State)
			}
		})
	}
}

// The run's owner or an administrator decides one held write; an unrelated member gets the 404 a row
// that does not exist gets, and a row carrying a grant id is not one, so the owner cannot.
func TestKubeCallOwnerDecides(t *testing.T) {
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
			id := seedKubeCall(t, f, tc.withGrant)
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

// WARDYN_CAPABILITY_SECOND_HUMAN governs a held write: the run's creator is refused, naming the change.
func TestKubeCallSecondHumanSwitch(t *testing.T) {
	t.Setenv(envCapabilitySecondHuman, "1")
	f := newScopeFixture(t)
	id := seedKubeCall(t, f, false)
	w := mcpDecide(t, f, id, "approve", ssoSession(t, f.memberID, "owner@corp.example", oidc.RoleUser), "")
	var body errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusForbidden || body.Reason != "second_human_required" || !strings.Contains(body.Error, "kubectl change") {
		t.Fatalf("status %d %q %q, want 403 second_human_required naming the kubectl change", w.Code, body.Reason, body.Error)
	}
	if st := approvalState(f, id); st != types.ApprovalPending {
		t.Fatalf("state = %s, want PENDING", st)
	}
}

// An enforce approval rule never decides a held kubectl write.
func TestApprovalRulesNeverDecideKubeCall(t *testing.T) {
	t.Setenv(envApprovalRules, approvalRulesOn)
	f := newRuleFix(t)
	w, created := f.raise(types.ApprovalToolCall, string(kubeCallScopeJSON(t, "PATCH", "", `{"spec":{"replicas":3}}`)))
	f.answerIsTheCreatedRow(w, created)
	if skips := f.pending(created.ID); len(skips) != 0 {
		t.Fatalf("approval.policy.skip rows = %d, want none", len(skips))
	}
}

// The control plane recomputes a held write's key from the scope's own fields: a call_sha256 that is
// not that hash, a field a person could not read exactly, or a key that is not the scope's own, is
// refused and recorded.
func TestKubeCallScopeHashMismatchRefused(t *testing.T) {
	good := kubeCallScopeJSON(t, "PATCH", "dryRun=All", `{"spec":{"replicas":3}}`)
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
		"another query":      with(func(m map[string]any) { m["query"] = "" }),
		"another media type": with(func(m map[string]any) { m["media_type"] = "application/strategic-merge-patch+json" }),
		"another body":       with(func(m map[string]any) { m["body"] = map[string]any{"spec": map[string]any{"replicas": 30}} }),
		"another path":       with(func(m map[string]any) { m["path"] = "/apis/apps/v1/namespaces/team-a/deployments/api" }),
		"another namespace":  with(func(m map[string]any) { m["namespace"] = "team-b" }),
		"no hash":            with(func(m map[string]any) { m["call_sha256"] = "" }),
		"an MCP key too":     with(func(m map[string]any) { m["mcp_server"] = "jira" }),
		"an unknown key":     with(func(m map[string]any) { m["note"] = "x" }),
		"a non-JSON type":    with(func(m map[string]any) { m["media_type"] = "application/yaml" }),
		"a GET":              with(func(m map[string]any) { m["method"] = "GET" }),
		"case variant":       strings.Replace(string(good), `"cluster"`, `"Cluster"`, 1),
		"repeated key":       strings.Replace(string(good), `"cluster":"k8s.example.com"`, `"cluster":"k8s.example.com","cluster":"other.example"`, 1),
		"a query in the path": with(func(m map[string]any) {
			m["path"] = kcTestPath + "?dryRun=All"
		}),
	} {
		t.Run(name, func(t *testing.T) {
			raiseMCPRefused(t, types.ApprovalToolCall, scope, "kube_call_scope_invalid")
		})
	}
}

// A held write's scope is refused on any kind but tool_call.
func TestKubeCallScopeOnOtherKindRefused(t *testing.T) {
	for _, kind := range []types.ApprovalKind{types.ApprovalEgressDomain, types.ApprovalPushContent} {
		raiseMCPRefused(t, kind, string(kubeCallScopeJSON(t, "PATCH", "", `{"spec":{"replicas":3}}`)), "kube_call_scope_invalid")
	}
}

// The row stores the scope as validated, re-marshalled from its typed form.
func TestInternalRaiseStoresTheValidatedKubeCallScope(t *testing.T) {
	scope := kubeCallScopeJSON(t, "DELETE", "labelSelector=app%3Dweb", "")
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

// approval.decide names the held write's cluster, namespace, method, path and hash.
func TestDecideAuditCarriesKubeCall(t *testing.T) {
	f := newRuleFix(t)
	scope := kubeCallScopeJSON(t, "PATCH", "", `{"spec":{"replicas":3}}`)
	_, created := f.raise(types.ApprovalToolCall, string(scope))
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+created.ID.String()+"/approve", admin, ""); w.Code != http.StatusOK {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	rows := f.rows("approval.decide")
	if len(rows) != 1 {
		t.Fatalf("approval.decide rows = %d, want 1", len(rows))
	}
	var data map[string]any
	_ = json.Unmarshal(rows[0].Data, &data)
	var sc types.KubeCallScope
	_ = json.Unmarshal(scope, &sc)
	if data["decision_scope"] != "once" || data["cluster"] != "k8s.example.com" || data["namespace"] != "team-a" ||
		data["method"] != "PATCH" || data["path"] != kcTestPath || data["call_sha256"] != sc.CallSHA256 || data["tool"] != nil {
		t.Fatalf("approval.decide data = %s", rows[0].Data)
	}
	if ap, _ := f.aps.Get(context.Background(), created.ID); ap.DecisionScope != types.ScopeOnce {
		t.Fatalf("stored decision_scope = %q, want once", ap.DecisionScope)
	}
}
