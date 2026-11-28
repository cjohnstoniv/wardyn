// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistant

import (
	"reflect"
	"testing"
)

// Each refusal code maps to one dropped entry carrying the unit's approval count; a held widening is not dropped.
func TestDropped(t *testing.T) {
	asked := map[string]int{}
	approvals := func(unit string) int {
		asked[unit]++
		return map[string]int{"eng": 2, "ops": 1}[unit]
	}
	in := []Refusal{
		{Host: "evil.example", Code: DropOutsideCeiling, Unit: "eng"},
		{Field: "min_confinement_class", Code: DropClamped, Unit: "eng"},
		{Field: "resources", Code: DropInvalid, Unit: "ops"},
		{Field: "nonsense", Code: DropUnknownSection},
		{Host: "held.example", Code: DropOutsideCeiling, Unit: "eng", Held: true},
	}
	got := DroppedOf(in, approvals)
	want := []Dropped{
		{Host: "evil.example", Code: DropOutsideCeiling, Unit: "eng", AdminChangeApprovals: 2},
		{Field: "min_confinement_class", Code: DropClamped, Unit: "eng", AdminChangeApprovals: 2},
		{Field: "resources", Code: DropInvalid, Unit: "ops", AdminChangeApprovals: 1},
		{Field: "nonsense", Code: DropUnknownSection},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DroppedOf = %+v\nwant %+v", got, want)
	}
	if asked["eng"] != 1 || asked["ops"] != 1 {
		t.Errorf("approvals asked %v, want once per unit", asked)
	}
}

// No refusal, or only held widenings, is an empty list, never nil, so the proposal always says what it dropped.
func TestDroppedEmpty(t *testing.T) {
	for _, in := range [][]Refusal{nil, {{Host: "h", Held: true}}} {
		if got := DroppedOf(in, nil); got == nil || len(got) != 0 {
			t.Errorf("DroppedOf(%v) = %#v, want an empty list", in, got)
		}
	}
}
