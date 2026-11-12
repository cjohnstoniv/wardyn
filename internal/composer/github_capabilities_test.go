// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A ceiling's github_capabilities bounds the proposal: entries it does not list
// are dropped with a warning, in the proposal's order; a proposal that names none
// takes the ceiling's list; and the result is within the ceiling. A ceiling that
// names none leaves the choice as proposed. The clamped spec owns its slice.
func TestClamp_GitHubCapabilitiesBoundedByTheCeiling(t *testing.T) {
	r, w, pr, ra := ghscope.CapCodeRead, ghscope.CapCodeWrite, ghscope.CapPR, ghscope.CapRepoAdmin
	caps := func(c ...ghscope.Capability) []ghscope.Capability { return c }
	for _, tc := range []struct {
		name              string
		proposed, ceiling []ghscope.Capability
		want              []ghscope.Capability
		warn              bool
	}{
		{"wider than the ceiling", caps(r, w), caps(r), caps(r), true},
		{"none named", nil, caps(r), caps(r), false},
		{"within the ceiling", caps(pr, r), caps(r, w, pr), caps(ghscope.CapPRWrite, ghscope.CapPRReview, r), false},
		{"order kept", caps(ra, pr, r), caps(r, pr), caps(ghscope.CapPRWrite, ghscope.CapPRReview, r), true},
		{"nothing in common", caps(pr), caps(r), caps(r), true},
		{"ceiling silent", caps(r, ra), nil, caps(r, ra), false},
	} {
		ceiling := operatorCeiling(t)
		ceiling.GitHubCapabilities = tc.ceiling
		proposed := types.RunPolicySpec{GitHubCapabilities: tc.proposed}
		got, warns := Clamp(proposed, ceiling, types.GovernanceLimits{})
		if !slices.Equal(got.GitHubCapabilities, tc.want) || hasWarn(warns, "github_capabilities") != tc.warn {
			t.Errorf("%s: %v (warns %v), want %v with warning %v", tc.name, got.GitHubCapabilities, warns, tc.want, tc.warn)
		}
		if tc.ceiling != nil && !leqPosture(got, ceiling) {
			t.Errorf("%s: the clamped list %v is not within the ceiling's %v", tc.name, got.GitHubCapabilities, tc.ceiling)
		}
		if len(got.GitHubCapabilities) > 0 {
			got.GitHubCapabilities[0] = ghscope.CapOrgAdmin
			if slices.Contains(tc.proposed, ghscope.CapOrgAdmin) || slices.Contains(ceiling.GitHubCapabilities, ghscope.CapOrgAdmin) {
				t.Errorf("%s: the clamped spec aliases the proposal's or the ceiling's slice", tc.name)
			}
		}
	}
}

// The spec's own example, through the whole of Leq: a member asking for
// code_read and code_write under a ceiling of code_read gets code_read, a
// warning, and a spec Leq calls within the ceiling.
func TestClamp_GitHubCapabilitiesLeqTheCeiling(t *testing.T) {
	ceiling := operatorCeiling(t)
	ceiling.GitHubCapabilities = []ghscope.Capability{ghscope.CapCodeRead}
	for _, proposed := range [][]ghscope.Capability{{ghscope.CapCodeRead, ghscope.CapCodeWrite}, nil} {
		got, warns := Clamp(types.RunPolicySpec{GitHubCapabilities: proposed}, ceiling, types.GovernanceLimits{})
		if !slices.Equal(got.GitHubCapabilities, []ghscope.Capability{ghscope.CapCodeRead}) {
			t.Errorf("proposed %v: %v (warns %v), want [code_read]", proposed, got.GitHubCapabilities, warns)
		}
		if proposed != nil && !hasWarn(warns, "dropped code_write") {
			t.Errorf("proposed %v: warns %v, want one naming the dropped code_write", proposed, warns)
		}
		if !Leq(got, ceiling, types.GovernanceLimits{}, types.GovernanceLimits{}) {
			t.Errorf("proposed %v: Leq(clamped, ceiling) = false, want true", proposed)
		}
	}
}

