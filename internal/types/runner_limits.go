// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"fmt"
)

// A runner's limits (0.9), both kinds: Runs at once, the total CPU, memory and disk all its runs at once
// may reserve, and what one run may reserve. A self-hosted runner's owner declares them; an
// administrator declares a remote-hosted runner's. A runner with none declared takes no run.

const (
	// RunnerMaxRunsCeiling bounds a self-hosted runner's Runs at once (migration 0172's check).
	RunnerMaxRunsCeiling = 64
	// RemoteRunnerMaxRunsCeiling bounds a remote-hosted runner's Runs at once (migration 0174's check).
	RemoteRunnerMaxRunsCeiling = 1024
	// RunnerCPUMillisMin is the least CPU a runner's total or per-run limit may name: one CPU.
	RunnerCPUMillisMin = 1000
	// RunnerDiskStepMiB: a runner's disk limits are whole GiB, as a run's main drive is sized.
	RunnerDiskStepMiB = 1024
	// RunnerMaxRunsDefault is the max_runs of a runner registered before 0172.
	RunnerMaxRunsDefault = 2
)

// SuggestedRunnerMaxRuns is what registration offers when no Runs at once is given: about half the
// host's CPU cores, at least one, at most the ceiling.
func SuggestedRunnerMaxRuns(cpus int) int { return min(max(1, cpus/2), RunnerMaxRunsCeiling) }

// ValidRunnerMaxRuns reports whether n is a Runs at once a self-hosted runner may hold.
func ValidRunnerMaxRuns(n int) bool { return n >= 1 && n <= RunnerMaxRunsCeiling }

// RunnerAmounts is an amount of CPU (milli-CPU), memory (MiB) and disk (MiB).
type RunnerAmounts struct {
	CPUMillis int64 `json:"cpu_millis"`
	MemoryMiB int64 `json:"memory_mib"`
	DiskMiB   int64 `json:"disk_mib"`
}

// RunnerLimits is a runner's declared limit set: how many runs it takes at once, what all of them
// together may reserve (Total) and what one may reserve (PerRun).
type RunnerLimits struct {
	RunsAtOnce int           `json:"runs_at_once"`
	Total      RunnerAmounts `json:"total"`
	PerRun     RunnerAmounts `json:"per_run"`
}

// Validate refuses a limit set the store's checks would: Runs at once from 1 to maxRuns, each amount
// within the run request contract's bounds (at least one CPU, disk in whole GiB), and no per-run amount
// past its total.
func (l RunnerLimits) Validate(maxRuns int) error {
	if l.RunsAtOnce < 1 || l.RunsAtOnce > maxRuns {
		return fmt.Errorf("runs at once is a whole number from 1 to %d", maxRuns)
	}
	if err := l.Total.validate("total"); err != nil {
		return err
	}
	if err := l.PerRun.validate("per run"); err != nil {
		return err
	}
	if l.PerRun.CPUMillis > l.Total.CPUMillis || l.PerRun.MemoryMiB > l.Total.MemoryMiB || l.PerRun.DiskMiB > l.Total.DiskMiB {
		return errors.New("a per-run limit cannot be more than the runner's total")
	}
	return nil
}

func (a RunnerAmounts) validate(what string) error {
	switch {
	case a.CPUMillis < RunnerCPUMillisMin || a.CPUMillis > RunnerPoolCPUMillisMax:
		return fmt.Errorf("%s CPU is from 1 to %d CPUs", what, RunnerPoolCPUMillisMax/1000)
	case a.MemoryMiB < 1 || a.MemoryMiB > RunnerPoolMemoryMiBMax:
		return fmt.Errorf("%s memory is from 1 MiB to %d GiB", what, RunnerPoolMemoryMiBMax/1024)
	case a.DiskMiB < RunnerDiskStepMiB || a.DiskMiB > RunnerPoolDiskMiBMax || a.DiskMiB%RunnerDiskStepMiB != 0:
		return fmt.Errorf("%s disk is a whole number of GiB from 1 to %d", what, RunnerPoolDiskMiBMax/1024)
	}
	return nil
}

// ProposeRunnerLimits is what setup offers for a computer that measured as measured (owner ruling RP
// Q3): half of each as the total, and per run the total divided by Runs at once, at least one CPU and
// one GiB of memory and disk, never past the total. A measured amount of 0 (not measurable) proposes 0
// there, which Validate refuses, so the person is asked for it.
func ProposeRunnerLimits(measured RunnerAmounts, runsAtOnce int) RunnerLimits {
	runs := int64(max(1, runsAtOnce))
	half := func(v, least, step int64) int64 {
		if v <= 0 {
			return 0
		}
		return max(least, v/2/step*step)
	}
	total := RunnerAmounts{
		CPUMillis: half(measured.CPUMillis, RunnerCPUMillisMin, 1),
		MemoryMiB: half(measured.MemoryMiB, 1, 1),
		DiskMiB:   half(measured.DiskMiB, RunnerDiskStepMiB, RunnerDiskStepMiB),
	}
	return RunnerLimits{RunsAtOnce: int(runs), Total: total, PerRun: PerRunShare(total, int(runs))}
}

// PerRunShare is each run's share of total when runsAtOnce run side by side: total divided by Runs at
// once, at least one CPU and one GiB of memory and disk, never past the total.
func PerRunShare(total RunnerAmounts, runsAtOnce int) RunnerAmounts {
	runs := int64(max(1, runsAtOnce))
	share := func(v, least, step int64) int64 { return min(v, max(least, v/runs/step*step)) }
	return RunnerAmounts{
		CPUMillis: share(total.CPUMillis, RunnerCPUMillisMin, 1),
		MemoryMiB: share(total.MemoryMiB, 1024, 1),
		DiskMiB:   share(total.DiskMiB, RunnerDiskStepMiB, RunnerDiskStepMiB),
	}
}

// RunnerTally is a number of runs and the CPU, memory and disk they reserve.
type RunnerTally struct {
	Runs      int   `json:"runs"`
	CPUMillis int64 `json:"cpu_millis"`
	MemoryMiB int64 `json:"memory_mib"`
	DiskMiB   int64 `json:"disk_mib"`
}

// RunnerUsage is how much of a runner its runs hold. InUse is every run holding a sandbox on it, from
// every pool and person: what placement counts. Available is the limits' total less InUse, and all
// zero while the runner is offline or declares no limits. DiskCapped is false where the runner cannot
// cap a run's disk, so its disk limits are counted, never enforced. Reported is what the runner last
// said it has, never a limit.
type RunnerUsage struct {
	Online     bool           `json:"online"`
	InUse      RunnerTally    `json:"in_use"`
	Available  RunnerTally    `json:"available"`
	DiskCapped bool           `json:"disk_capped"`
	Reported   *RunnerAmounts `json:"reported,omitempty"`
}
