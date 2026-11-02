// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestNoRoomReasonNamesTheLimitFitsStoppedAt(t *testing.T) {
	const gib = 1024
	limits := &types.RunnerLimits{
		RunsAtOnce: 2,
		Total:      types.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 16 * gib, DiskMiB: 100 * gib},
		PerRun:     types.RunnerAmounts{CPUMillis: 4000, MemoryMiB: 8 * gib, DiskMiB: 50 * gib},
	}
	need := types.RunnerAmounts{CPUMillis: 1500, MemoryMiB: 8 * gib, DiskMiB: 50 * gib}
	for _, tc := range []struct {
		name  string
		l     *types.RunnerLimits
		inUse types.RunnerTally
		need  types.RunnerAmounts
		fit   Fit
		want  string
	}{
		{"no limits", nil, types.RunnerTally{}, need, FitUnset, ""},
		{"past the per-run limit", limits, types.RunnerTally{}, types.RunnerAmounts{MemoryMiB: 9 * gib}, FitTooBig, ""},
		{"fits", limits, types.RunnerTally{Runs: 1, CPUMillis: 1000, MemoryMiB: 8 * gib, DiskMiB: 50 * gib}, need, FitOK, ""},
		{"runs at once", limits, types.RunnerTally{Runs: 2}, need, FitNoRoom, "Runs at once: 2 running of 2"},
		{"cpu total", limits, types.RunnerTally{Runs: 1, CPUMillis: 3000}, need, FitNoRoom, "CPU total: 3 CPUs held of 4 CPUs, this run needs 1.5 CPUs"},
		{"one cpu reads singular", limits, types.RunnerTally{Runs: 1, CPUMillis: 3500}, types.RunnerAmounts{CPUMillis: 1000}, FitNoRoom, "CPU total: 3.5 CPUs held of 4 CPUs, this run needs 1 CPU"},
		{"memory total", limits, types.RunnerTally{Runs: 1, MemoryMiB: 16 * gib}, need, FitNoRoom, "memory total: 16 GB held of 16 GB, this run needs 8 GB"},
		{"a size that isn't whole GB", limits, types.RunnerTally{Runs: 1, MemoryMiB: 15 * gib}, types.RunnerAmounts{MemoryMiB: 1536}, FitNoRoom, "memory total: 15 GB held of 16 GB, this run needs 1.5 GB"},
		{"disk total", limits, types.RunnerTally{Runs: 1, DiskMiB: 60 * gib}, need, FitNoRoom, "disk total: 60 GB held of 100 GB, this run needs 50 GB"},
		// Fits asks in this order, so every limit passed at once names the first.
		{"all passed", limits, types.RunnerTally{Runs: 2, CPUMillis: 4000, MemoryMiB: 16 * gib, DiskMiB: 100 * gib}, need, FitNoRoom, "Runs at once: 2 running of 2"},
		{"cpu before memory", limits, types.RunnerTally{Runs: 1, CPUMillis: 4000, MemoryMiB: 16 * gib, DiskMiB: 100 * gib}, need, FitNoRoom, "CPU total: 4 CPUs held of 4 CPUs, this run needs 1.5 CPUs"},
		{"memory before disk", limits, types.RunnerTally{Runs: 1, MemoryMiB: 16 * gib, DiskMiB: 100 * gib}, need, FitNoRoom, "memory total: 16 GB held of 16 GB, this run needs 8 GB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Fits(tc.l, tc.inUse, tc.need); got != tc.fit {
				t.Fatalf("Fits = %v, want %v", got, tc.fit)
			}
			if got := NoRoomReason(tc.l, tc.inUse, tc.need); got != tc.want {
				t.Fatalf("NoRoomReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// Over every combination of two levels on each limit, the reason is present exactly when Fits answers
// FitNoRoom, so the sentence can never name a limit that isn't the one holding the run back.
func TestNoRoomReasonAgreesWithFitsEverywhere(t *testing.T) {
	limits := &types.RunnerLimits{RunsAtOnce: 1, Total: types.RunnerAmounts{CPUMillis: 2, MemoryMiB: 2, DiskMiB: 2}, PerRun: types.RunnerAmounts{CPUMillis: 2, MemoryMiB: 2, DiskMiB: 2}}
	noRoom := 0
	for _, runs := range []int{0, 1} {
		for _, cpu := range []int64{0, 2} {
			for _, mem := range []int64{0, 2} {
				for _, disk := range []int64{0, 2} {
					for _, need := range []int64{1, 3} {
						inUse := types.RunnerTally{Runs: runs, CPUMillis: cpu, MemoryMiB: mem, DiskMiB: disk}
						amounts := types.RunnerAmounts{CPUMillis: need, MemoryMiB: need, DiskMiB: need}
						fit := Fits(limits, inUse, amounts)
						reason := NoRoomReason(limits, inUse, amounts)
						if (fit == FitNoRoom) != (reason != "") {
							t.Errorf("inUse %+v need %d: Fits = %v, reason = %q", inUse, need, fit, reason)
						}
						if fit == FitNoRoom {
							noRoom++
						}
					}
				}
			}
		}
	}
	if noRoom == 0 {
		t.Fatal("no combination had no room, so the agreement was not exercised")
	}
}

func TestWaitLimitSentences(t *testing.T) {
	limit := "memory total: 16 GB held of 16 GB, this run needs 8 GB"
	if got, want := WaitRunnerAtLimitMsg("mac-1", limit), "mac-1 is at a limit: memory total: 16 GB held of 16 GB, this run needs 8 GB. This run starts when it has room."; got != want {
		t.Errorf("one runner: %q, want %q", got, want)
	}
	if got, want := WaitRunnersAtLimitMsg("Laptops", "mac-1", limit), "Every runner in Laptops is at a limit. For example, mac-1: memory total: 16 GB held of 16 GB, this run needs 8 GB. This run starts when one has room."; got != want {
		t.Errorf("several runners: %q, want %q", got, want)
	}
}
