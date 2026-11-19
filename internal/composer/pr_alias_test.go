// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pr is an alias for its parts wherever the composer compares capability lists: a part of pr is
// within pr, and pr is within its parts together.

func TestMeetCapabilitiesPRAlias(t *testing.T) {
	ado := func(c ...adoscope.Capability) *[]adoscope.Capability { return &c }
	gh := func(c ...ghscope.Capability) *[]ghscope.Capability { return &c }

	base := Authority{Ceiling: types.RunPolicySpec{AzureDevOpsCapabilities: []adoscope.Capability{adoscope.CapPR},
		GitHubCapabilities: []ghscope.Capability{ghscope.CapPR}}}
	narrow := overlayOf(types.CeilingOverlay{AzureDevOpsCapabilities: ado(adoscope.CapPRCreate), GitHubCapabilities: gh(ghscope.CapPRWrite)})
	if err := ValidateOverlay(base, narrow); err != nil {
		t.Fatalf("a part under pr is a widening: %v", err)
	}
	got := mustApply(t, base, narrow)
	if !slices.Equal(got.Ceiling.AzureDevOpsCapabilities, []adoscope.Capability{adoscope.CapPRCreate}) ||
		!slices.Equal(got.Ceiling.GitHubCapabilities, []ghscope.Capability{ghscope.CapPRWrite}) {
		t.Errorf("pr met with a part = ado %v, github %v; want the part", got.Ceiling.AzureDevOpsCapabilities, got.Ceiling.GitHubCapabilities)
	}

	base = Authority{Ceiling: types.RunPolicySpec{AzureDevOpsCapabilities: []adoscope.Capability{adoscope.CapPRCreate},
		GitHubCapabilities: []ghscope.Capability{ghscope.CapPRWrite}}}
	wide := overlayOf(types.CeilingOverlay{AzureDevOpsCapabilities: ado(adoscope.CapPR), GitHubCapabilities: gh(ghscope.CapPR)})
	wantOverlayErr(t, ValidateOverlay(base, wide), ReasonOverlayInvalid, "azure_devops_capabilities")
	got, warns, err := ApplyOverlay(base, wide)
	if err != nil || len(warns) == 0 ||
		!slices.Equal(got.Ceiling.AzureDevOpsCapabilities, []adoscope.Capability{adoscope.CapPRCreate}) ||
		!slices.Equal(got.Ceiling.GitHubCapabilities, []ghscope.Capability{ghscope.CapPRWrite}) {
		t.Errorf("a part met with pr = ado %v, github %v, warns %v, err %v; want the part and a widening",
			got.Ceiling.AzureDevOpsCapabilities, got.Ceiling.GitHubCapabilities, warns, err)
	}
}

func TestLeqCapabilitiesPRAlias(t *testing.T) {
	ado := func(c ...adoscope.Capability) types.RunPolicySpec {
		return types.RunPolicySpec{AzureDevOpsCapabilities: c}
	}
	gh := func(c ...ghscope.Capability) types.RunPolicySpec { return types.RunPolicySpec{GitHubCapabilities: c} }
	for _, tc := range []struct {
		name string
		a, b types.RunPolicySpec
		want bool
	}{
		{"ado part under pr", ado(adoscope.CapPRVote), ado(adoscope.CapPR), true},
		{"ado pr under its parts", ado(adoscope.CapPR), ado(adoscope.CapPRCreate, adoscope.CapPRVote, adoscope.CapPRComplete), true},
		{"ado pr under one part", ado(adoscope.CapPR), ado(adoscope.CapPRCreate), false},
		{"github part under pr", gh(ghscope.CapPRReview), gh(ghscope.CapPR), true},
		{"github pr under its parts", gh(ghscope.CapPR), gh(ghscope.CapPRWrite, ghscope.CapPRReview), true},
		{"github pr under one part", gh(ghscope.CapPR), gh(ghscope.CapPRWrite), false},
	} {
		if got := leqPosture(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: leqPosture = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestClampGitHubCapabilitiesPRAlias(t *testing.T) {
	ceiling := types.RunPolicySpec{GitHubCapabilities: []ghscope.Capability{ghscope.CapCodeRead, ghscope.CapPRWrite}}
	got, warns := Clamp(types.RunPolicySpec{GitHubCapabilities: []ghscope.Capability{ghscope.CapPR}}, ceiling, types.GovernanceLimits{})
	if !slices.Equal(got.GitHubCapabilities, []ghscope.Capability{ghscope.CapPRWrite}) {
		t.Errorf("pr under [code_read pr_write] = %v, want pr_write", got.GitHubCapabilities)
	}
	if !slices.ContainsFunc(warns, func(w string) bool { return strings.Contains(w, "dropped pr_review") }) {
		t.Errorf("warns %v, want pr_review named as dropped", warns)
	}
	ceiling = types.RunPolicySpec{GitHubCapabilities: []ghscope.Capability{ghscope.CapPR}}
	got, warns = Clamp(types.RunPolicySpec{GitHubCapabilities: []ghscope.Capability{ghscope.CapPRWrite}}, ceiling, types.GovernanceLimits{})
	if !slices.Equal(got.GitHubCapabilities, []ghscope.Capability{ghscope.CapPRWrite}) ||
		slices.ContainsFunc(warns, func(w string) bool { return strings.Contains(w, "github_capabilities") }) {
		t.Errorf("pr_write under [pr] = %v, warns %v; want pr_write kept with no drop", got.GitHubCapabilities, warns)
	}
}
