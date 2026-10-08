// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// github_capabilities follows azure_devops_capabilities exactly: Clamp keeps
// a choice as proposed (dispatch, which knows the row, bounds it) and owns
// its slice, and an unset choice stays unset.
func TestClamp_GitHubCapabilitiesPassThrough(t *testing.T) {
	r, pr, ra := ghscope.CapCodeRead, ghscope.CapPR, ghscope.CapRepoAdmin
	for name, ceilingCaps := range map[string][]ghscope.Capability{"silent": nil, "disjoint": {pr}, "wider": {r, pr, ra}} {
		ceiling := operatorCeiling(t)
		ceiling.GitHubCapabilities = ceilingCaps
		proposed := types.RunPolicySpec{GitHubCapabilities: []ghscope.Capability{r, ra}}
		got, warns := Clamp(proposed, ceiling, types.GovernanceLimits{})
		if !slices.Equal(got.GitHubCapabilities, proposed.GitHubCapabilities) || hasWarn(warns, "github") {
			t.Errorf("%s ceiling: %v (warns %v), want the choice unchanged", name, got.GitHubCapabilities, warns)
		}
		got.GitHubCapabilities[0] = pr
		if proposed.GitHubCapabilities[0] != r {
			t.Errorf("%s ceiling: the clamped spec aliases the proposal's slice", name)
		}
		if got, _ := Clamp(types.RunPolicySpec{}, ceiling, types.GovernanceLimits{}); got.GitHubCapabilities != nil {
			t.Errorf("%s ceiling: unset proposal = %v, want unset (never inherited)", name, got.GitHubCapabilities)
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
