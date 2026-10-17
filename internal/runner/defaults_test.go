// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "testing"

func TestEffectiveLimitsAndProxyLimits(t *testing.T) {
	t.Cleanup(func() { SetDefaultLimits(0, 0); SetProxyLimits(0, 0) })
	for _, tc := range []struct {
		name               string
		cpu, mem           int64
		wantCPU, wantMem   int64
		pcpu, pmem         int64
		wantPCPU, wantPMem int64
	}{
		{"unset keeps compiled-in", 0, 0, 2000, 4096, 0, 0, 500, 256},
		{"set", 1000, 2048, 1000, 2048, 250, 128, 250, 128},
		{"one field", 1000, 0, 1000, 4096, 0, 512, 500, 512},
	} {
		SetDefaultLimits(tc.cpu, tc.mem)
		SetProxyLimits(tc.pcpu, tc.pmem)
		got := EffectiveLimits()
		if got.CPUMillis != tc.wantCPU || got.MemoryMiB != tc.wantMem || got.PidsLimit != DefaultPidsLimit {
			t.Errorf("%s: EffectiveLimits = %+v", tc.name, got)
		}
		if c, m := ProxyLimits(); c != tc.wantPCPU || m != tc.wantPMem {
			t.Errorf("%s: ProxyLimits = %d/%d", tc.name, c, m)
		}
	}
}
