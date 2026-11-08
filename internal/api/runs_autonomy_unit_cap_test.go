// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAutonomyUnitCapBelowSiteCap: the rubric's self_defined_component caps a run that carries a
// self-defined component. The site cap stays a floor: a unit tightens below it and never loosens it.
func TestAutonomyUnitCapBelowSiteCap(t *testing.T) {
	const auto = `{"agent":"claude-code","task":"t","confinement_class":"CC2","tool_approvals":"auto"}`
	unit := func(l types.AutonomyLevel) governanceCeiling {
		return profileCeiling(&types.AutonomyRubric{SelfDefinedComponent: l})
	}

	t.Run("the unit is lower: it alone binds, as the profile", func(t *testing.T) {
		got := callComponentCap(t, unit(types.AutonomyL0), selfComponent(types.AutonomyL1), auto)
		if got.ok || got.body.Reason != "governance_unit" || got.res.Level != types.AutonomyL0 ||
			!slices.Equal(got.res.BoundBy, []string{"self_defined_component"}) {
			t.Fatalf("got ok=%v reason %q %+v, want L0 bound by self_defined_component, refused as governance_unit", got.ok, got.body.Reason, got.res)
		}
	})
	t.Run("a tie names both", func(t *testing.T) {
		got := callComponentCap(t, unit(types.AutonomyL1), selfComponent(types.AutonomyL1), auto)
		if !got.ok || got.res.Level != types.AutonomyL1 || !slices.Equal(got.res.BoundBy, []string{"self_defined_component", "custom_component"}) {
			t.Fatalf("got ok=%v %+v, want L1 bound by self_defined_component and custom_component", got.ok, got.res)
		}
	})
	t.Run("the site cap is lower: the unit cannot loosen it", func(t *testing.T) {
		got := callComponentCap(t, unit(types.AutonomyL1), selfComponent(types.AutonomyL0), auto)
		if got.ok || got.body.Reason != "component_autonomy" || got.res.Level != types.AutonomyL0 || !slices.Equal(got.res.BoundBy, []string{"custom_component"}) {
			t.Fatalf("got ok=%v reason %q %+v, want L0 bound by custom_component, refused as component_autonomy", got.ok, got.body.Reason, got.res)
		}
	})
	t.Run("a run with no self-defined component is not capped by it", func(t *testing.T) {
		got := callComponentCap(t, unit(types.AutonomyL0), runComponents{}, auto)
		if !got.ok || got.res.Level != "" || len(got.res.BoundBy) != 0 {
			t.Fatalf("got ok=%v %+v, want no cap", got.ok, got.res)
		}
	})
	t.Run("the posture says the run carries one", func(t *testing.T) {
		got := callComponentCap(t, unit(types.AutonomyL1), selfComponent(""), auto)
		if !got.res.Posture.CustomComponent {
			t.Errorf("posture = %+v, want custom_component", got.res.Posture)
		}
	})
}
