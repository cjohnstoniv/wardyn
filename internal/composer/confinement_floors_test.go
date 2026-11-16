// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestLeqConfinementFloors: for each trait, Leq never proves a lowered, removed or emptied floor narrower, and
// proves a kept, raised or added one within; a floor moved to another trait is not narrower either.
func TestLeqConfinementFloors(t *testing.T) {
	var spec types.RunPolicySpec
	limits := func(f *types.ConfinementFloors) types.GovernanceLimits {
		return types.GovernanceLimits{ConfinementFloors: f}
	}
	for _, trait := range []struct {
		name string
		set  func(types.ConfinementClass) *types.ConfinementFloors
	}{
		{"own_image", func(c types.ConfinementClass) *types.ConfinementFloors { return &types.ConfinementFloors{OwnImage: c} }},
		{"self_defined_component", func(c types.ConfinementClass) *types.ConfinementFloors {
			return &types.ConfinementFloors{SelfDefinedComponent: c}
		}},
		{"data_out_open", func(c types.ConfinementClass) *types.ConfinementFloors {
			return &types.ConfinementFloors{DataOutOpen: c}
		}},
	} {
		for _, tc := range []struct {
			name string
			a, b *types.ConfinementFloors
			want bool
		}{
			{"a lowered floor is wider", trait.set(types.CC1), trait.set(types.CC2), false},
			{"a removed floor is wider", nil, trait.set(types.CC1), false},
			{"an emptied floor is wider", &types.ConfinementFloors{}, trait.set(types.CC2), false},
			{"a kept floor is within", trait.set(types.CC2), trait.set(types.CC2), true},
			{"a raised floor is narrower", trait.set(types.CC3), trait.set(types.CC2), true},
			{"an added floor is narrower", trait.set(types.CC2), nil, true},
		} {
			if got := Leq(spec, spec, limits(tc.a), limits(tc.b)); got != tc.want {
				t.Errorf("%s: %s: Leq = %v, want %v", trait.name, tc.name, got, tc.want)
			}
		}
	}
	if Leq(spec, spec, limits(&types.ConfinementFloors{DataOutOpen: types.CC3}), limits(&types.ConfinementFloors{OwnImage: types.CC2})) {
		t.Error("a floor moved to another trait is proved narrower")
	}
}

// TestMeetConfinementFloors: the meet takes the higher class per trait and keeps a floor the overlay leaves
// unset; an overlay floor below the base's is refused at write and the base's is kept at resolve; and no result
// is wider than its base.
func TestMeetConfinementFloors(t *testing.T) {
	floors := func(f types.ConfinementFloors) types.GovernanceLimits {
		return types.GovernanceLimits{ConfinementFloors: &f}
	}
	for _, tc := range []struct {
		name string
		base types.GovernanceLimits
		ov   types.ConfinementFloors
		want *types.ConfinementFloors
	}{
		{"a higher overlay floor wins", floors(types.ConfinementFloors{OwnImage: types.CC2}),
			types.ConfinementFloors{OwnImage: types.CC3}, &types.ConfinementFloors{OwnImage: types.CC3}},
		{"a floor on the overlay only is taken", types.GovernanceLimits{},
			types.ConfinementFloors{DataOutOpen: types.CC2}, &types.ConfinementFloors{DataOutOpen: types.CC2}},
		{"a floor on the base only is kept", floors(types.ConfinementFloors{SelfDefinedComponent: types.CC2}),
			types.ConfinementFloors{OwnImage: types.CC3}, &types.ConfinementFloors{OwnImage: types.CC3, SelfDefinedComponent: types.CC2}},
		{"an overlay that raises nothing keeps none", types.GovernanceLimits{}, types.ConfinementFloors{}, nil},
		{"a higher overlay floor wins on self_defined_component", floors(types.ConfinementFloors{SelfDefinedComponent: types.CC1}),
			types.ConfinementFloors{SelfDefinedComponent: types.CC3}, &types.ConfinementFloors{SelfDefinedComponent: types.CC3}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := Authority{Limits: tc.base}
			got := mustApply(t, base, limitsOverlay(types.LimitsOverlay{ConfinementFloors: &tc.ov}))
			if !reflect.DeepEqual(got.Limits.ConfinementFloors, tc.want) {
				t.Errorf("floors = %+v, want %+v", got.Limits.ConfinementFloors, tc.want)
			}
			if !Leq(got.Ceiling, base.Ceiling, got.Limits, base.Limits) {
				t.Errorf("the result %+v is wider than its base %+v", got.Limits.ConfinementFloors, base.Limits.ConfinementFloors)
			}
		})
	}
	base := Authority{Limits: floors(types.ConfinementFloors{OwnImage: types.CC3})}
	lower := limitsOverlay(types.LimitsOverlay{ConfinementFloors: &types.ConfinementFloors{OwnImage: types.CC2}})
	got, warns, err := ApplyOverlay(base, lower)
	if err != nil || got.Limits.ConfinementFloors == nil || got.Limits.ConfinementFloors.OwnImage != types.CC3 {
		t.Errorf("a lower overlay floor at resolve: floors %+v, err %v; want the base's CC3 kept", got.Limits.ConfinementFloors, err)
	}
	if !slices.ContainsFunc(warns, func(w string) bool { return strings.HasPrefix(w, "confinement_floors.own_image: ") }) {
		t.Errorf("warnings = %q, want one naming confinement_floors.own_image", warns)
	}
	wantOverlayErr(t, ValidateOverlay(base, lower), ReasonOverlayInvalid, "confinement_floors.own_image")
}

