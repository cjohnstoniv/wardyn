// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The k8s fallbacks read runner.EffectiveLimits/ProxyLimits, the same source the docker
// substrate reads (see docker TestDefaultSizeFollowsEffectiveLimits), so the two apply one size.
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
		r := resourceRequirements(runner.Resources{}).Limits
		if r.Cpu().MilliValue() != want.CPUMillis || r.Memory().Value() != want.MemoryMiB*1024*1024 {
			t.Errorf("%s: resourceRequirements limits = %v, want %+v", tc.name, r, want)
		}
		pc, pm := runner.ProxyLimits()
		p := proxyResources().Limits
		if p.Cpu().MilliValue() != pc || p.Memory().Value() != pm*1024*1024 {
			t.Errorf("%s: proxyResources = %v, want %d/%d", tc.name, p, pc, pm)
		}
	}
}
