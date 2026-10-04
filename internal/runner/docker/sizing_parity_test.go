// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// What the containers carry is exactly runner.EffectiveResources' limits and proxy envelope.
func TestContainersMatchEffectiveResources(t *testing.T) {
	t.Cleanup(func() { runner.SetProxyLimits(0, 0) })
	for _, pxy := range []int64{500, 750} {
		runner.SetProxyLimits(pxy, 300)
		for _, res := range []runner.Resources{{CPUMillis: 3000, MemoryMiB: 6144}, {}} {
			sz := runner.EffectiveResources(res)
			a := resourcesFromSpec(res)
			if a.NanoCPUs != sz.AgentCPULimitMillis*1_000_000 || a.Memory != sz.AgentMemoryLimitMiB<<20 {
				t.Errorf("res %+v: agent %d nanos / %d bytes, sizing %+v", res, a.NanoCPUs, a.Memory, sz)
			}
			p := proxyResources(false)
			if p.NanoCPUs != sz.ProxyCPUMillis*1_000_000 || p.Memory != sz.ProxyMemoryMiB<<20 {
				t.Errorf("proxy %d nanos / %d bytes, sizing %+v", p.NanoCPUs, p.Memory, sz)
			}
		}
	}
}
