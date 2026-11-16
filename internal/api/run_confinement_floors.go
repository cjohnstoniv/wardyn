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
// rule that lets content leave (stepComputerFolderDataOut's reading). Like createDigestInput, this and the
// functions below take the fold's facts, not the fold: only runFoldSteps steps take the fold.
func confinementTraits(req createRunRequest, carried string, comps runComponents, spec types.RunPolicySpec) composer.ConfinementTraits {
	return composer.ConfinementTraits{
		OwnImage:             createDigestInput(req, carried) != "" || req.DevcontainerRepo != "",
		SelfDefinedComponent: comps.selfDefined > 0,
		DataOutOpen:          spec.DataOut.DataOut() == types.TransferAllowed,
	}
}

// traitFloor is the run's policy minimum raised by its profile's confinement floors for the traits it has, and
// the traits that raised it. spec is the run's resolved policy, whose minimum no floor has raised, so the
// traits are named against the policy's own class. The floors come from the caller's resolved profile
// (ceiling.Limits), as every other limit does, so a run no profile governs carries none.
func traitFloor(req createRunRequest, carried string, comps runComponents, spec types.RunPolicySpec, ceiling governanceCeiling) (types.ConfinementClass, []string) {
	return composer.TraitFloor(spec.MinConfinementClass, ceiling.Limits.ConfinementFloors, confinementTraits(req, carried, comps, spec))
}

// confinementFloor is the spec a run's confinement floor is computed on (confinementFloorSpec), its minimum
// class raised by traitFloor. stepPlacement and stepConfinement both read it and nothing else builds a floor
// spec, so the class a run is placed for and the class it is held to are one at every door and at a queued
// start, which replays the fold.
func confinementFloor(req createRunRequest, carried string, comps runComponents, spec types.RunPolicySpec, ceiling governanceCeiling) types.RunPolicySpec {
	floor := confinementFloorSpec(spec, comps)
	floor.MinConfinementClass, _ = traitFloor(req, carried, comps, spec, ceiling)
	return floor
}

// confinementRaisedBy names the traits whose floor raised the run's minimum class; nil when none did.
func confinementRaisedBy(req createRunRequest, carried string, comps runComponents, spec types.RunPolicySpec, ceiling governanceCeiling) []string {
	_, by := traitFloor(req, carried, comps, spec, ceiling)
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
