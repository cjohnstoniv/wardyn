// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/placement"
)

func TestEngineCapacityIsTheEnginesSizeOrNothing(t *testing.T) {
	if got := engineCapacity(8, 16<<30); got != (placement.Capacity{CPUMillisMax: 8000, MemoryMiBMax: 16384}) {
		t.Fatalf("8 CPUs, 16 GiB = %+v", got)
	}
	for _, c := range []struct {
		cpus int
		mem  int64
	}{{0, 16 << 30}, {8, 0}} {
		if got := engineCapacity(c.cpus, c.mem); got != (placement.Capacity{}) {
			t.Errorf("%d CPUs, %d bytes = %+v, want nothing reported", c.cpus, c.mem, got)
		}
	}
}
