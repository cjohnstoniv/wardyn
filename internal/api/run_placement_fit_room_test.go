// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run that names no size is sized to a runner with room, not to the largest runner while it is at
// its Runs at once: otherwise the run is cut to the busy runner's disk and waits for it, although a
// smaller runner is idle.
func TestRunnerSourcePrefersARunnerWithRoom(t *testing.T) {
	big := &types.RunnerLimits{RunsAtOnce: 1, PerRun: types.RunnerAmounts{CPUMillis: 8000, MemoryMiB: 16384, DiskMiB: 102400}}
	small := &types.RunnerLimits{RunsAtOnce: 2, PerRun: types.RunnerAmounts{CPUMillis: 2000, MemoryMiB: 4096, DiskMiB: 20480}}
	smallCap := runnerpool.RunnerCapacity{CPUMillis: 2000, MemoryMiB: 4096, DiskMiB: 20480}
	bigCap := runnerpool.RunnerCapacity{CPUMillis: 8000, MemoryMiB: 16384, DiskMiB: 102400}

	fit := []poolTarget{{id: "big", limits: big, full: true}, {id: "small", limits: small}}
	if got := runnerSource(fit, "", types.RunnerAmounts{}); len(got) != 1 || got[0] != smallCap {
		t.Fatalf("source with the large runner full = %+v, want only the idle runner %+v", got, smallCap)
	}
	// Every runner full: the run waits, sized as before by the largest.
	fit[1].full = true
	if got := runnerSource(fit, "", types.RunnerAmounts{}); len(got) != 2 || got[0] != bigCap {
		t.Fatalf("source with every runner full = %+v, want both runners", got)
	}
	// A pinned runner answers for itself even when it is full.
	if got := runnerSource([]poolTarget{{id: "big", limits: big, full: true}, {id: "small", limits: small}}, "big", types.RunnerAmounts{}); len(got) != 1 || got[0] != bigCap {
		t.Fatalf("pinned full runner source = %+v, want %+v", got, bigCap)
	}
}
