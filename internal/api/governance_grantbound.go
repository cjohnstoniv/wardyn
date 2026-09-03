// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The structural bound on a governance profile's eligible grants: a profile may
// NARROW the deployment's credential eligibility, never MINT new eligibility.
//
// Its own file, beside governance.go's CRUD, because it is one pure predicate
// with a security argument attached, and because the resolve-time re-check will
// call it from a different site than the write handlers do.
package api

import (
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxGovernanceGrantTTLSeconds MIRRORS composer's own maxGrantTTLSeconds (see
// internal/composer/clamp.go), which is unexported. It is the broker ceiling —
// a minted credential lives at most an hour — and it is what a TTL of 0 MEANS.
// Duplicated rather than exported because drift in THIS constant cannot widen
// anything: clampGrants re-caps every minted grant at the real maximum on both
// the profile-as-ceiling and deployment-as-ceiling paths, so a stale copy here
// can only make this comparator refuse MORE than it must. That argument rests on
// the clamp capping against the ceiling grant this comparator ACCEPTED against,
// which is now guaranteed rather than assumed: both sides select that grant with
// composer.ceilingGrantsBounding. While the clamp indexed the ceiling by kind
// alone, the argument held only for a ceiling carrying at most one grant per
// kind. The rules that could
// widen if they drifted — the github repo/permission subset test — are not
// duplicated at all; they live in composer beside the clamp that honors them
// (composer.GitHubScopeWithin).
const maxGovernanceGrantTTLSeconds = 3600

// normalizeGrantTTLSeconds resolves a GrantSpec's TTL to the number of seconds
// composer.Clamp would actually mint at (clamp.go's clampGrants: a ceiling TTL
// bounds the max, and 0 means "take the max"). True of the SAME ceiling grant
// on both sides now that the clamp selects by pairing rather than by kind — it
// was not while a ceiling with two same-kind grants let the clamp cap against
// one grant and this comparator judge against another.
//
// This is the whole of PF-28, and it is the difference between a comparator
// that works and one that admits the widening it was written to refuse: raw
// `profile.TTL <= ceiling.TTL` accepts a profile TTL of 0 under a ceiling of
// 300, because 0 < 300 — while 0 MEANS 3600, so the "narrower" profile mints
// credentials that live twelve times as long. Both sides normalize before
// comparing.
//
// A NEGATIVE ttl normalizes to the max too, which is one notch STRICTER than
// clampGrants (which leaves a negative alone). Strict is the right direction
// here: on the profile side it can only cause a refusal, and on the ceiling
// side it matches clampGrants exactly (`cg.TTLSeconds > 0 &&` — a negative
// ceiling TTL bounds nothing).
func normalizeGrantTTLSeconds(ttl int) int {
	if ttl <= 0 || ttl > maxGovernanceGrantTTLSeconds {
		return maxGovernanceGrantTTLSeconds
	}
	return ttl
}

// grantBoundFailure ranks HOW FAR a profile grant got against one candidate
// ceiling grant, so a refusal names the most specific reason rather than
// whichever candidate happened to be examined last. A profile grant is accepted
// when SOME single ceiling grant dominates it on EVERY axis — checking the axes
// against different ceiling grants would let a profile pair one grant's
// approval posture with another's TTL, which is precisely the widening a
// per-axis check misses. The RUNTIME clamp now takes its bound from that same
// single grant (composer's ceilingGrantsBounding); it used to take approval and
// TTL from whichever same-kind ceiling grant came last, which is this exact
// widening committed one layer down.
type grantBoundFailure int

const (
	boundFailKind grantBoundFailure = iota
	boundFailPairing
	boundFailApproval
	boundFailTTL
	boundFailGitHubScope
)

// governanceGrantsWithinCeiling reports whether every grant in profile is
// MONOTONE-⊆ some grant in ceiling, returning a caller-facing error naming the
// first grant that is not.
//
// WHY THIS BOUND EXISTS. A profile's ceiling is otherwise freely narrower OR
// wider than the deployment's — that asymmetry IS the feature for egress, tool
// rules and confinement. Eligible grants are the one axis where it cannot be:
// they are DEPLOYER-PROVISIONED material (a stored operator secret, a GitHub
// App installation), and the principal authoring profiles is not necessarily
// the principal who provisioned them. Without this bound, whoever holds the
// profile-authoring surface writes a profile carrying an arbitrary grant
// pairing, assigns it to themselves — their own ceiling IS their assigned
// profile, since the resolver's operator short-circuit keys on the admin tier
// they do not hold — and filterMemberGrants plus the dispatch injection then
// deliver any operator-stored secret into their own sandbox, with self-authored
// egress to carry it out. A profile may narrow credential eligibility; it may
// never mint it.
//
// MONOTONE, not membership and not equality — both of those are wrong in one
// direction:
//
//   - Membership alone ("the pairing is listed") would let a profile keep the
//     pairing while STRIPPING RequiresApproval, and a stripped approval flag
//     auto-mints the injection at proxy boot with no human in the loop. So:
//     ceiling.RequiresApproval ⇒ profile.RequiresApproval (forcing it ON is
//     always allowed; that is a narrowing).
//   - Equality would refuse a legitimately STRICTER profile — a shorter TTL,
//     approval forced on, a smaller GitHub repo set — which is the entire point
//     of authoring one.
//
// Four axes, all in the narrowing direction: the pairing must be one the
// deployment ceiling already lists (storedSecretPairingInCeiling, which forwards
// to composer.PairingInCeiling — the SAME comparator filterMemberGrants AND the
// runtime clamp use, so a profile, a member and a dispatched run are bounded by
// one rule, not three that can drift); approval may be forced on, never stripped;
// TTL may be shortened, never lengthened (both sides normalized —
// normalizeGrantTTLSeconds); and github_token repos/permissions must be a
// subset, which pairing checks CANNOT see (storedSecretGrantPairing reports
// covered=false for github_token — it names no stored secret) and which is
// therefore the one axis this file has to carry itself.
//
// Enforced at WRITE by the profile handlers. Config.DefaultPolicy is env-borne,
// so a redeploy that drops a pairing must not leave old profiles serving it
// forever — hence the resolve-time re-intersect, which calls this same
// function.
func governanceGrantsWithinCeiling(profile, ceiling []types.GrantSpec) error {
	for _, g := range profile {
		if err := governanceGrantWithinCeiling(g, ceiling); err != nil {
			return err
		}
	}
	return nil
}

// governanceGrantWithinCeiling is the per-grant half: find a ceiling grant that
// dominates g on every axis, or report the most specific reason none did.
func governanceGrantWithinCeiling(g types.GrantSpec, ceiling []types.GrantSpec) error {
	// An UNDECODABLE profile scope is a malformed write, not a silent pass: the
	// pairing cannot be computed, so nothing can be said about whether it is in
	// the ceiling. Fail closed, exactly as filterMemberGrants does.
	host, secretRef, knownHostsRef, covered, derr := storedSecretGrantPairing(g)
	if derr != nil {
		return fmt.Errorf("eligible grant %q: invalid scope: %w", g.Kind, derr)
	}

	worst, worstErr := boundFailKind, error(nil)
	note := func(rank grantBoundFailure, err error) {
		if worstErr == nil || rank >= worst {
			worst, worstErr = rank, err
		}
	}
	for _, cg := range ceiling {
		if cg.Kind != g.Kind {
			continue
		}
		// Pairing. For the stored-secret kinds this is the exact (host, secret,
		// known_hosts) match the member path already enforces. github_token and
		// cloud_sts name no stored secret, so same-kind membership IS the
		// pairing test for them and the GitHub scope check below carries the
		// rest.
		if covered && !storedSecretPairingInCeiling(g.Kind, host, secretRef, knownHostsRef, []types.GrantSpec{cg}) {
			note(boundFailPairing, fmt.Errorf(
				"eligible grant %q pairing secret %q with host %q is not in the deployment ceiling "+
					"(a profile may narrow the deployment's eligible grants, never add one)",
				g.Kind, secretRef, host))
			continue
		}
		if cg.RequiresApproval && !g.RequiresApproval {
			note(boundFailApproval, fmt.Errorf(
				"eligible grant %q strips requires_approval, which the deployment ceiling sets "+
					"(a profile may force approval on, never off — without it the credential auto-mints at proxy boot)",
				g.Kind))
			continue
		}
		if pt, ct := normalizeGrantTTLSeconds(g.TTLSeconds), normalizeGrantTTLSeconds(cg.TTLSeconds); pt > ct {
			note(boundFailTTL, fmt.Errorf(
				"eligible grant %q ttl_seconds resolves to %ds, above the deployment ceiling's %ds "+
					"(0 means the %ds default, so it is not a narrowing)",
				g.Kind, pt, ct, maxGovernanceGrantTTLSeconds))
			continue
		}
		if g.Kind == types.GrantGitHubToken {
			if err := composer.GitHubScopeWithin(g.Scope, cg.Scope); err != nil {
				note(boundFailGitHubScope, fmt.Errorf("eligible grant %q: %w", g.Kind, err))
				continue
			}
		}
		return nil // this ceiling grant dominates on every axis
	}
	if worstErr != nil {
		return worstErr
	}
	return fmt.Errorf(
		"eligible grant %q is not in the deployment ceiling's eligible grants "+
			"(a profile may narrow the deployment's credential eligibility, never mint new eligibility)",
		g.Kind)
}
