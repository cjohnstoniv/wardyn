// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import "testing"

// The managed preferences are prepare's per-run limits when no flag is set and the organisation has sent none;
// a saved limit and a flag both outrank them.
func TestPrepareLimitsFromManagedPreferences(t *testing.T) {
	m, file := limitsRig(t)
	withManagedPrefs(t, "darwin", map[string]string{"PerRunCPU": "4", "PerRunMemoryGB": "8"})
	cpus, mem, err := prepareLimits(file, 0, 0)
	if err != nil || cpus != 4 || mem != 8<<10 {
		t.Fatalf("from the profile: %d CPUs, %d MiB, %v", cpus, mem, err)
	}
	if cpus, _, err = prepareLimits(file, 2, 0); err != nil || cpus != 2 {
		t.Fatalf("a flag lost to the profile: %d, %v", cpus, err)
	}
	m.saveLimits(6000, 12<<10)
	if cpus, mem, err = prepareLimits(file, 0, 0); err != nil || cpus != 6 || mem != 12<<10 {
		t.Fatalf("the organisation's limits lost to the profile: %d CPUs, %d MiB, %v", cpus, mem, err)
	}
}