// Narrow-only, over random subsets of the catalogue: the clamped list is within
// the ceiling's, and within the proposal's wherever the two share an entry.
func TestClamp_GitHubCapabilitiesNarrowOnly(t *testing.T) {
	all := []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapCodeWrite, ghscope.CapWorkflowsWrite, ghscope.CapPR,
		ghscope.CapIssuesRead, ghscope.CapIssuesWrite, ghscope.CapActionsRead, ghscope.CapRepoAdmin}
	rng := rand.New(rand.NewPCG(1, 2))
	subset := func() []ghscope.Capability {
		var out []ghscope.Capability
		for _, c := range all {
			if rng.IntN(2) == 0 {
				out = append(out, c)
			}
		}
		return out
	}
	within := func(a, b []ghscope.Capability) bool {
		return !slices.ContainsFunc(a, func(c ghscope.Capability) bool { return !ghscope.Has(b, c) })
	}
	for i := 0; i < 500; i++ {
		proposed, ceilingCaps := subset(), subset()
		if len(ceilingCaps) == 0 {
			continue
		}
		ceiling := operatorCeiling(t)
		ceiling.GitHubCapabilities = ceilingCaps
		got, _ := Clamp(types.RunPolicySpec{GitHubCapabilities: proposed}, ceiling, types.GovernanceLimits{})
		if len(got.GitHubCapabilities) == 0 || !within(got.GitHubCapabilities, ceilingCaps) {
			t.Fatalf("proposed %v ceiling %v: %v, want a non-empty list within the ceiling", proposed, ceilingCaps, got.GitHubCapabilities)
		}
		shared := slices.ContainsFunc(ghscope.ExpandPR(proposed), func(c ghscope.Capability) bool { return ghscope.Has(ceilingCaps, c) })
		if shared && !within(got.GitHubCapabilities, proposed) {
			t.Fatalf("proposed %v ceiling %v: %v, want it within the proposal too", proposed, ceilingCaps, got.GitHubCapabilities)
		}
	}
}

// The overlay meet: intersection; disjoint is unsatisfiable; an empty overlay
// list is refused; a list over an empty base is a widening that leaves the
// base empty. The Azure DevOps list is untouched by a GitHub overlay.
func TestMeet_GitHubCapabilities(t *testing.T) {
	caps := func(c ...ghscope.Capability) *[]ghscope.Capability { return &c }
	base := Authority{Ceiling: types.RunPolicySpec{GitHubCapabilities: []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapIssuesRead}}}
	_, _, err := ApplyOverlay(base, overlayOf(types.CeilingOverlay{GitHubCapabilities: caps(ghscope.CapPR)}))
	wantOverlayErr(t, err, ReasonOverlayUnsatisfiable, "github_capabilities")
	_, _, err = ApplyOverlay(base, overlayOf(types.CeilingOverlay{GitHubCapabilities: caps()}))
	wantOverlayErr(t, err, ReasonOverlayInvalid, "github_capabilities")
	got := mustApply(t, base, overlayOf(types.CeilingOverlay{GitHubCapabilities: caps(ghscope.CapCodeRead)}))
	if !slices.Equal(got.Ceiling.GitHubCapabilities, []ghscope.Capability{ghscope.CapCodeRead}) || got.Ceiling.AzureDevOpsCapabilities != nil {
		t.Errorf("meet = github %v, ado %v", got.Ceiling.GitHubCapabilities, got.Ceiling.AzureDevOpsCapabilities)
	}
	ov := overlayOf(types.CeilingOverlay{GitHubCapabilities: caps(ghscope.CapCodeRead, ghscope.CapPR)})
	wantOverlayErr(t, ValidateOverlay(base, ov), ReasonOverlayInvalid, "github_capabilities")
	got, warns, err := ApplyOverlay(base, ov)
	if err != nil || len(warns) == 0 || !slices.Equal(got.Ceiling.GitHubCapabilities, []ghscope.Capability{ghscope.CapCodeRead}) {
		t.Errorf("lenient meet = %v, warns %v, err %v; want code_read and a warning", got.Ceiling.GitHubCapabilities, warns, err)
	}
	empty := overlayOf(types.CeilingOverlay{GitHubCapabilities: caps(ghscope.CapCodeRead)})
	wantOverlayErr(t, ValidateOverlay(Authority{}, empty), ReasonOverlayInvalid, "github_capabilities")
	got, warns, err = ApplyOverlay(Authority{}, empty)
	if err != nil || len(warns) == 0 || got.Ceiling.GitHubCapabilities != nil {
		t.Errorf("resolve = %v, warns %v, err %v; want the empty base kept and a warning", got.Ceiling.GitHubCapabilities, warns, err)
	}
}

// Leq reads github_capabilities with azure_devops_capabilities' rule.
func TestLeq_GitHubCapabilities(t *testing.T) {
	spec := func(c ...ghscope.Capability) types.RunPolicySpec { return types.RunPolicySpec{GitHubCapabilities: c} }
	for _, tc := range []struct {
		a, b types.RunPolicySpec
		want bool
	}{
		{spec(), spec(), true},
		{spec(ghscope.CapCodeRead), spec(), false},
		{spec(), spec(ghscope.CapCodeRead), false},
		{spec(ghscope.CapCodeRead), spec(ghscope.CapCodeRead, ghscope.CapPR), true},
		{spec(ghscope.CapCodeRead, ghscope.CapPR), spec(ghscope.CapCodeRead), false},
	} {
		if got := leqPosture(tc.a, tc.b); got != tc.want {
			t.Errorf("leqPosture(%v, %v) = %v, want %v", tc.a.GitHubCapabilities, tc.b.GitHubCapabilities, got, tc.want)
		}
	}
}
