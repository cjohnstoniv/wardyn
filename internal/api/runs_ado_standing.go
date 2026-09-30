// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// adoStandingAtDoor is dispatch's member bound on a run policy's
// azure_devops_capabilities (adoEntraRun.withPolicyCapabilities), asked at the
// two doors so a person hears it before the run, not as a FAILED badge:
//
//   - narrowed: the sentence for the 201's clamp_warnings when the bound kept
//     some of a non-empty list and dropped the rest;
//   - none=true: nothing in the list may stand, which dispatch refuses with
//     ado_capabilities_none_permitted. Review refuses with the same reason and
//     sentence; launch still refuses at dispatch (the live provider row is
//     re-read there), so no run row is created or changed by this check.
//
// scmSite is the snapshot resolveRunAutonomy read at the same door, and the
// lane is resolved from it exactly as dispatch resolves it, so the answer is
// the one dispatch will give. Zero values for a run not on the per-person lane
// or with no list, which is every run on most installs.
func (s *Server) adoStandingAtDoor(r *http.Request, spec types.RunPolicySpec, scmSite types.SiteConfig,
	ceiling governanceCeiling,
) (narrowed string, none bool) {
	picked := spec.AzureDevOpsCapabilities
	if len(picked) == 0 {
		return "", false
	}
	lane, on := resolveADOEntraRun(scmSite, repoLocatorsOf(spec.WorkspaceRepos),
		runIdentitySubject(r.Context(), principalFromRequest(r)))
	if !on {
		return "", false
	}
	bounded, permitted := lane.withPolicyCapabilities(picked, adoStandingFor(ceiling))
	if !permitted {
		return "", true
	}
	var dropped []string
	for _, c := range picked {
		if !slices.Contains(bounded.caps, c) {
			dropped = append(dropped, "“"+adoscope.ShortLabel(c)+"”")
		}
	}
	if len(dropped) == 0 {
		return "", false
	}
	return fmt.Sprintf("dropped %d Azure DevOps capability(ies) not in this provider's default profile or the access "+
		"your administrator granted you: %s", len(dropped), strings.Join(dropped, ", ")), false
}
