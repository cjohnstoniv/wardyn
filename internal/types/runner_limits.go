// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

// A runner's limits, which the runner's owner declares and placement holds every run on it to. Today
// that is max_runs, the runner's "Runs at once"; a runner's other limits belong here beside it.

const (
	// RunnerMaxRunsCeiling bounds a runner's max_runs (migration 0172's check).
	RunnerMaxRunsCeiling = 64
	// RunnerMaxRunsDefault is the max_runs of a runner that never stated one: registered before 0172,
	// or by a wardyn-runnerd that does not send it.
	RunnerMaxRunsDefault = 2
)

// SuggestedRunnerMaxRuns is what registration offers when no max_runs is given: about half the host's
// CPU cores, at least one, at most the ceiling.
func SuggestedRunnerMaxRuns(cpus int) int { return min(max(1, cpus/2), RunnerMaxRunsCeiling) }

// ValidRunnerMaxRuns reports whether n is a max_runs a runner may hold.
func ValidRunnerMaxRuns(n int) bool { return n >= 1 && n <= RunnerMaxRunsCeiling }
