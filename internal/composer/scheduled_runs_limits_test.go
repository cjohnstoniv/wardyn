// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMeetScheduledRunsDoor: the scheduled-runs door only ever closes. An overlay that sets it
// wins, and one that leaves it false or silent keeps the base's denial.
func TestMeetScheduledRunsDoor(t *testing.T) {
	for _, tc := range []struct {
		name string
		base bool
		ov   *bool
		want bool
	}{
		{"overlay denies, base open", false, ptr(true), true},
		{"overlay open, base denies", true, ptr(false), true},
		{"overlay silent, base denies", true, nil, true},
		{"overlay silent, base open", false, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := Authority{Limits: types.GovernanceLimits{DenyScheduledRuns: tc.base}}
			got := mustApply(t, base, limitsOverlay(types.LimitsOverlay{DenyScheduledRuns: tc.ov}))
			if got.Limits.DenyScheduledRuns != tc.want {
				t.Errorf("DenyScheduledRuns = %v, want %v", got.Limits.DenyScheduledRuns, tc.want)
			}
		})
	}
}

// TestLeqKeepsScheduledDenial: a result that lifts the base's scheduled-runs denial is wider; one
// that adds the denial is narrower.
func TestLeqKeepsScheduledDenial(t *testing.T) {
	var spec types.RunPolicySpec
	denied := types.GovernanceLimits{DenyScheduledRuns: true}
	open := types.GovernanceLimits{}
	if Leq(spec, spec, open, denied) {
		t.Error("Leq is true for a lifted scheduled-runs denial")
	}
	if !Leq(spec, spec, denied, open) {
		t.Error("Leq is false for a new scheduled-runs denial")
	}
	if !Leq(spec, spec, denied, denied) {
		t.Error("Leq is false for a kept scheduled-runs denial")
	}
}

// TestFoldSelfDefinedComponent: the self_defined_component field caps only a run whose posture
// carries a self-defined component, and ties and minima name it like any other field.
func TestFoldSelfDefinedComponent(t *testing.T) {
	sealedNoneCC2 := types.AutonomyPosture{Egress: types.AutonomyEgressSealed, Secrets: types.AutonomySecretsNone, Confinement: types.CC2}
	withComponent := sealedNoneCC2
	withComponent.CustomComponent = true
	for _, tc := range []struct {
		name      string
		rubric    types.AutonomyRubric
		posture   types.AutonomyPosture
		wantLevel types.AutonomyLevel
		wantBound []string
	}{
		{"a run with no self-defined component is not capped by it",
			types.AutonomyRubric{SelfDefinedComponent: types.AutonomyL0}, sealedNoneCC2, "", nil},
		{"the zero posture selects nothing",
			types.AutonomyRubric{SelfDefinedComponent: types.AutonomyL0}, types.AutonomyPosture{}, "", nil},
		{"a run with one is capped by it alone",
			types.AutonomyRubric{SelfDefinedComponent: types.AutonomyL1}, withComponent, types.AutonomyL1, []string{"self_defined_component"}},
		{"an unset field caps nothing",
			types.AutonomyRubric{EgressSealed: types.AutonomyL2}, withComponent, types.AutonomyL2, []string{"egress_sealed"}},
		{"the lower of the two binds, not the last",
			types.AutonomyRubric{EgressSealed: types.AutonomyL0, SelfDefinedComponent: types.AutonomyL2}, withComponent, types.AutonomyL0, []string{"egress_sealed"}},
		{"the field alone binds when it is lower",
			types.AutonomyRubric{EgressSealed: types.AutonomyL3, SecretsNone: types.AutonomyL3, ConfinementCC2: types.AutonomyL3, SelfDefinedComponent: types.AutonomyL1},
			withComponent, types.AutonomyL1, []string{"self_defined_component"}},
		{"a tie names it last, after the three axes",
			types.AutonomyRubric{EgressSealed: types.AutonomyL1, SecretsNone: types.AutonomyL1, ConfinementCC2: types.AutonomyL1, SelfDefinedComponent: types.AutonomyL1},
			withComponent, types.AutonomyL1, []string{"egress_sealed", "secrets_none", "confinement_cc2", "self_defined_component"}},
	} {
		level, bound := FoldAutonomy(tc.rubric, tc.posture)
		if level != tc.wantLevel || !slices.Equal(bound, tc.wantBound) {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.name, level, bound, tc.wantLevel, tc.wantBound)
		}
	}
}

// TestMeetRubricSelfDefined: the field takes the lower level, a field set on one side only takes
// that side, and a higher overlay level is a widening that leaves the base's level in place.
func TestMeetRubricSelfDefined(t *testing.T) {
	rubric := func(l types.AutonomyLevel) *types.AutonomyRubric {
		return &types.AutonomyRubric{SelfDefinedComponent: l}
	}
	for _, tc := range []struct {
		name     string
		base, ov types.AutonomyLevel
		want     types.AutonomyLevel
		widens   bool
	}{
		{"overlay lowers", types.AutonomyL2, types.AutonomyL1, types.AutonomyL1, false},
		{"overlay silent", types.AutonomyL2, "", types.AutonomyL2, false},
		{"base silent", "", types.AutonomyL1, types.AutonomyL1, false},
		{"overlay raises", types.AutonomyL1, types.AutonomyL3, types.AutonomyL1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := Authority{}
			if tc.base != "" {
				base.Limits.AutonomyRubric = rubric(tc.base)
			}
			ov := types.LimitsOverlay{}
			if tc.ov != "" {
				ov.AutonomyRubric = rubric(tc.ov)
			}
			got, warns, err := ApplyOverlay(base, limitsOverlay(ov))
			if err != nil {
				t.Fatal(err)
			}
			if got.Limits.AutonomyRubric == nil || got.Limits.AutonomyRubric.SelfDefinedComponent != tc.want {
				t.Errorf("SelfDefinedComponent = %+v, want %s", got.Limits.AutonomyRubric, tc.want)
			}
			if widened := slices.ContainsFunc(warns, func(w string) bool { return strings.HasPrefix(w, "autonomy_rubric.self_defined_component") }); widened != tc.widens {
				t.Errorf("widening warning = %v, want %v (warnings %v)", widened, tc.widens, warns)
			}
		})
	}
}

// TestLeqRubricSelfDefined: dropping or raising the field widens the result; lowering it narrows.
func TestLeqRubricSelfDefined(t *testing.T) {
	var spec types.RunPolicySpec
	with := func(l types.AutonomyLevel) types.GovernanceLimits {
		return types.GovernanceLimits{AutonomyRubric: &types.AutonomyRubric{SelfDefinedComponent: l}}
	}
	for _, tc := range []struct {
		name string
		a, b types.GovernanceLimits
		want bool
	}{
		{"a dropped cap", types.GovernanceLimits{}, with(types.AutonomyL1), false},
		{"a raised cap", with(types.AutonomyL3), with(types.AutonomyL1), false},
		{"the same cap", with(types.AutonomyL1), with(types.AutonomyL1), true},
		{"a lower cap", with(types.AutonomyL0), with(types.AutonomyL1), true},
		{"a new cap", with(types.AutonomyL1), types.GovernanceLimits{}, true},
	} {
		if got := Leq(spec, spec, tc.a, tc.b); got != tc.want {
			t.Errorf("%s: Leq = %v, want %v", tc.name, got, tc.want)
		}
	}
}
