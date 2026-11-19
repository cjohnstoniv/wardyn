// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_DecideApprovalKeepsTheDecisionBasis: a decision made under a unit's deciders rule keeps its basis and the
// rule's unit on the approval row, read back by the decide itself, by GetApproval and by ListApprovals. A decision
// no rule governs keeps neither, so its row reads as before.
func TestPG_DecideApprovalKeepsTheDecisionBasis(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning))
	unit := uuid.New()
	basis := "unit_admin:" + unit.String()

	ruled, err := st.CreateApproval(ctx, newApproval(run.ID, time.Now().UTC()))
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	got, err := st.DecideApproval(ctx, ruled.ID, types.ApprovalDecision{State: types.ApprovalApproved, DecidedBy: "admin@corp.example",
		Basis: basis, Rule: &types.DeciderRuleRef{Unit: unit, Kind: types.DeciderKindEgressDomain}})
	if err != nil {
		t.Fatalf("decide under a rule: %v", err)
	}
	read, err := st.GetApproval(ctx, ruled.ID)
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	for name, ap := range map[string]types.ApprovalRequest{"decide": got, "get": read} {
		if ap.DecisionBasis != basis || ap.DecisionRuleUnit == nil || *ap.DecisionRuleUnit != unit {
			t.Errorf("%s: basis %q rule unit %v, want %q %s", name, ap.DecisionBasis, ap.DecisionRuleUnit, basis, unit)
		}
	}

	second := newApproval(run.ID, time.Now().UTC())
	second.RequestedScope = json.RawMessage(`{"n":1}`)
	plain, err := st.CreateApproval(ctx, second)
	if err != nil {
		t.Fatalf("create approval: %v", err)
	}
	got, err = st.DecideApproval(ctx, plain.ID, types.ApprovalDecision{State: types.ApprovalDenied, DecidedBy: "u"})
	if err != nil {
		t.Fatalf("decide with no rule: %v", err)
	}
	if got.DecisionBasis != "" || got.DecisionRuleUnit != nil {
		t.Errorf("no rule: basis %q rule unit %v, want neither", got.DecisionBasis, got.DecisionRuleUnit)
	}

	list, err := st.ListApprovals(ctx, types.ApprovalApproved)
	if err != nil {
		t.Fatalf("list approvals: %v", err)
	}
	found := false
	for _, ap := range list {
		if ap.ID == ruled.ID {
			found = ap.DecisionBasis == basis && ap.DecisionRuleUnit != nil && *ap.DecisionRuleUnit == unit
		}
	}
	if !found {
		t.Errorf("ListApprovals does not carry the decided row's basis and rule unit")
	}
}
