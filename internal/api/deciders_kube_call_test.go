// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// kubeHold seeds a PENDING held kubectl write on the deciders fixture's run.
func (f *dcFix) kubeHold() types.ApprovalRequest {
	f.t.Helper()
	ap := types.ApprovalRequest{ID: uuid.New(), RunID: f.run.ID, State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
		Kind: types.ApprovalToolCall, RequestedScope: kubeCallScopeJSON(f.t, "PATCH", "", `{"spec":{"replicas":3}}`)}
	f.aps.mu.Lock()
	f.aps.byID[ap.ID] = ap
	f.aps.mu.Unlock()
	return ap
}

// A held kubectl write is the kube_call kind, never mcp_call.
func TestKubeCallDeciderKind(t *testing.T) {
	f := newDCFix(t)
	if got := deciderKindOf(f.kubeHold()); got != types.DeciderKindKubeCall {
		t.Fatalf("deciderKindOf = %q, want kube_call", got)
	}
}

// A unit's kube_call rule that names unit admins and requires a second human drops the run's owner: the
// owner, who decides a held write when no rule governs it, is refused, a unit admin who is not the run's
// author decides it, and an mcp_call rule never governs it.
func TestKubeCallDecidersRuleDropsOwner(t *testing.T) {
	f := newDCFix(t)
	rule := dcRule(types.DeciderKindKubeCall, types.DeciderUnitAdmins)
	rule.SecondHuman = true
	f.st.setRules(f.root.ID, rule)
	ap := f.kubeHold()
	if w := f.decide(dcMember(t, dcOwner), ap, "approve", ""); w.Code == http.StatusOK {
		t.Fatalf("the owner decided a write the rule takes from them: %d %s", w.Code, w.Body)
	}
	if f.row(ap).State != types.ApprovalPending {
		t.Fatalf("state %s, want PENDING", f.row(ap).State)
	}
	f.wantStatus("unit admin", f.decide(dcMember(t, dcAdmin), ap, "approve", ""), http.StatusOK, "", ap)
	if got := f.row(ap).DecisionScope; got != types.ScopeOnce {
		t.Errorf("decision_scope = %q, want once", got)
	}

	g := newDCFix(t)
	g.st.setRules(g.root.ID, dcRule(types.DeciderKindMCPCall, types.DeciderUnitAdmins))
	held := g.kubeHold()
	g.wantStatus("an mcp_call rule's unit admin", g.decide(dcMember(t, dcAdmin), held, "deny", ""), http.StatusNotFound, reasonApprovalNotFound, held)
	g.wantStatus("owner under an mcp_call rule", g.decide(dcMember(t, dcOwner), held, "deny", ""), http.StatusOK, "", held)
}

// A kube_call rule that names unit admins and leaves second_human off still takes the write from the run's
// owner: the second-human gate is not what stops them. Approve and deny both answer 403 not_a_decider, the
// row stays PENDING, the list does not offer it to them, and a unit admin decides it.
func TestKubeCallDecidersRuleDropsOwnerWithoutSecondHuman(t *testing.T) {
	f := newDCFix(t)
	f.st.setRules(f.root.ID, dcRule(types.DeciderKindKubeCall, types.DeciderUnitAdmins))
	ap := f.kubeHold()
	f.wantStatus("owner approve", f.decide(dcMember(t, dcOwner), ap, "approve", ""), http.StatusForbidden, "not_a_decider", ap)
	f.wantStatus("owner deny", f.decide(dcMember(t, dcOwner), ap, "deny", ""), http.StatusForbidden, "not_a_decider", ap)
	if ids := f.inboxIDs(dcMember(t, dcOwner)); slices.Contains(ids, ap.ID.String()) {
		t.Errorf("the owner's decidable list holds the write the rule took from them: %v", ids)
	}
	if ids := f.inboxIDs(dcMember(t, dcAdmin)); !slices.Contains(ids, ap.ID.String()) {
		t.Errorf("the unit admin's decidable list lacks the write: %v", ids)
	}
	f.wantStatus("unit admin", f.decide(dcMember(t, dcAdmin), ap, "approve", ""), http.StatusOK, "", ap)
}