// TestValidateOverlayRefusesAnUnknownConfinementFloor: an overlay floor that is not CC1, CC2 or CC3 is refused
// before the meet, naming confinement_floors.
func TestValidateOverlayRefusesAnUnknownConfinementFloor(t *testing.T) {
	ov := limitsOverlay(types.LimitsOverlay{ConfinementFloors: &types.ConfinementFloors{DataOutOpen: "CC9"}})
	wantOverlayErr(t, ValidateOverlay(Authority{}, ov), ReasonOverlayInvalid, "confinement_floors")
}

// TestComposedConfinementFloorIsChecked: a stored floor with an unknown class is refused when the profile is
// composed, naming limits.confinement_floors, as an autonomy rubric is.
func TestComposedConfinementFloorIsChecked(t *testing.T) {
	base := Authority{Limits: types.GovernanceLimits{ConfinementFloors: &types.ConfinementFloors{OwnImage: "CC9"}}}
	_, _, err := ApplyOverlay(base, limitsOverlay(types.LimitsOverlay{}))
	wantOverlayErr(t, err, ReasonOverlayUnsatisfiable, "limits.confinement_floors")
}

// TestTraitFloor: each trait raises the floor to its class only when the run has it, the strongest class wins
// and names every trait that set it, and a class at or below the floor raises nothing.
func TestTraitFloor(t *testing.T) {
	all := ConfinementTraits{OwnImage: true, SelfDefinedComponent: true, DataOutOpen: true}
	for _, tc := range []struct {
		name   string
		floor  types.ConfinementClass
		floors *types.ConfinementFloors
		traits ConfinementTraits
		want   types.ConfinementClass
		by     []string
	}{
		{"no floors", types.CC1, nil, all, types.CC1, nil},
		{"own_image fires", types.CC1, &types.ConfinementFloors{OwnImage: types.CC3},
			ConfinementTraits{OwnImage: true}, types.CC3, []string{"own_image"}},
		{"self_defined_component fires", types.CC1, &types.ConfinementFloors{SelfDefinedComponent: types.CC2},
			ConfinementTraits{SelfDefinedComponent: true}, types.CC2, []string{"self_defined_component"}},
		{"data_out_open fires", "", &types.ConfinementFloors{DataOutOpen: types.CC2},
			ConfinementTraits{DataOutOpen: true}, types.CC2, []string{"data_out_open"}},
		{"a trait the run lacks raises nothing", types.CC1, &types.ConfinementFloors{OwnImage: types.CC3},
			ConfinementTraits{SelfDefinedComponent: true, DataOutOpen: true}, types.CC1, nil},
		{"the strongest class wins", types.CC1, &types.ConfinementFloors{OwnImage: types.CC2, DataOutOpen: types.CC3},
			all, types.CC3, []string{"data_out_open"}},
		{"a tie names both", types.CC1, &types.ConfinementFloors{OwnImage: types.CC3, SelfDefinedComponent: types.CC3},
			all, types.CC3, []string{"own_image", "self_defined_component"}},
		{"a trait the run lacks is not named at a tie", types.CC1, &types.ConfinementFloors{OwnImage: types.CC3, DataOutOpen: types.CC3},
			ConfinementTraits{DataOutOpen: true}, types.CC3, []string{"data_out_open"}},
		{"a floor never lowers", types.CC3, &types.ConfinementFloors{OwnImage: types.CC2}, all, types.CC3, nil},
		{"a class equal to the floor names none", types.CC2, &types.ConfinementFloors{OwnImage: types.CC2}, all, types.CC2, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, by := TraitFloor(tc.floor, tc.floors, tc.traits)
			if got != tc.want || !slices.Equal(by, tc.by) {
				t.Errorf("TraitFloor = %q %v, want %q %v", got, by, tc.want, tc.by)
			}
		})
	}
}

// TestTraitFloorNamesAreTheFieldNames: the names TraitFloor gives are the ConfinementFloors json names in field
// order, so what a run records and what a profile sets are one closed set.
func TestTraitFloorNamesAreTheFieldNames(t *testing.T) {
	var want []string
	ft := reflect.TypeOf(types.ConfinementFloors{})
	for i := range ft.NumField() {
		name, _, _ := strings.Cut(ft.Field(i).Tag.Get("json"), ",")
		want = append(want, name)
	}
	all := &types.ConfinementFloors{OwnImage: types.CC3, SelfDefinedComponent: types.CC3, DataOutOpen: types.CC3}
	if _, by := TraitFloor("", all, ConfinementTraits{OwnImage: true, SelfDefinedComponent: true, DataOutOpen: true}); !slices.Equal(by, want) {
		t.Errorf("names = %v, want the json names %v", by, want)
	}
}
