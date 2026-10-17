// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import "github.com/cjohnstoniv/wardyn/internal/hostcapacity"

// hostCapacityFlags are the host-capacity admission limits (docs/ENV.md); both
// default to 0, which leaves the guard off.
type hostCapacityFlags struct {
	minMemAvailableMiB, maxLoad1, maxConcurrentRuns *int
}

func registerHostCapacityFlags() hostCapacityFlags {
	return hostCapacityFlags{
		minMemAvailableMiB: flagIntEnv("host-min-mem-available-mib", "WARDYN_HOST_MIN_MEM_AVAILABLE_MIB", 0, "refuse new runs (HTTP 503) while the host's MemAvailable is below this many MiB (default 0, off)"),
		maxLoad1:           flagIntEnv("host-max-load1", "WARDYN_HOST_MAX_LOAD1", 0, "refuse new runs (HTTP 503) while the host's 1-minute load average is above this (default 0, off)"),
		maxConcurrentRuns:  flagIntEnv("max-concurrent-runs", "WARDYN_MAX_CONCURRENT_RUNS", 0, "refuse new runs (HTTP 422) while this many non-terminal runs exist across the whole deployment (default 0, unlimited)"),
	}
}

func (h hostCapacityFlags) limits() hostcapacity.Limits {
	return hostcapacity.Limits{MinAvailableMiB: *h.minMemAvailableMiB, MaxLoad1: *h.maxLoad1}
}
