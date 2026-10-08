// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// componentAutonomyCause is the bound_by entry, and the sentence clause, that
// names the organisation's cap. It sits beside the rubric's own field names
// (egress_open, secrets_powerful, …), so it is spelled the same way.
const componentAutonomyCause = "custom_component"

// componentAutonomySource is who a refusal or the derived-hold warning names
// when the cap alone decided the level. It replaces "your governance profile
// %q" in the same sentences, so it reads in that slot.
const componentAutonomySource = "your organisation's rule for runs that use your own custom components"

// componentAutonomyCap is the level the organisation caps this run at: its
// components.autonomy_cap when the run carries a self-defined component, and
// "" (no cap) otherwise. "" is also the default setting, so with nothing set no
// run is capped — the owner's "warn only" default; the run is still marked as
// carrying reach its launcher added, by the gate and not here.
func componentAutonomyCap(comps runComponents) types.AutonomyLevel {
	if comps.selfDefined == 0 {
		return ""
	}
	return comps.settings.AutonomyCap
}

// autonomySource is who decided the level the ladder enforces: the phrase its
// sentences name, the reason its refusals carry, and the policy they point at.
type autonomySource struct {
	phrase string
	reason authz.Reason
	policy *policyref.Ref
}

// foldComponentCap folds the organisation's cap into the level the rubric
// resolved (folded is "" when no rubric bound the run) and says who decided the
// result:
//
//   - no cap: the rubric's answer, unchanged, named by the profile;
//   - the cap is lower, or nothing else bound the run: the cap, bound by
//     custom_component ALONE (the rubric's causes sit at a higher rung and did
//     not bind), with reason component_autonomy;
//   - a tie: the rubric's answer with custom_component added, named by the
//     profile, with reason governance_profile — raising either rung alone
//     would not move the level, so both are named;
//   - the rubric is lower: the rubric's answer, unchanged.
//
// When the cap alone binds, the refusal points at the DEPLOYMENT's policy even
// under a profile: the cap is a site-config setting, so the profile's contact
// is the wrong person to ask. With no profile that is exactly ceilingPolicy's
// answer; for an operator, as there, it is none.
//
// The policy is read only when a level results, as the ladder's caller did
// before: a run nothing caps costs no site-config read here.
func (s *Server) foldComponentCap(ctx context.Context, ceiling governanceCeiling,
	folded types.AutonomyLevel, boundBy []string, capLevel types.AutonomyLevel,
) (types.AutonomyLevel, []string, autonomySource) {
	switch {
	case capLevel == "" && folded == "":
		return "", boundBy, autonomySource{}
	case capLevel != "" && (folded == "" || capLevel.Rank() < folded.Rank()):
		return capLevel, []string{componentAutonomyCause}, autonomySource{
			phrase: componentAutonomySource,
			reason: authz.ReasonComponentAutonomy,
			policy: s.ceilingPolicy(ctx, governanceCeiling{Operator: ceiling.Operator}),
		}
	}
	if capLevel == folded {
		boundBy = append(slices.Clone(boundBy), componentAutonomyCause)
	}
	return folded, boundBy, autonomySource{
		phrase: fmt.Sprintf("your governance profile %q", ceiling.Profile.Name),
		reason: authz.ReasonGovernanceProfile,
		policy: s.ceilingPolicy(ctx, ceiling),
	}
}
