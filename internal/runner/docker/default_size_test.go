// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The docker fallbacks read runner.EffectiveLimits/ProxyLimits, the same source the k8s
// substrate reads (see k8s TestDefaultSizeFollowsEffectiveLimits), so the two apply one size.
func TestDefaultSizeFollowsEffectiveLimits(t *testing.T) {
	t.Cleanup(func() { runner.SetDefaultLimits(0, 0); runner.SetProxyLimits(0, 0) })
	for _, tc := range []struct {
		name                 string
		cpu, mem, pcpu, pmem int64
	}{
		{"unset", 0, 0, 0, 0},
		{"knob", 1000, 2048, 250, 128},
	} {
		runner.SetDefaultLimits(tc.cpu, tc.mem)
		runner.SetProxyLimits(tc.pcpu, tc.pmem)
		want := runner.EffectiveLimits()
		r := resourcesFromSpec(runner.Resources{})
		if r.NanoCPUs != want.CPUMillis*1_000_000 || r.Memory != want.MemoryMiB*1024*1024 {
			t.Errorf("%s: resourcesFromSpec = %d nanos / %d bytes, want %+v", tc.name, r.NanoCPUs, r.Memory, want)
		}
		pc, pm := runner.ProxyLimits()
		p := proxyResources(false)
		if p.NanoCPUs != pc*1_000_000 || p.Memory != pm*1024*1024 {
			t.Errorf("%s: proxyResources = %d nanos / %d bytes, want %d/%d", tc.name, p.NanoCPUs, p.Memory, pc, pm)
		}
	}
}
