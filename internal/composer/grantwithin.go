// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"fmt"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// grantBoundFailure ranks HOW FAR a grant got against one candidate ceiling
// grant, so a refusal names the most specific reason rather than whichever
// candidate happened to be examined last.
type grantBoundFailure int

const (
	// The BOUND axes only. Kind and pairing are not ranked because they are not
	// walked here: CeilingGrantsCovering answers identity in one place, and a
	// grant it covers nothing of is refused with the matching sentence before any
	// ranking starts.
	boundFailApproval grantBoundFailure = iota
	boundFailOwnerOnly
	boundFailTTL
	boundFailGitHubScope
	boundFailPATScope
)

// GrantWithin is the one grant-dominance contract: g is within the ceiling when
// SOME single ceiling grant dominates it on EVERY axis, and the refusal names
// the most specific reason none did. internal/api's write-time and resolve-time
// bounds call it, and Leq calls it, so a profile, a composed overlay and a
// dispatched run are bounded by one rule rather than copies that drift.
//
// Why this bound exists. A profile's ceiling is otherwise freely narrower OR
// wider than the deployment's. Eligible grants are the one axis where it cannot
// be: they are DEPLOYER-PROVISIONED material (a stored operator secret, a
// GitHub App installation), and the principal authoring profiles is not
// necessarily the principal who provisioned them. A profile may narrow
// credential eligibility; it may never mint it.
//
// Monotone, not membership and not equality:
//
//   - Membership alone ("the pairing is listed") would let a grant keep the
//     pairing while STRIPPING RequiresApproval, and a stripped approval flag
//     auto-mints the injection at proxy boot with no human in the loop. So
//     ceiling.RequiresApproval implies g.RequiresApproval (forcing it ON is a
//     narrowing).
//   - Equality would refuse a legitimately STRICTER grant: a shorter TTL,
//     approval forced on, a smaller repo set.
//
// Axes, all in the narrowing direction: identity (CeilingGrantsCovering, the
// SAME selection the runtime clamp bounds against and the same pairing rule
// filterUserGrants enforces); approval; owner_only; TTL, both sides normalised
// by normalizeClampTTL (0 means the broker maximum, so a raw "<=" would accept a
// TTL of 0 under a ceiling of 300, which mints credentials twelve times as
// long); github_token repos/permissions (GitHubScopeWithin), which pairing
// cannot see because github_token names no stored secret; and the git_pat scope
// axes repos, access, api and forge (PATScopeWithin).
//
// Checking the axes against different ceiling grants would let a grant pair one
// ceiling grant's approval posture with another's TTL, which is precisely the
// widening a per-axis check misses; the RUNTIME clamp bounds from the same
// candidate set, because both sides call CeilingGrantsCovering.
//
// An undecodable scope or an unknown kind is refused, never passed: the pairing
// cannot be computed, so nothing can be said about whether it is in the ceiling.
func GrantWithin(g types.GrantSpec, ceiling []types.GrantSpec) error {
	switch g.Kind {
	case types.GrantAPIKey, types.GrantGitPAT, types.GrantSSHKey, types.GrantEnvSecret,
		types.GrantGitHubToken, types.GrantCloudSTS:
	default:
		return fmt.Errorf("eligible grant %q: invalid scope: unknown grant kind", g.Kind)
	}
	pairing, covered, ok := grantPairingOf(g)
	if !ok {
		return fmt.Errorf("eligible grant %q: invalid scope: the scope does not decode into a usable pairing", g.Kind)
	}

	covering := CeilingGrantsCovering(g, ceiling)
	if len(covering) == 0 {
		// Nothing covers it, and the two reasons need different sentences: a
		// pairing the deployment does not list, versus a KIND it does not list
		// at all. Only the covered kinds can produce the first.
		if covered && slices.ContainsFunc(ceiling, func(cg types.GrantSpec) bool { return cg.Kind == g.Kind }) {
			return fmt.Errorf(
				"eligible grant %q pairing secret %q with host %q is not in the deployment ceiling "+
					"(a profile may narrow the deployment's eligible grants, never add one)",
				g.Kind, pairing.secretRef, pairing.host)
		}
		return fmt.Errorf(
			"eligible grant %q is not in the deployment ceiling's eligible grants "+
				"(a profile may narrow the deployment's credential eligibility, never mint new eligibility)",
			g.Kind)
	}

	worst, worstErr := boundFailApproval, error(nil)
	note := func(rank grantBoundFailure, err error) {
		if worstErr == nil || rank >= worst {
			worst, worstErr = rank, err
		}
	}
	for _, cg := range covering {
		if cg.RequiresApproval && !g.RequiresApproval {
			note(boundFailApproval, fmt.Errorf(
				"eligible grant %q strips requires_approval, which the deployment ceiling sets "+
					"(a profile may force approval on, never off — without it the credential auto-mints at proxy boot)",
				g.Kind))
			continue
		}
		if cg.OwnerOnly && !g.OwnerOnly {
			note(boundFailOwnerOnly, fmt.Errorf(
				"eligible grant %q strips owner_only, which the deployment ceiling sets "+
					"(a profile may force it on, never off — without it a person with no row of their own is served the operator's)",
				g.Kind))
			continue
		}
		if pt, ct := normalizeClampTTL(g.TTLSeconds), normalizeClampTTL(cg.TTLSeconds); pt > ct {
			note(boundFailTTL, fmt.Errorf(
				"eligible grant %q ttl_seconds resolves to %ds, above the deployment ceiling's %ds "+
					"(0 means the %ds default, so it is not a narrowing)",
				g.Kind, pt, ct, maxGrantTTLSeconds))
			continue
		}
		if g.Kind == types.GrantGitHubToken {
			if err := GitHubScopeWithin(g.Scope, cg.Scope); err != nil {
				note(boundFailGitHubScope, fmt.Errorf("eligible grant %q: %w", g.Kind, err))
				continue
			}
		}
		if g.Kind == types.GrantGitPAT {
			if err := PATScopeWithin(g.Scope, cg.Scope); err != nil {
				note(boundFailPATScope, fmt.Errorf("eligible grant %q: %w", g.Kind, err))
				continue
			}
		}
		return nil // this ceiling grant dominates on every axis
	}
	// Non-nil by construction: covering is non-empty, and every iteration above
	// either returns nil or records a failure.
	return worstErr
}
