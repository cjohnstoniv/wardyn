// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import "github.com/cjohnstoniv/wardyn/internal/types"

// Fit is a runner's answer for one run of a given size: the one rule placement, the store's locked
// admission, the promotion, the queue's verdict and every usage read share, so none of them can
// disagree about whether a run fits.
type Fit int

const (
	// FitOK: the run fits now.
	FitOK Fit = iota
	// FitUnset: the runner declares no limits, so it takes no run at all.
	FitUnset
	// FitTooBig: the run asks for more than the runner's per-run limit, so it never fits there.
	FitTooBig
	// FitNoRoom: the run is within the per-run limit, but the runner is at its Runs at once or one of
	// its totals would be passed; it fits once runs there end.
	FitNoRoom
)

// Fits answers whether a run reserving need fits on a runner with limits l whose runs already hold
// inUse.
func Fits(l *types.RunnerLimits, inUse types.RunnerTally, need types.RunnerAmounts) Fit {
	switch {
	case l == nil:
		return FitUnset
	case need.CPUMillis > l.PerRun.CPUMillis || need.MemoryMiB > l.PerRun.MemoryMiB || need.DiskMiB > l.PerRun.DiskMiB:
		return FitTooBig
	case inUse.Runs >= l.RunsAtOnce || inUse.CPUMillis+need.CPUMillis > l.Total.CPUMillis ||
		inUse.MemoryMiB+need.MemoryMiB > l.Total.MemoryMiB || inUse.DiskMiB+need.DiskMiB > l.Total.DiskMiB:
		return FitNoRoom
	}
	return FitOK
}

// Available is a runner's total less what its runs hold, never below zero, and nothing at all while it
// is offline or declares no limits: such a runner's capacity is not counted as available.
func Available(l *types.RunnerLimits, inUse types.RunnerTally, online bool) types.RunnerTally {
	if l == nil || !online {
		return types.RunnerTally{}
	}
	return types.RunnerTally{
		Runs:      max(0, l.RunsAtOnce-inUse.Runs),
		CPUMillis: max(0, l.Total.CPUMillis-inUse.CPUMillis),
		MemoryMiB: max(0, l.Total.MemoryMiB-inUse.MemoryMiB),
		DiskMiB:   max(0, l.Total.DiskMiB-inUse.DiskMiB),
	}
}

// Usage is a runner's usage as every read shows it.
func Usage(l *types.RunnerLimits, inUse types.RunnerTally, online, diskCapped bool, reported *types.RunnerAmounts) types.RunnerUsage {
	return types.RunnerUsage{Online: online, InUse: inUse, Available: Available(l, inUse, online), DiskCapped: diskCapped, Reported: reported}
}
