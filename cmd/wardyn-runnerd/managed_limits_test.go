// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A profile's limit keys stand for the register limit flags the person left out, and a flag they gave wins.
func TestRegisterLimitsFromManagedPreferences(t *testing.T) {
	withManagedPrefs(t, "darwin", map[string]string{"MaxRuns": "3", "TotalCPU": "6", "TotalMemoryGB": "12", "PerRunCPU": "2", "PerRunMemoryGB": "4"})
	run := func(args ...string) types.RunnerLimits {
		t.Helper()
		var lim registerLimits
		cmd := &cobra.Command{}
		lim.register(cmd)
		if err := cmd.ParseFlags(append([]string{"--total-disk=60"}, args...)); err != nil {
			t.Fatal(err)
		}
		if err := cmd.PreRunE(cmd, nil); err != nil {
			t.Fatal(err)
		}
		l, err := lim.limits(cmd, types.RunnerAmounts{CPUMillis: 16000, MemoryMiB: 64 << 10, DiskMiB: 200 << 10})
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	l := run()
	if l.RunsAtOnce != 3 || l.Total.CPUMillis != 6000 || l.Total.MemoryMiB != 12<<10 || l.PerRun.CPUMillis != 2000 || l.PerRun.MemoryMiB != 4<<10 {
		t.Fatalf("limits from the profile = %+v", l)
	}
	if l := run("--max-runs=2", "--per-run-cpu=1"); l.RunsAtOnce != 2 || l.PerRun.CPUMillis != 1000 || l.Total.CPUMillis != 6000 {
		t.Fatalf("a flag lost to the profile: %+v", l)
	}
}

// A profile value the flag would refuse is refused by name, and off a Mac the keys are not read.
func TestRegisterLimitsManagedPreferenceRefusedAndIgnoredOffMac(t *testing.T) {
	var lim registerLimits
	cmd := &cobra.Command{}
	lim.register(cmd)
	withManagedPrefs(t, "darwin", map[string]string{"MaxRuns": "many"})
	if err := cmd.PreRunE(cmd, nil); err == nil || !strings.Contains(err.Error(), "MaxRuns managed preference") {
		t.Fatalf("a bad MaxRuns = %v", err)
	}
	withManagedPrefs(t, "linux", map[string]string{"MaxRuns": "many"})
	if err := cmd.PreRunE(cmd, nil); err != nil || cmd.Flags().Changed("max-runs") {
		t.Fatalf("a non-Mac read the profile: %v", err)
	}
}
