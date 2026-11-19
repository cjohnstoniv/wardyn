// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"reflect"
	"testing"
)

// TestApprovalRequestDeciderWire pins the approval row's decider fields: a DECIDED row names how it was decided
// (decision_basis) and the unit whose rule it was decided under (decision_rule_unit), and a PENDING row a rule
// governs names that rule's max_scope and second_human beside its deciders. Each is omitted when empty, so a row
// no rule governs reads as it did before.
func TestApprovalRequestDeciderWire(t *testing.T) {
	want := map[string]bool{"decision_basis,omitempty": false, "decision_rule_unit,omitempty": false,
		"max_scope,omitempty": false, "second_human,omitempty": false}
	rt := reflect.TypeFor[ApprovalRequest]()
	for i := range rt.NumField() {
		if _, ok := want[rt.Field(i).Tag.Get("json")]; ok {
			want[rt.Field(i).Tag.Get("json")] = true
		}
	}
	for tag, found := range want {
		if !found {
			t.Errorf("ApprovalRequest has no field tagged json:%q", tag)
		}
	}
}
