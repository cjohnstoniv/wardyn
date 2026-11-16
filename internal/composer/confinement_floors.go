// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import "github.com/cjohnstoniv/wardyn/internal/types"

// ConfinementTraits are the facts about a run that a profile's confinement floors read. The API fills each from
// the field the gate that owns that fact reads.
type ConfinementTraits struct {
	OwnImage             bool
	SelfDefinedComponent bool
	DataOutOpen          bool
}

// TraitFloor is floor raised to the strongest class floors sets for a trait the run has, with the names of the
// traits that set that class, in ConfinementFloors' field order. A trait whose class is not above floor raises
// nothing and is not named, so nil floors, or a run with none of the traits, leaves floor as it is and names
// none. It only ever raises; the credential floor (RequiredConfinementFloor) still applies after it.
func TraitFloor(floor types.ConfinementClass, floors *types.ConfinementFloors, t ConfinementTraits) (types.ConfinementClass, []string) {
	if floors == nil {
		return floor, nil
	}
	traits := []struct {
		name  string
		has   bool
		class types.ConfinementClass
	}{
		{"own_image", t.OwnImage, floors.OwnImage},
		{"self_defined_component", t.SelfDefinedComponent, floors.SelfDefinedComponent},
		{"data_out_open", t.DataOutOpen, floors.DataOutOpen},
	}
	top := confinementRank(floor)
	for _, c := range traits {
		if c.has {
			top = max(top, confinementRank(c.class))
		}
	}
	if top == confinementRank(floor) {
		return floor, nil
	}
	var by []string
	for _, c := range traits {
		if c.has && confinementRank(c.class) == top {
			by = append(by, c.name)
		}
	}
	return []types.ConfinementClass{types.CC1, types.CC2, types.CC3}[top-1], by
}
