// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package approval_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// decideRow decides a fresh egress_domain request with decision and returns its approval.decide row.
func decideRow(t *testing.T, decision types.ApprovalDecision) (uuid.UUID, types.AuditEvent) {
	t.Helper()
	ctx := context.Background()
	st := &fakeStore{}
	ap, err := approval.RequestApproval(ctx, st, newReq(uuid.New(), types.ApprovalEgressDomain, json.RawMessage(`{"host":"registry.npmjs.org"}`)))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if _, err := approval.Decide(ctx, st, ap.ID, types.ActorHuman, decision); err != nil {
		t.Fatalf("decide: %v", err)
	}
	for _, ev := range st.audit {
		if ev.Action == "approval.decide" {
			return ap.ID, ev
		}
	}
	t.Fatal("no approval.decide row")
	return ap.ID, types.AuditEvent{}
}

// TestDecideAuditCarriesDeciderBasis: a decision made under a unit's deciders rule names how the decider
// decided (data.basis) and the rule that applied (data.rule: its unit and kind), beside the usual keys.
func TestDecideAuditCarriesDeciderBasis(t *testing.T) {
	unit := uuid.New()
	var d types.ApprovalDecision
	// The two fields are read from JSON, as any field of the decision would be.
	raw := `{"Basis":"group:eng-leads","Rule":{"unit":"` + unit.String() + `","kind":"egress_domain"}}`
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("decision: %v", err)
	}
	d.State, d.DecidedBy, d.Reason, d.Scope = types.ApprovalApproved, "sub-lead", "ok", types.ScopeOnce
	_, ev := decideRow(t, d)
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("data %s: %v", ev.Data, err)
	}
	if data["basis"] != "group:eng-leads" {
		t.Errorf("data.basis = %v, want group:eng-leads (data %s)", data["basis"], ev.Data)
	}
	rule, _ := data["rule"].(map[string]any)
	if rule["unit"] != unit.String() || rule["kind"] != types.DeciderKindEgressDomain || len(rule) != 2 {
		t.Errorf("data.rule = %v, want {unit: %s, kind: egress_domain} (data %s)", data["rule"], unit, ev.Data)
	}
}

// TestDecidedAuditBytesUnchangedWithoutRule: a decision made with no deciders rule writes the same
// approval.decide data, byte for byte, as a deployment without deciders: no basis and no rule key.
func TestDecidedAuditBytesUnchangedWithoutRule(t *testing.T) {
	id, ev := decideRow(t, types.ApprovalDecision{
		State: types.ApprovalApproved, DecidedBy: "sub-owner", Reason: "ok", Scope: types.ScopeRun,
	})
	want := `{"approval_id":"` + id.String() + `","decision":"APPROVED","decision_scope":"run","host":"registry.npmjs.org","reason":"ok"}`
	if string(ev.Data) != want {
		t.Errorf("approval.decide data =\n %s\nwant\n %s", ev.Data, want)
	}
}
