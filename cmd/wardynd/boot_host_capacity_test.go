// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"strings"
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

// TestPreflightRateBoot: the default is 20 a minute, an env value reaches the
// flag, and a negative one refuses to boot naming the variable.
func TestPreflightRateBoot(t *testing.T) {
	ensureUnset(t, "WARDYN_PREFLIGHT_RATE_PER_MIN")
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"wardynd-test"}

	resetFlags(t)
	if got := *parseBootFlags().preflightRatePerMin; got != 20 {
		t.Fatalf("default = %d, want 20", got)
	}
	t.Setenv("WARDYN_PREFLIGHT_RATE_PER_MIN", "0")
	resetFlags(t)
	if got := *parseBootFlags().preflightRatePerMin; got != 0 {
		t.Fatalf("env 0 = %d, want 0 (off)", got)
	}

	neg := -1
	err := validateBootPosture(&bootFlags{preflightRatePerMin: &neg}, tlsPosture{})
	if err == nil || !strings.Contains(err.Error(), "WARDYN_PREFLIGHT_RATE_PER_MIN") {
		t.Fatalf("negative rate error = %v, want one naming WARDYN_PREFLIGHT_RATE_PER_MIN", err)
	}
}

func TestPolicyPreviewRateBoot(t *testing.T) {
	ensureUnset(t, "WARDYN_POLICY_PREVIEW_RATE_PER_MIN")
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"wardynd-test"}

	resetFlags(t)
	if got := *parseBootFlags().policyPreviewRatePerMin; got != 60 {
		t.Fatalf("default = %d, want 60", got)
	}
	t.Setenv("WARDYN_POLICY_PREVIEW_RATE_PER_MIN", "0")
	resetFlags(t)
	if got := *parseBootFlags().policyPreviewRatePerMin; got != 0 {
		t.Fatalf("env 0 = %d, want 0 (off)", got)
	}

	neg := -1
	err := validateBootPosture(&bootFlags{preflightRatePerMin: new(int), policyPreviewRatePerMin: &neg}, tlsPosture{})
	if err == nil || !strings.Contains(err.Error(), "WARDYN_POLICY_PREVIEW_RATE_PER_MIN") {
		t.Fatalf("negative rate error = %v, want one naming WARDYN_POLICY_PREVIEW_RATE_PER_MIN", err)
	}
}
