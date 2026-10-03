// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Two same-pairing ceiling grants, one wide on repos and one wide on access, never
// combine into a proposal wide on both: no single grant dominates it, and the
// clamp narrows it to the meet of both, in whichever order they are listed.
func TestClampGitPATNeedsOneDominatingGrant(t *testing.T) {
	wideRepos := patGrant(`"access":"read"`)                        // every repository, read only
	wideAccess := patGrant(`"repos":["team/app"],"access":"write"`) // one repository, write
	proposal := patGrant(`"access":"write"`)                        // every repository, write

	for _, cg := range []types.GrantSpec{wideRepos, wideAccess} {
		if PATScopeWithin(proposal.Scope, cg.Scope) == nil {
			t.Fatalf("%s alone dominates a proposal that needs both grants", cg.Scope)
		}
	}
	for name, order := range map[string][]types.GrantSpec{
		"repos grant first":  {wideRepos, wideAccess},
		"access grant first": {wideAccess, wideRepos},
	} {
		t.Run(name, func(t *testing.T) {
			out, warns := Clamp(types.RunPolicySpec{EligibleGrants: []types.GrantSpec{proposal}},
				types.RunPolicySpec{EligibleGrants: order}, types.GovernanceLimits{})
			if len(out.EligibleGrants) != 1 {
				t.Fatalf("grant dropped (%q)", warns)
			}
			sc, err := types.DecodeGitPATScope(out.EligibleGrants[0].Scope)
			if err != nil {
				t.Fatal(err)
			}
			if sc.Repos == nil || !slices.Equal(*sc.Repos, []string{"team/app"}) || sc.Access != types.PATAccessRead {
				t.Fatalf("scope = %s, want repos [team/app] and access read (the meet of both grants)", out.EligibleGrants[0].Scope)
			}
		})
	}
}

// An empty repos intersection drops the grant at Clamp: it is never kept as an
// unnarrowed one.
func TestClampGitPATEmptyIntersectionDropsTheGrant(t *testing.T) {
	out, warns := Clamp(
		types.RunPolicySpec{EligibleGrants: []types.GrantSpec{patGrant(`"repos":["team/other"]`)}},
		types.RunPolicySpec{EligibleGrants: []types.GrantSpec{patGrant(`"repos":["team/app"]`)}},
		types.GovernanceLimits{})
	if len(out.EligibleGrants) != 0 {
		t.Fatalf("kept %s", out.EligibleGrants[0].Scope)
	}
	if !slices.ContainsFunc(warns, func(w string) bool { return strings.Contains(w, "dropped git_pat grant") }) {
		t.Errorf("no drop warning in %q", warns)
	}
}
