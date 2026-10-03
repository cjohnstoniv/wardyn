// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A profile's CPU/memory maximum bounds a request AND a fill from the ceiling.
func TestClampProfileSizeMaximum(t *testing.T) {
	ceiling := operatorCeiling(t)
	ceiling.Resources = &types.ResourceLimits{CPUMillis: 64000, MemoryMiB: 1 << 20}
	limits := types.GovernanceLimits{MaxCPUMillis: 4000, MaxMemoryMiB: 8192}

	got, warns := Clamp(types.RunPolicySpec{MinConfinementClass: types.CC2}, ceiling, limits)
	if got.Resources.CPUMillis != 4000 || got.Resources.MemoryMiB != 8192 {
		t.Errorf("fill = %+v, want 4000/8192", got.Resources)
	}
	if hasWarn(warns, WarnResourcesCapped) {
		t.Errorf("a fill nobody asked for is not a cut: %v", warns)
	}

	got, warns = Clamp(types.RunPolicySpec{MinConfinementClass: types.CC2,
		Resources: &types.ResourceLimits{CPUMillis: 16000}}, ceiling, limits)
	if got.Resources.CPUMillis != 4000 || !hasWarn(warns, WarnResourcesCapped) {
		t.Errorf("request = %+v warns=%v, want 4000 and the capped warning", got.Resources, warns)
	}

	got, _ = Clamp(types.RunPolicySpec{MinConfinementClass: types.CC2,
		Resources: &types.ResourceLimits{CPUMillis: 16000}}, ceiling, types.GovernanceLimits{})
	if got.Resources.CPUMillis != 16000 {
		t.Errorf("no limit: cpu = %d, want the request kept at 16000", got.Resources.CPUMillis)
	}
}

func TestCapResources(t *testing.T) {
	t.Cleanup(func() { runner.SetDefaultLimits(0, 0) })
	runner.SetDefaultLimits(1000, 2048)
	lim := types.GovernanceLimits{MaxCPUMillis: 500, MaxMemoryMiB: 4096}

	// An unset field stands at the deployment size: cut when that is above the maximum.
	got, changed := CapResources(nil, lim)
	if !changed || got.CPUMillis != 500 || got.MemoryMiB != 0 {
		t.Errorf("nil = %+v changed=%v, want cpu 500 and memory left to the default", got, changed)
	}
	orig := &types.ResourceLimits{CPUMillis: 64000, MemoryMiB: 1 << 20, DiskMiB: 7}
	got, changed = CapResources(orig, lim)
	if !changed || got.CPUMillis != 500 || got.MemoryMiB != 4096 || got.DiskMiB != 7 {
		t.Errorf("set = %+v, want 500/4096, disk untouched", got)
	}
	if orig.CPUMillis != 64000 {
		t.Error("CapResources wrote through the caller's block")
	}
	if same, changed := CapResources(orig, types.GovernanceLimits{}); changed || same != orig {
		t.Error("no limit must return the block untouched")
	}
}
