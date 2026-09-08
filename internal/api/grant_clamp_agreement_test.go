// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestClampAndComparatorAgreeOnEveryCeilingShape is F014's cross-package pin,
// widened after round 2 reopened the finding.
//
// TestClampAndComparatorAreOneRule (governance_grantbound_test.go) pins ONE
// input — pairing A asking for pairing B's TTL headroom. Round 1 closed that
// one and left two more standing, because the two sites still run two selection
// rules: the comparator asks "does SOME single ceiling grant dominate this
// proposal on every axis", the clamp MET every candidate. Meeting can only
// narrow, so neither residual was a widening — but a rule that gives one input
// two answers is the defect F014 names, whichever direction it errs in.
//
// The agreement is stated as two directions, and both are asserted for every
// shape below:
//
//	D1  the comparator ACCEPTS the proposal  =>  the clamp returns it UNCHANGED.
//	    A grant the operator's own ceiling already permits must not be narrowed
//	    on its way to a run. This is the direction round 1 broke for github_token.
//	D2  the clamp KEEPS a grant  =>  the comparator ACCEPTS what it kept.
//	    Whatever bound the clamp applied is a bound some single ceiling grant
//	    actually wrote. This is the no-widening direction.
//
// Plus order independence: a ceiling is a SET everywhere else in this codebase,
// so reversing the slice cannot move either answer.
//
// D2 is asserted only when the proposal's pairing IS in the ceiling. A proposal
// naming a pairing no ceiling entry carries is deliberately KEPT by the clamp
// (bounded to the strictest same-kind entry) and REFUSED by the comparator, and
// that asymmetry is by design: the clamp bounds, the pairing gate is
// filterMemberGrants (stage 2 of boundMemberSpec), and composer/grantbound_test
// .go's TestClampGrantsBoundsByPairingNotKind pins it. Asserting D2 there would
// be asserting the clamp took over another stage's job.
func TestClampAndComparatorAgreeOnEveryCeilingShape(t *testing.T) {
	const hostA, hostB = "a.corp.example", "b.corp.example"
	apiKey := func(host, secret string, approval bool, ttl int) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantAPIKey, Scope: apiKeyScope(t, host, secret), RequiresApproval: approval, TTLSeconds: ttl}
	}
	gh := func(repos []string, perms map[string]string, approval bool, ttl int) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantGitHubToken, Scope: ghScopeJSON(t, repos, perms), RequiresApproval: approval, TTLSeconds: ttl}
	}
	ssh := func(host, keyRef string, approval bool, ttl int) types.GrantSpec {
		return types.GrantSpec{
			Kind:             types.GrantSSHKey,
			Scope:            mustJSON(map[string]any{"host": host, "key_secret_ref": keyRef}),
			RequiresApproval: approval,
			TTLSeconds:       ttl,
		}
	}

	for _, tc := range []struct {
		name             string
		ceiling          []types.GrantSpec
		proposal         types.GrantSpec
		pairingInCeiling bool
	}{
		{
			// ROUND-2 RESIDUAL 1. Two github_token entries are the only way to give
			// two repo sets different permission levels (one scope carries one
			// permissions map), and validatePolicySpec has no per-kind cardinality
			// rule. The comparator accepts a proposal naming org/beta because the
			// org/beta entry dominates it; the clamp must not intersect it against
			// the org/alpha entry as well and hand the run no repos at all.
			name: "github_token: a ceiling carving two repo sets",
			ceiling: []types.GrantSpec{
				gh([]string{"org/alpha"}, map[string]string{"contents": "write"}, false, 3600),
				gh([]string{"org/beta"}, map[string]string{"contents": "write"}, false, 3600),
			},
			proposal:         gh([]string{"org/beta"}, map[string]string{"contents": "read"}, false, 3600),
			pairingInCeiling: true,
		},
		{
			// ROUND-2 RESIDUAL 2. One pairing written twice with different bounds.
			// The comparator accepts (the permissive entry dominates), so D1 forces
			// one answer and order independence forces it to be the same answer in
			// both slice orders.
			name: "api_key: one pairing written twice",
			ceiling: []types.GrantSpec{
				apiKey(hostA, "secret-a", true, 300),
				apiKey(hostA, "secret-a", false, 3600),
			},
			proposal:         apiKey(hostA, "secret-a", false, 3600),
			pairingInCeiling: true,
		},
		{
			// F014's ORIGINAL fixture, kept so round 1's fix cannot regress: the
			// comparator refuses pairing A at pairing B's TTL, so only D2 applies —
			// the clamp must bound it to pairing A's own 60s.
			name: "api_key: pairing A asking for pairing B's TTL headroom",
			ceiling: []types.GrantSpec{
				apiKey(hostA, "secret-a", false, 60),
				apiKey(hostB, "secret-b", false, 3600),
			},
			proposal:         apiKey(hostA, "secret-a", false, 3600),
			pairingInCeiling: true,
		},
		{
			// F014's other original consequence: a proposal naming the STRICT
			// forge's pairing must not pick up the permissive forge's approval
			// posture. requires_approval=false auto-mints the injection at proxy
			// boot with no human in the loop.
			name: "ssh_key: two forges, proposal names the strict one",
			ceiling: []types.GrantSpec{
				ssh("git.corp.example", "corp_key", true, 300),
				ssh("git.public.example", "public_key", false, 3600),
			},
			proposal:         ssh("git.corp.example", "corp_key", false, 3600),
			pairingInCeiling: true,
		},
		{
			// R1 F292, shape (a), handed over by lane core: the two ceiling
			// shapes the COMPOSER-side mutants live under, asserted here for the
			// half the composer cannot reach. composer/grantbound_test.go pins
			// what Clamp PRODUCES for these (repos [org/alpha], ttl 300,
			// approval forced true); it cannot pin that internal/api's
			// governanceGrantsWithinCeiling gives the same answer for the same
			// input, because the composer must not import internal/api. That
			// AGREEMENT is this table's whole subject, so the two shapes belong
			// in both places.
			//
			// OVERLAPPING repo sets, not disjoint ones: [alpha,beta] and [alpha]
			// share a repo, so the permissive entry dominates the proposal's
			// SCOPE while the strict one holds the tighter ttl and the approval
			// requirement. No single entry dominates on every axis — the
			// permissive one is out-approved, the strict one is out-scoped — so
			// the comparator refuses and only D2 applies: whatever the clamp
			// keeps must be something a single ceiling entry actually wrote.
			name: "github_token: overlapping repo sets, split ttl and approval",
			ceiling: []types.GrantSpec{
				gh([]string{"org/alpha", "org/beta"}, map[string]string{"contents": "write"}, true, 3600),
				gh([]string{"org/alpha"}, map[string]string{"contents": "write"}, false, 300),
			},
			proposal:         gh([]string{"org/alpha", "org/beta"}, map[string]string{"contents": "write"}, false, 600),
			pairingInCeiling: true,
		},
		{
			// R1 F292, shape (b). The same overlap with the NEGATIVE ttl the
			// case below covers for api_key — and it is worth having twice,
			// because the two kinds clamp their scope by different code
			// (clampGitHubScope re-marshals a repo list; api_key's scope is
			// compared whole), so a ttl rule that holds for one is not evidence
			// about the other.
			name: "github_token: a negative ttl_seconds against overlapping repo sets",
			ceiling: []types.GrantSpec{
				gh([]string{"org/alpha", "org/beta"}, map[string]string{"contents": "write"}, false, 300),
				gh([]string{"org/alpha"}, map[string]string{"contents": "write"}, false, 600),
			},
			proposal:         gh([]string{"org/alpha", "org/beta"}, map[string]string{"contents": "write"}, false, -1),
			pairingInCeiling: true,
		},
		{
			// The comparator resolves a NEGATIVE ttl_seconds to the broker maximum
			// (governance_grantbound.go's normalizeGrantTTLSeconds) and refuses it
			// under a 300s ceiling. The clamp tested `== 0` and passed the negative
			// through, so it kept a grant the comparator refuses — D2's failure.
			name:             "api_key: a negative ttl_seconds",
			ceiling:          []types.GrantSpec{apiKey(hostA, "secret-a", false, 300)},
			proposal:         apiKey(hostA, "secret-a", false, -1),
			pairingInCeiling: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clampOnce := func(ceiling []types.GrantSpec) ([]types.GrantSpec, []string) {
				out, warns := composer.Clamp(
					types.RunPolicySpec{EligibleGrants: []types.GrantSpec{tc.proposal}},
					types.RunPolicySpec{EligibleGrants: ceiling},
				)
				return out.EligibleGrants, warns
			}
			clamped, warns := clampOnce(tc.ceiling)
			accepted := governanceGrantsWithinCeiling([]types.GrantSpec{tc.proposal}, tc.ceiling) == nil

			// D1.
			if accepted {
				if len(clamped) != 1 {
					t.Fatalf("the comparator ACCEPTS this proposal but the clamp dropped it (warns=%q)", warns)
				}
				if diff := grantDiff(t, tc.proposal, clamped[0]); diff != "" {
					t.Errorf("the comparator ACCEPTS this proposal unchanged, but the clamp narrowed it: %s (warns=%q)", diff, warns)
				}
			}

			// D2.
			if len(clamped) == 1 && tc.pairingInCeiling {
				if err := governanceGrantsWithinCeiling(clamped, tc.ceiling); err != nil {
					t.Errorf("the clamp kept a grant the comparator refuses: %v (warns=%q)", err, warns)
				}
			}

			// Order independence — a ceiling is a set.
			reversedCeiling := make([]types.GrantSpec, 0, len(tc.ceiling))
			for i := len(tc.ceiling) - 1; i >= 0; i-- {
				reversedCeiling = append(reversedCeiling, tc.ceiling[i])
			}
			reversed, _ := clampOnce(reversedCeiling)
			if len(reversed) != len(clamped) {
				t.Fatalf("reversing the ceiling changed how many grants survived: %d vs %d", len(reversed), len(clamped))
			}
			if len(clamped) == 1 {
				if diff := grantDiff(t, clamped[0], reversed[0]); diff != "" {
					t.Errorf("the same ceiling SET clamped differently in the two slice orders: %s", diff)
				}
			}
		})
	}
}

// grantDiff reports how two grants differ on the axes a clamp can move, reading
// the scope semantically because clampGitHubScope re-marshals it.
func grantDiff(t *testing.T, want, got types.GrantSpec) string {
	t.Helper()
	if want.Kind != got.Kind {
		return "kind " + string(want.Kind) + " -> " + string(got.Kind)
	}
	if want.RequiresApproval != got.RequiresApproval {
		return "requires_approval changed"
	}
	if normalizeGrantTTLSeconds(want.TTLSeconds) != normalizeGrantTTLSeconds(got.TTLSeconds) {
		return "ttl_seconds changed"
	}
	var a, b any
	if err := json.Unmarshal(want.Scope, &a); err != nil {
		t.Fatalf("decode scope %s: %v", want.Scope, err)
	}
	if err := json.Unmarshal(got.Scope, &b); err != nil {
		t.Fatalf("decode scope %s: %v", got.Scope, err)
	}
	if !reflect.DeepEqual(a, b) {
		return "scope " + string(want.Scope) + " -> " + string(got.Scope)
	}
	return ""
}
