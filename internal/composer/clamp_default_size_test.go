// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A nil-ceiling Clamp caps an over-asking request at the deployment's default size.
func TestClampNilCeilingUsesEffectiveLimits(t *testing.T) {
	t.Cleanup(func() { runner.SetDefaultLimits(0, 0) })
	for _, tc := range []struct {
		name             string
		cpu, mem         int64
		wantCPU, wantMem int
	}{
		{"unset", 0, 0, 2000, 4096},
		{"knob", 1000, 2048, 1000, 2048},
	} {
		runner.SetDefaultLimits(tc.cpu, tc.mem)
		out, _ := Clamp(types.RunPolicySpec{Resources: &types.ResourceLimits{CPUMillis: 64000, MemoryMiB: 1 << 20}}, types.RunPolicySpec{}, types.GovernanceLimits{})
		if out.Resources == nil || out.Resources.CPUMillis != tc.wantCPU || out.Resources.MemoryMiB != tc.wantMem {
			t.Errorf("%s: Resources = %+v, want %d/%d", tc.name, out.Resources, tc.wantCPU, tc.wantMem)
		}
	}
}
