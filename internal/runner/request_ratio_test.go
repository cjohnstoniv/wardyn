// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "testing"

func TestEffectiveRequestsAndRatioBounds(t *testing.T) {
	t.Cleanup(func() { _ = SetRequestRatio(0); SetDefaultLimits(0, 0) })
	if cpu, mem := EffectiveRequests(Resources{CPUMillis: 1000, MemoryMiB: 2048}); cpu != 1000 || mem != 2048 {
		t.Errorf("no ratio: requests = %d/%d, want limits 1000/2048", cpu, mem)
	}
	for _, bad := range []float64{1.5, -0.1} {
		if err := SetRequestRatio(bad); err == nil {
			t.Errorf("SetRequestRatio(%v) accepted", bad)
		}
	}
	if err := SetRequestRatio(0.25); err != nil {
		t.Fatal(err)
	}
	if cpu, mem := EffectiveRequests(Resources{CPUMillis: 1000, MemoryMiB: 2048}); cpu != 250 || mem != 512 {
		t.Errorf("ratio 0.25: requests = %d/%d, want 250/512", cpu, mem)
	}
	SetDefaultLimits(2000, 4096)
	if cpu, mem := EffectiveRequests(Resources{}); cpu != 500 || mem != 1024 {
		t.Errorf("ratio over the default size: requests = %d/%d, want 500/1024", cpu, mem)
	}
	if cpu, _ := EffectiveRequests(Resources{CPUMillis: 1000, CPURequestMillis: 4000}); cpu != 1000 {
		t.Errorf("explicit request above limit = %d, want clamp to 1000", cpu)
	}
}
