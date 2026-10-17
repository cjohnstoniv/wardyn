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
	floor := MemberDefault(types.ResourceLimits{}, nil)

	// An unset field stands at the deployment size, which the member cap is never below (#1949).
	got, changed := CapResources(nil, lim, floor)
	if changed || got != nil {
		t.Errorf("nil = %+v changed=%v, want the deployment default left alone", got, changed)
	}
	// ... but with no floor above it, a maximum below the default also bounds a run that set no size.
	got, changed = CapResources(nil, lim, types.ResourceLimits{})
	if !changed || got == nil || *got != (types.ResourceLimits{CPUMillis: 500}) {
		t.Errorf("nil under a 500 maximum, no floor = %+v changed=%v, want a fresh 500-CPU block with memory left unset", got, changed)
	}
	orig := &types.ResourceLimits{CPUMillis: 64000, MemoryMiB: 1 << 20, DiskMiB: 7}
	got, changed = CapResources(orig, lim, floor)
	if !changed || got.CPUMillis != 1000 || got.MemoryMiB != 4096 || got.DiskMiB != 7 {
		t.Errorf("set = %+v, want 1000 (the default over a 500 maximum)/4096, disk untouched", got)
	}
	if orig.CPUMillis != 64000 {
		t.Error("CapResources wrote through the caller's block")
	}
	if same, changed := CapResources(orig, types.GovernanceLimits{}, floor); changed || same != orig {
		t.Error("no limit must return the block untouched")
	}
}

// TestClampUsesMemberCap pins #1949 in Clamp and CapResources: a profile maximum counts as the
// ceiling's size, so with no ceiling size, a maximum of 8 CPU and a deployment default of 2 an inline
// ask of 8 keeps 8; and the cap never falls below the default, so a maximum under it cuts nothing.
func TestClampUsesMemberCap(t *testing.T) {
	t.Cleanup(func() { runner.SetDefaultLimits(0, 0) })
	runner.SetDefaultLimits(2000, 4096)
	eight := types.GovernanceLimits{MaxCPUMillis: 8000}
	def := MemberDefault(types.ResourceLimits{}, nil)
	got, warns := Clamp(types.RunPolicySpec{Resources: &types.ResourceLimits{CPUMillis: 8000}}, types.RunPolicySpec{}, eight)
	if got.Resources.CPUMillis != 8000 || hasWarn(warns, WarnResourcesCapped) {
		t.Errorf("Clamp ask 8000 under max 8000 = %+v warns=%v, want 8000 kept", got.Resources, warns)
	}
	got, _ = Clamp(types.RunPolicySpec{}, types.RunPolicySpec{}, eight)
	if got.Resources.CPUMillis != 2000 {
		t.Errorf("Clamp fill = %+v, want the default 2000, never the cap", got.Resources)
	}
	if out, changed := CapResources(&types.ResourceLimits{CPUMillis: 8000}, eight, def); changed || out.CPUMillis != 8000 {
		t.Errorf("CapResources ask 8000 under max 8000 = %+v changed=%v, want 8000 kept", out, changed)
	}
	under := types.GovernanceLimits{MaxCPUMillis: 1000}
	if out, changed := CapResources(nil, under, def); changed || out != nil {
		t.Errorf("CapResources nil under max 1000 = %+v changed=%v, want the default left alone", out, changed)
	}
	if out, changed := CapResources(&types.ResourceLimits{CPUMillis: 4000}, under, def); !changed || out.CPUMillis != 2000 {
		t.Errorf("CapResources 4000 under max 1000 = %+v changed=%v, want 2000 (the default, never below it)", out, changed)
	}
}

// ClampMember reads the profile's AUTHORED size, not the deployment size the resolved ceiling
// inherited, and leaves the CPU and memory warning to the run's placement step.
func TestClampMemberReadsTheAuthoredSize(t *testing.T) {
	t.Cleanup(func() { runner.SetDefaultLimits(0, 0) })
	runner.SetDefaultLimits(2000, 4096)
	inherited := types.RunPolicySpec{Resources: &types.ResourceLimits{CPUMillis: 2000, MemoryMiB: 4096}}
	ask := types.RunPolicySpec{Resources: &types.ResourceLimits{CPUMillis: 8000}}
	got, warns := ClampMember(ask, inherited, types.GovernanceLimits{MaxCPUMillis: 8000}, types.ResourceLimits{}, MemberDefault(types.ResourceLimits{}, inherited.Resources))
	if got.Resources.CPUMillis != 8000 || hasWarn(warns, WarnResourcesCapped) {
		t.Errorf("max 8000, no authored size: %+v warns=%v, want 8000 kept", got.Resources, warns)
	}
	got, warns = ClampMember(ask, inherited, types.GovernanceLimits{MaxCPUMillis: 4000}, types.ResourceLimits{}, MemberDefault(types.ResourceLimits{}, inherited.Resources))
	if got.Resources.CPUMillis != 4000 || hasWarn(warns, WarnResourcesCapped) {
		t.Errorf("max 4000: %+v warns=%v, want 4000 and no warning here", got.Resources, warns)
	}
	if got, warns = Clamp(ask, inherited, types.GovernanceLimits{MaxCPUMillis: 8000}); got.Resources.CPUMillis != 2000 || !hasWarn(warns, WarnResourcesCapped) {
		t.Errorf("Clamp reads the ceiling's own size: %+v warns=%v, want 2000 and the warning", got.Resources, warns)
	}
}

// MemberDefault starts from the deployment's own size, not the platform's: a deployment whose default
// policy is above the platform default gives a member with no size that default, lowered only by a
// ceiling below it; and CapResources never cuts below it.
func TestMemberDefaultIsTheDeploymentsOwnSize(t *testing.T) {
	t.Cleanup(func() { runner.SetDefaultLimits(0, 0) })
	runner.SetDefaultLimits(2000, 4096)
	deployment := types.ResourceLimits{CPUMillis: 4000, MemoryMiB: 8192}
	if got := MemberDefault(deployment, &types.ResourceLimits{CPUMillis: 4000, MemoryMiB: 8192}); got != deployment {
		t.Errorf("member default = %+v, want the deployment's %+v", got, deployment)
	}
	if got := MemberDefault(deployment, &types.ResourceLimits{CPUMillis: 3000}); got.CPUMillis != 3000 || got.MemoryMiB != 8192 {
		t.Errorf("member default under a 3000 ceiling = %+v, want 3000 / 8192", got)
	}
	if got := MemberDefault(types.ResourceLimits{}, nil); got.CPUMillis != 2000 || got.MemoryMiB != 4096 {
		t.Errorf("member default with no deployment size = %+v, want the platform's", got)
	}
	if out, changed := CapResources(&types.ResourceLimits{CPUMillis: 4000, MemoryMiB: 8192}, types.GovernanceLimits{MaxCPUMillis: 3000}, deployment); changed {
		t.Errorf("CapResources cut %+v under a 3000 maximum, want the 4000 member default kept", out)
	}
}
