// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// confinementTraits are the run facts a governance profile's confinement floors read, each from the field the
// gate that owns it reads: the image the digest rule holds (createDigestInput) or a devcontainer build, a
// component the launcher defined (runComponents.selfDefined, as the autonomy posture counts it), and a data_out
// rule that lets content leave (stepComputerFolderDataOut's reading).
func (f *runFold) confinementTraits() composer.ConfinementTraits {
	return composer.ConfinementTraits{
		OwnImage:             createDigestInput(*f.req, f.carriedImage) != "" || f.req.DevcontainerRepo != "",
		SelfDefinedComponent: f.comps.selfDefined > 0,
		DataOutOpen:          f.spec.DataOut.DataOut() == types.TransferAllowed,
	}
}

// traitFloor is the run's policy minimum raised by its profile's confinement floors for the traits it has, and
// the traits that raised it. The floors come from the caller's resolved profile (f.ceiling.Limits), as every
// other limit does, so a run no profile governs carries none.
func (f *runFold) traitFloor() (types.ConfinementClass, []string) {
	return composer.TraitFloor(f.spec.MinConfinementClass, f.ceiling.Limits.ConfinementFloors, f.confinementTraits())
}

// confinementFloor is the spec a run's confinement floor is computed on (confinementFloorSpec), its minimum
// class raised by traitFloor. stepPlacement and stepConfinement both read it and nothing else builds a floor
// spec, so the class a run is placed for and the class it is held to are one at every door and at a queued
// start, which replays the fold.
func (f *runFold) confinementFloor() types.RunPolicySpec {
	spec := confinementFloorSpec(f.spec, f.comps)
	spec.MinConfinementClass, _ = f.traitFloor()
	return spec
}

// confinementRaisedBy names the traits whose floor raised the run's minimum class; nil when none did.
func (f *runFold) confinementRaisedBy() []string {
	_, by := f.traitFloor()
	return by
}

// stampConfinementRaisedBy records on run.create the traits whose profile floor raised the run's minimum
// confinement class (confinement_raised_by), a closed vocabulary: the confinement_floors field names. A run no
// floor raised carries nothing.
func stampConfinementRaisedBy(data map[string]any, by []string) {
	if len(by) > 0 {
		data["confinement_raised_by"] = by
	}
}

// confinementRaisedWarning is Review's sentence for a minimum class the profile's confinement floors raised,
// naming the traits that did; none when they raised nothing.
func confinementRaisedWarning(class types.ConfinementClass, by []string) []string {
	if len(by) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("Your governance profile raises this run's minimum confinement class to %s because of what the run does (confinement_floors: %s).", class, strings.Join(by, ", "))}
}
