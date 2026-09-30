// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// azure_devops_capabilities is bounded at dispatch, where the row's
// default_profile is known, so Clamp keeps a choice exactly as proposed — an
// explicit list must not arrive at dispatch narrowed to nothing, which reads as
// "use the default" — and the clamped spec owns its slice.
func TestClamp_ADOCapabilitiesPassThrough(t *testing.T) {
	r, pr, pa := adoscope.CapCodeRead, adoscope.CapPR, adoscope.CapPolicyAdmin
	for name, ceilingCaps := range map[string][]adoscope.Capability{"silent": nil, "disjoint": {pr}, "wider": {r, pr, pa}} {
		ceiling := operatorCeiling(t)
		ceiling.AzureDevOpsCapabilities = ceilingCaps
		proposed := types.RunPolicySpec{AzureDevOpsCapabilities: []adoscope.Capability{r, pa}}
		got, warns := Clamp(proposed, ceiling, 0)
		if !slices.Equal(got.AzureDevOpsCapabilities, proposed.AzureDevOpsCapabilities) || hasWarn(warns, "azure_devops") {
			t.Errorf("%s ceiling: %v (warns %v), want the choice unchanged", name, got.AzureDevOpsCapabilities, warns)
		}
		got.AzureDevOpsCapabilities[0] = pr
		if proposed.AzureDevOpsCapabilities[0] != r {
			t.Errorf("%s ceiling: the clamped spec aliases the proposal's slice", name)
		}
		if got, _ := Clamp(types.RunPolicySpec{}, ceiling, 0); got.AzureDevOpsCapabilities != nil {
			t.Errorf("%s ceiling: unset proposal = %v, want unset (never inherited)", name, got.AzureDevOpsCapabilities)
		}
	}
}
