// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	recOff    = types.RecordingOff
	recTerm   = types.RecordingTerminal
	recScreen = types.RecordingScreenActions
)

// TestRecordingClampIsAMinimum: a member's policy records at least what its ceiling does, and may
// record more; an unset proposal takes the ceiling's level.
func TestRecordingClampIsAMinimum(t *testing.T) {
	for _, tc := range []struct{ proposed, ceiling, want types.RecordingLevel }{
		{"", "", ""},
		{recOff, "", recOff},
		{"", recTerm, recTerm},
		{recOff, recTerm, recTerm},
		{recScreen, recTerm, recScreen},
		{recTerm, recScreen, recScreen},
		{recOff, recOff, recOff},
	} {
		out, _ := Clamp(types.RunPolicySpec{MinConfinementClass: types.CC2, Recording: tc.proposed},
			types.RunPolicySpec{MinConfinementClass: types.CC2, Recording: tc.ceiling}, types.GovernanceLimits{})
		if out.Recording != tc.want {
			t.Errorf("Clamp(%q under %q) = %q, want %q", tc.proposed, tc.ceiling, out.Recording, tc.want)
		}
	}
}

// TestRecordingLeq: a records at least what b does.
func TestRecordingLeq(t *testing.T) {
	for _, tc := range []struct {
		a, b types.RecordingLevel
		want bool
	}{
		{"", "", true},
		{recOff, "", true},
		{"", recOff, false},
		{recTerm, recOff, true},
		{recOff, recTerm, false},
		{recScreen, recTerm, true},
	} {
		a := types.RunPolicySpec{MinConfinementClass: types.CC2, Recording: tc.a}
		b := types.RunPolicySpec{MinConfinementClass: types.CC2, Recording: tc.b}
		if got := Leq(a, b, types.GovernanceLimits{}, types.GovernanceLimits{}); got != tc.want {
			t.Errorf("Leq(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestRecordingMeet: an overlay records at least the base's level; a lower one never lowers it, and
// a value that is not a level refuses the composition.
func TestRecordingMeet(t *testing.T) {
	lvl := func(l types.RecordingLevel) *types.RecordingLevel { return &l }
	for _, tc := range []struct {
		base types.RecordingLevel
		over *types.RecordingLevel
		want types.RecordingLevel
		bad  bool
	}{
		{"", lvl(recTerm), recTerm, false},
		{recTerm, lvl(recScreen), recScreen, false},
		{recScreen, lvl(recOff), recScreen, false},
		{"", lvl("keystrokes"), "", true},
		{"", lvl(""), "", true},
	} {
		base := Authority{Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2, Recording: tc.base}}
		got, _, err := ApplyOverlay(base, Overlay{Ceiling: types.CeilingOverlay{Recording: tc.over}})
		if tc.bad {
			if err == nil {
				t.Errorf("meet(%q, %q) = %q, want a refusal", tc.base, *tc.over, got.Ceiling.Recording)
			}
			continue
		}
		if err == nil && got.Ceiling.Recording != tc.want {
			t.Errorf("meet(%q, %q) = %q, want %q", tc.base, *tc.over, got.Ceiling.Recording, tc.want)
		}
		if err != nil && tc.over.Rank() >= tc.base.Rank() {
			t.Errorf("meet(%q, %q) refused: %v", tc.base, *tc.over, err)
		}
	}
}
