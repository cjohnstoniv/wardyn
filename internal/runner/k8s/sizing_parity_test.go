// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// What the pod carries is exactly runner.EffectiveResources, for a policy-set 3000m/6144Mi
// run and a default run, under two ratio and proxy settings.
func TestPodMatchesEffectiveResources(t *testing.T) {
	t.Cleanup(func() { _ = runner.SetRequestRatio(0); runner.SetProxyLimits(0, 0) })
	for _, ratio := range []float64{0, 0.5} {
		for _, pxy := range []int64{500, 750} {
			if err := runner.SetRequestRatio(ratio); err != nil {
				t.Fatal(err)
			}
			runner.SetProxyLimits(pxy, 300)
			for _, res := range []runner.Resources{{CPUMillis: 3000, MemoryMiB: 6144}, {}} {
				sz := runner.EffectiveResources(res)
				a := resourceRequirements(res)
				if a.Requests.Cpu().MilliValue() != sz.AgentCPURequestMillis || a.Limits.Cpu().MilliValue() != sz.AgentCPULimitMillis ||
					a.Requests.Memory().Value() != sz.AgentMemoryRequestMiB<<20 || a.Limits.Memory().Value() != sz.AgentMemoryLimitMiB<<20 {
					t.Errorf("ratio %v res %+v: agent %v/%v, sizing %+v", ratio, res, a.Requests, a.Limits, sz)
				}
				p := proxyResources(false)
				if p.Limits.Cpu().MilliValue() != sz.ProxyCPUMillis || p.Limits.Memory().Value() != sz.ProxyMemoryMiB<<20 {
					t.Errorf("proxy %v, sizing %+v", p.Limits, sz)
				}
			}
		}
	}
}
