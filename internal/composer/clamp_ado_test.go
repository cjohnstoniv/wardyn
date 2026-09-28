// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// azure_devops_capabilities can widen a run past its row's default_profile, so
// a proposal keeps only what the ceiling's own list names — and the clamped
// spec owns its slice.
func TestClamp_ADOCapabilities(t *testing.T) {
	r, cw, pr, pa := adoscope.CapRead, adoscope.CapCodeWrite, adoscope.CapPR, adoscope.CapPolicyAdmin
	ceiling := operatorCeiling(t)
	ceiling.AzureDevOpsCapabilities = []adoscope.Capability{r, cw, pr}

	proposed := types.RunPolicySpec{AzureDevOpsCapabilities: []adoscope.Capability{r, pa, pr}}
	got, warns := Clamp(proposed, ceiling, 0)
	if !slices.Equal(got.AzureDevOpsCapabilities, []adoscope.Capability{r, pr}) {
		t.Errorf("intersected = %v, want [read pr]", got.AzureDevOpsCapabilities)
	}
	if !hasWarn(warns, "policy_admin") {
		t.Errorf("no warning names the dropped capability: %v", warns)
	}
	got.AzureDevOpsCapabilities[0] = pa
	if proposed.AzureDevOpsCapabilities[0] != r || ceiling.AzureDevOpsCapabilities[0] != r {
		t.Error("the clamped spec aliases an argument's slice")
	}

	// The ceiling's list is a bound, never a grant: nothing chosen, or nothing
	// chosen inside it, leaves the field unset (the row's default_profile).
	got, warns = Clamp(types.RunPolicySpec{}, ceiling, 0)
	if got.AzureDevOpsCapabilities != nil || hasWarn(warns, "azure_devops") {
		t.Errorf("unset proposal = %v (warns %v), want unset and no warning", got.AzureDevOpsCapabilities, warns)
	}
	got, warns = Clamp(types.RunPolicySpec{AzureDevOpsCapabilities: []adoscope.Capability{pa}}, ceiling, 0)
	if got.AzureDevOpsCapabilities != nil || !hasWarn(warns, "azure_devops_capabilities dropped") {
		t.Errorf("disjoint proposal = %v (warns %v), want unset with a warning", got.AzureDevOpsCapabilities, warns)
	}
	silent := operatorCeiling(t)
	got, warns = Clamp(proposed, silent, 0)
	if got.AzureDevOpsCapabilities != nil || !hasWarn(warns, "azure_devops_capabilities dropped") {
		t.Errorf("silent ceiling: %v (warns %v), want the choice dropped with a warning", got.AzureDevOpsCapabilities, warns)
	}
	if _, warns := Clamp(types.RunPolicySpec{}, silent, 0); hasWarn(warns, "azure_devops") {
		t.Errorf("nothing to clamp, yet warned: %v", warns)
	}
}
