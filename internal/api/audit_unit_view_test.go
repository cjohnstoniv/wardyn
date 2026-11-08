// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAuditUnitViewAllowListCensus pins the unit view's data allow list: every key it serves is a key
// docs/AUDIT-ACTIONS.md documents, and none is a key that holds what only the run's owner or the
// security tier may read. A key missing from the list is withheld, so the list only ever grows on purpose.
func TestAuditUnitViewAllowListCensus(t *testing.T) {
	doc, err := os.ReadFile("../../docs/AUDIT-ACTIONS.md")
	if err != nil {
		t.Fatalf("read the audit action docs: %v", err)
	}
	if len(unitViewDataKeys) == 0 {
		t.Fatal("the unit view serves no data key")
	}
	for _, k := range unitViewDataKeys {
		if !bytes.Contains(doc, []byte("`"+k+"`")) {
			t.Errorf("unit view data key %q is not documented in docs/AUDIT-ACTIONS.md", k)
		}
	}
	never := []string{"policy_source", "personal_walls", "wall_preset", "restriction_ids", "overrides_diff",
		"contact", "spec", "inline_policy", "secret_name", "error", "detail", "owner", "subject"}
	for _, k := range never {
		if slices.Contains(unitViewDataKeys, k) {
			t.Errorf("unit view data key %q holds what a unit admin must not read", k)
		}
	}
}

// TestAuditUnitViewKeepsApprovalDecision pins that an approval.decide row keeps its decision in the unit
// view, so a unit admin reads approved or denied and not only the row's outcome. The value is the
// approval's closed state code, which the view's code shape serves; the reason beside it is a sentence
// and stays out.
func TestAuditUnitViewKeepsApprovalDecision(t *testing.T) {
	for _, state := range []types.ApprovalState{types.ApprovalApproved, types.ApprovalDenied} {
		ev := types.AuditEvent{Action: "approval.decide", Outcome: "success",
			Data: json.RawMessage(`{"approval_id":"9f1c2b7e-0d3a-4c55-9a51-3c1f6b2e8d10","decision":"` + string(state) +
				`","decision_scope":"run","reason":"looks fine to me"}`)}
		var got map[string]any
		if err := json.Unmarshal(unitView(ev).Data, &got); err != nil {
			t.Fatalf("%s: unit view data: %v", state, err)
		}
		if got["decision"] != string(state) {
			t.Errorf("%s: unit view decision = %v, want %q (data %v)", state, got["decision"], state, got)
		}
		if _, ok := got["reason"]; ok {
			t.Errorf("%s: unit view served the decision's free-text reason: %v", state, got)
		}
	}
	for _, state := range types.ApprovalStates {
		if !unitViewCode.MatchString(string(state)) {
			t.Errorf("approval state %q is not a code the unit view serves", state)
		}
	}
}
