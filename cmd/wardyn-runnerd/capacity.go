// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import "github.com/cjohnstoniv/wardyn/internal/placement"

// engineCapacity is a container engine's CPUs and memory (bytes) as a runner capacity: millicores and
// MiB. An engine that reports either as zero gives zero, which the control plane reads as unknown.
func engineCapacity(cpus int, memBytes int64) placement.Capacity {
	if cpus <= 0 || memBytes <= 0 {
		return placement.Capacity{}
	}
	return placement.Capacity{CPUMillisMax: int64(cpus) * 1000, MemoryMiBMax: memBytes >> 20}
}
