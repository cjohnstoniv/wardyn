// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"os"
	"strings"
	"testing"
)

func limitsRig(t *testing.T) (*macOSVMs, string) {
	t.Helper()
	file := macOSVMLimitsFile(t.TempDir())
	return &macOSVMs{limitsFile: file, kick: make(chan struct{}, 1)}, file
}

// A bare prepare with no saved limits has none to use and says the organisation sent none.
func TestPrepareLimitsRefuseWithNoFile(t *testing.T) {
	_, file := limitsRig(t)
	_, _, err := prepareLimits(file, 0, 0)
	if err == nil || !strings.Contains(err.Error(), "pass --per-run-cpu and --per-run-memory") ||
		!strings.HasSuffix(err.Error(), " (the organisation has sent no per-run limits)") {
		t.Fatalf("a bare prepare with no saved limits: %v", err)
	}
	if _, _, err := prepareLimits(file, 4, 0); err == nil {
		t.Fatal("one flag and no saved limits was accepted")
	}
}

// The limits the organisation sent are what prepare uses when no flag says otherwise, and a flag that is
// set wins over them.
func TestPrepareLimitsFromTheSavedFile(t *testing.T) {
	m, file := limitsRig(t)
	m.setPerRunLimits(6000, 12288)
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("the saved limits: %v %v", fi, err)
	}
	cpus, mem, err := prepareLimits(file, 0, 0)
	if err != nil || cpus != 6 || mem != 12288 {
		t.Fatalf("no flags: %d CPUs, %d MiB, %v", cpus, mem, err)
	}
	if cpus, mem, err = prepareLimits(file, 4, 0); err != nil || cpus != 4 || mem != 12288 {
		t.Fatalf("a CPU flag: %d CPUs, %d MiB, %v", cpus, mem, err)
	}
	if cpus, mem, err = prepareLimits(file, 0, 8); err != nil || cpus != 6 || mem != 8192 {
		t.Fatalf("a memory flag: %d CPUs, %d MiB, %v", cpus, mem, err)
	}
	if cpus, mem, err = prepareLimits(file, 5, 9); err != nil || cpus != 5 || mem != 9216 {
		t.Fatalf("both flags: %d CPUs, %d MiB, %v", cpus, mem, err)
	}
	m.setPerRunLimits(4000, 1536)
	if _, mem, err = prepareLimits(file, 0, 0); err != nil || mem != 1536 {
		t.Fatalf("a limit that is not whole GB: %d MiB, %v", mem, err)
	}
	m.setPerRunLimits(4000, 512) // under 1 GB: nothing prepare can use, though the runner has connected
	if _, _, err = prepareLimits(file, 0, 0); err == nil || strings.Contains(err.Error(), "sent no per-run limits") {
		t.Fatalf("saved limits too small to use: %v", err)
	}
	m.setPerRunLimits(8000, 16384)
	if cpus, mem, err = prepareLimits(file, 0, 0); err != nil || cpus != 8 || mem != 16384 {
		t.Fatalf("limits sent again replace the saved ones: %d CPUs, %d MiB, %v", cpus, mem, err)
	}
}

// An organisation that sends no CPU or no memory limit saves nothing.
func TestSetPerRunLimitsSavesOnlyBoth(t *testing.T) {
	m, file := limitsRig(t)
	for _, l := range [][2]int64{{0, 4096}, {4000, 0}, {0, 0}} {
		m.setPerRunLimits(l[0], l[1])
		if _, err := os.Stat(file); !os.IsNotExist(err) {
			t.Fatalf("limits %v wrote a file (%v)", l, err)
		}
	}
	m.limitsFile = ""
	m.setPerRunLimits(4000, 8192) // no file configured: nothing to write, nothing to fail
}

// An organisation that clears the limits drops the saved ones, so prepare no longer sizes from stale
// numbers and says the organisation has sent none.
func TestSetPerRunLimitsClearedDropsTheFile(t *testing.T) {
	m, file := limitsRig(t)
	m.setPerRunLimits(6000, 12288)
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("the limits were not saved: %v", err)
	}
	m.setPerRunLimits(0, 0)
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("0/0 left the saved limits (%v)", err)
	}
	m.setPerRunLimits(0, 0) // already gone: not an error
	_, _, err := prepareLimits(file, 0, 0)
	if err == nil || !strings.HasSuffix(err.Error(), " (the organisation has sent no per-run limits)") {
		t.Fatalf("a bare prepare after the limits were cleared: %v", err)
	}
	m.setPerRunLimits(6000, 12288)
	m.setPerRunLimits(4000, 0) // a half-set pair is cleared too
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("4000/0 left the saved limits (%v)", err)
	}
}

// A write that fails (the directory is gone) never fails setPerRunLimits.
func TestSetPerRunLimitsSurvivesAFailedSave(t *testing.T) {
	m := &macOSVMs{limitsFile: macOSVMLimitsFile(t.TempDir() + "/missing"), kick: make(chan struct{}, 1)}
	m.setPerRunLimits(4000, 8192)
	m.setPerRunLimits(4000, 8192)
	if m.perRunMemory != 8192<<20 {
		t.Fatalf("the limits were not applied: %d", m.perRunMemory)
	}
}
