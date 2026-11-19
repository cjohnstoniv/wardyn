// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "fmt"

// ConfinementFloors is a governance profile's minimum confinement class per run trait: a run that has a trait
// launches at that trait's class or stronger, whatever its policy's own min_confinement_class says. A trait left
// empty raises nothing, so an absent ConfinementFloors changes no run. A closed struct with omitempty on every
// field, validated by Validate, like AutonomyRubric.
type ConfinementFloors struct {
	// OwnImage floors a run on an image its launcher chose: the request's image (an attached workspace's base
	// image included), a policy-carried workspace's image, a devcontainer build, or a workspace built from its
	// repository's own devcontainer.
	OwnImage ConfinementClass `json:"own_image,omitempty"`
	// SelfDefinedComponent floors a run that carries a component its launcher defined.
	SelfDefinedComponent ConfinementClass `json:"self_defined_component,omitempty"`
	// DataOutOpen floors a run whose data_out rule lets content leave: allowed, or unset.
	DataOutOpen ConfinementClass `json:"data_out_open,omitempty"`
}

// Validate reports the first field set to anything but CC1, CC2 or CC3, naming it, so a 400 says which floor to
// fix. An empty field is valid: it raises nothing.
func (c ConfinementFloors) Validate() error {
	for _, f := range []struct {
		field string
		class ConfinementClass
	}{
		{"own_image", c.OwnImage},
		{"self_defined_component", c.SelfDefinedComponent},
		{"data_out_open", c.DataOutOpen},
	} {
		if f.class != "" && f.class.Rank() == 0 {
			return fmt.Errorf("%s: %q is not CC1, CC2 or CC3", f.field, f.class)
		}
	}
	return nil
}
