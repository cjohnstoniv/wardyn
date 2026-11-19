// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoStandingAtDoor is dispatch's member bound on a run policy's
// azure_devops_capabilities (adoEntraRun.withPolicyCapabilities), asked at the
// two doors so a person hears it before the run, not as a FAILED badge:
//
//   - narrowed: the sentence for the 201's clamp_warnings when the bound kept
//     some of a non-empty list and dropped the rest, and the capabilities it dropped;
//   - none=true: nothing in the list may stand, which dispatch refuses with
//     ado_capabilities_none_permitted. Review refuses with the same reason and
//     sentence; launch still refuses at dispatch (the live provider row is
//     re-read there), so no run row is created or changed by this check;
//   - pastCeiling: what the bounded list holds beyond the provider row's
//     capability_ceiling, which dispatch refuses as capability_ceiling for a
//     policy's list (authorADOEntraInjection). Review refuses with
//     ado_capabilities_above_ceiling and dispatch's sentence. Dispatch bounds
//     first and asks the ceiling of what is left, so a member's capability
//     that was dropped is never counted here. The check is on by default: it
//     moves a certain FAILED run to Review and refuses nothing dispatch would not.
//
// lane is the run's lane as stepADOStanding resolved it from the snapshot
// resolveRunAutonomy read at the same door, exactly as dispatch resolves it, so
// the answer is the one dispatch will give. Zero values for a run not on the
// per-person lane or with no list, which is every run on most installs.
func adoStandingAtDoor(spec types.RunPolicySpec, lane adoEntraRun, ceiling governanceCeiling,
) (narrowed string, droppedCaps, pastCeiling []adoscope.Capability, none bool) {
	picked := spec.AzureDevOpsCapabilities
	if len(picked) == 0 {
		return "", nil, nil, false
	}
	if lane.rowID == "" {
		return "", nil, nil, false
	}
	bounded, permitted := lane.withPolicyCapabilities(picked, adoStandingFor(ceiling))
	if !permitted {
		return "", nil, nil, true
	}
	pastCeiling = adoCapsOutside(bounded.caps, lane.ceiling)
	var dropped []string
	for _, c := range picked {
		if !adoscope.Has(bounded.caps, c) {
			dropped = append(dropped, "“"+adoscope.ShortLabel(c)+"”")
			droppedCaps = append(droppedCaps, c)
		}
	}
	if len(dropped) == 0 {
		return "", nil, pastCeiling, false
	}
	return adoNotIncluded(dropped), droppedCaps, pastCeiling, false
}

// refuseADOStandingAtDoor writes the dry doors' refusal for adoStandingAtDoor's
// answer, none_permitted first, and reports whether it wrote one. Create meets
// both at dispatch and never calls this.
func refuseADOStandingAtDoor(w http.ResponseWriter, picked, pastCeiling []adoscope.Capability, none bool) bool {
	switch {
	case none:
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonADOCapabilitiesNonePermitted, adoNonePermitted(picked))
	case len(pastCeiling) > 0:
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonADOCapabilitiesAboveCeiling, adoPolicyPastCeiling(pastCeiling))
	default:
		return false
	}
	return true
}

// adoNotIncluded is the approved canon sentence (#1384.1) for one or more
// capabilities the member bound dropped; labels are already in curly quotes.
func adoNotIncluded(labels []string) string {
	pronoun, verb := "it", "isn't"
	granted := "hasn't granted it"
	if len(labels) > 1 {
		pronoun, verb, granted = "they", "aren't", "hasn't granted them"
	}
	return "Not included: " + strings.Join(labels, ", ") + ". Your administrator " + granted + " to you, and " +
		pronoun + " " + verb + " in this provider's default access."
}
