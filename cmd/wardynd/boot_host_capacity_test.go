// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/hostcapacity"
)

// TestHostCapacityBootDefaultIsOff: a daemon booted with neither WARDYN_HOST_*
// variable set carries the zero Limits, which the guard admits without reading
// the host; set, both reach the guard.
func TestHostCapacityBootDefaultIsOff(t *testing.T) {
	ensureUnset(t, "WARDYN_HOST_MIN_MEM_AVAILABLE_MIB")
	ensureUnset(t, "WARDYN_HOST_MAX_LOAD1")
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })

	resetFlags(t)
	os.Args = []string{"wardynd-test"}
	if got := parseBootFlags().hostCapacity.limits(); got != (hostcapacity.Limits{}) {
		t.Fatalf("default limits = %+v, want the zero value (off)", got)
	}

	t.Setenv("WARDYN_HOST_MIN_MEM_AVAILABLE_MIB", "8192")
	t.Setenv("WARDYN_HOST_MAX_LOAD1", "100")
	resetFlags(t)
	if got, want := parseBootFlags().hostCapacity.limits(), (hostcapacity.Limits{MinAvailableMiB: 8192, MaxLoad1: 100}); got != want {
		t.Fatalf("env limits = %+v, want %+v", got, want)
	}
}
