// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// measureHost is what this computer has for runs (hostMeasure), swapped in tests.
var measureHost = hostMeasure

// registerLimits are register's limit flags: Runs at once, and the total and per-run CPU (CPUs), memory
// and disk (GB, 1024 MiB each). A flag left out takes the proposal: half of what this computer reports
// as the total, and per run the total divided by Runs at once (at least 1 CPU and 1 GB).
type registerLimits struct {
	maxRuns             int
	totalCPU, runCPU    float64
	totalMem, totalDisk int64
	runMem, runDisk     int64
}

func (f *registerLimits) register(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.IntVar(&f.maxRuns, "max-runs", 0, fmt.Sprintf("runs this host may hold at once, 1 to %d (defaults to half its CPUs, at least 1)", types.RunnerMaxRunsCeiling))
	fl.Float64Var(&f.totalCPU, "total-cpu", 0, "CPUs all its runs at once may use together (defaults to half of what this computer reports)")
	fl.Int64Var(&f.totalMem, "total-memory", 0, "GB of memory all its runs at once may use together (defaults to half)")
	fl.Int64Var(&f.totalDisk, "total-disk", 0, "GB of disk all its runs at once may use together (defaults to half of Docker's free disk)")
	fl.Float64Var(&f.runCPU, "per-run-cpu", 0, "CPUs one run may use (defaults to the total divided by runs at once)")
	fl.Int64Var(&f.runMem, "per-run-memory", 0, "GB of memory one run may use (defaults to the total divided by runs at once)")
	fl.Int64Var(&f.runDisk, "per-run-disk", 0, "GB of disk one run may use (defaults to the total divided by runs at once)")
}

// defaultRuns is the Runs at once proposed for what this computer reports.
func defaultRuns(measured types.RunnerAmounts) int {
	if measured.CPUMillis > 0 {
		return types.SuggestedRunnerMaxRuns(int(measured.CPUMillis / 1000))
	}
	return types.SuggestedRunnerMaxRuns(runtime.NumCPU())
}

// amountField is one of the six amounts: the flag that sets it, the prompt's label for it, the value the
// flag holds, where it lands in a limit set, and (for a total) what this computer reports.
type amountField struct {
	flag, label string
	cpu         bool
	v           float64
	dst         *int64
	reported    int64
	unit        string
}

func (f *registerLimits) totalFields(dst *types.RunnerAmounts, m types.RunnerAmounts) []amountField {
	return []amountField{
		{"total-cpu", "CPU", true, f.totalCPU, &dst.CPUMillis, m.CPUMillis, "CPUs"},
		{"total-memory", "memory GB", false, float64(f.totalMem), &dst.MemoryMiB, m.MemoryMiB, "GB"},
		{"total-disk", "disk GB", false, float64(f.totalDisk), &dst.DiskMiB, m.DiskMiB, "GB"},
	}
}

func (f *registerLimits) perRunFields(dst *types.RunnerAmounts) []amountField {
	return []amountField{
		{"per-run-cpu", "CPU", true, f.runCPU, &dst.CPUMillis, 0, ""},
		{"per-run-memory", "memory GB", false, float64(f.runMem), &dst.MemoryMiB, 0, ""},
		{"per-run-disk", "disk GB", false, float64(f.runDisk), &dst.DiskMiB, 0, ""},
	}
}

// apply lays the field's value into its limit set; a total past what the computer reports is refused.
func (a amountField) apply() error {
	v, err := scaled(a.flag, a.v, a.cpu)
	if err != nil {
		return err
	}
	if a.reported > 0 && v > a.reported {
		return fmt.Errorf("--%s %s is more than this computer reports (%s %s)", a.flag, strings.TrimSuffix(fmt.Sprint(a.v), ".0"), show(a.reported, a.cpu), a.unit)
	}
	*a.dst = v
	return nil
}

// limits is the limit set to register with: the proposal for what this computer reports, each flag
// given laid over it. A total past what the computer reports is refused, and one it cannot measure must
// be given.
func (f *registerLimits) limits(cmd *cobra.Command, measured types.RunnerAmounts) (types.RunnerLimits, error) {
	set := cmd.Flags().Changed
	runs := defaultRuns(measured)
	if set("max-runs") {
		if f.maxRuns < 1 {
			return types.RunnerLimits{}, errors.New("--max-runs is a whole number, 1 or more")
		}
		runs = f.maxRuns
	}
	l := types.ProposeRunnerLimits(measured, runs)
	for _, a := range f.totalFields(&l.Total, measured) {
		if set(a.flag) {
			if err := a.apply(); err != nil {
				return l, err
			}
		}
	}
	l.PerRun = types.PerRunShare(l.Total, runs)
	for _, a := range f.perRunFields(&l.PerRun) {
		if set(a.flag) {
			if err := a.apply(); err != nil {
				return l, err
			}
		}
	}
	if measured.DiskMiB <= 0 && !set("total-disk") {
		return l, errors.New("this computer's free disk for Docker could not be measured: pass --total-disk (and --per-run-disk if you like) in GB")
	}
	if measured.CPUMillis <= 0 && !set("total-cpu") || measured.MemoryMiB <= 0 && !set("total-memory") {
		//lint:ignore ST1005 the sentence the person registering reads; it opens with the product name Docker
		return l, errors.New("Docker did not report this computer's CPUs and memory: pass --total-cpu and --total-memory")
	}
	return l, l.Validate(types.RunnerMaxRunsCeiling)
}

// scaled is a flag's value in milli-CPU (cpu) or MiB.
func scaled(flag string, v float64, cpu bool) (int64, error) {
	unit := 1024.0
	if cpu {
		unit = 1000
	}
	if v <= 0 || math.IsNaN(v) || v*unit > math.MaxInt32 {
		return 0, fmt.Errorf("--%s is a number more than 0", flag)
	}
	return int64(math.Round(v * unit)), nil
}

// show is milli-CPU or MiB in CPUs or GB, without a trailing ".0".
func show(v int64, cpu bool) string {
	unit := 1024.0
	if cpu {
		unit = 1000
	}
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(v)/unit), ".0")
}

// reports is the line register prints for what this computer measured.
func reports(m types.RunnerAmounts) string {
	part := func(v int64, cpu bool, what string) string {
		if v <= 0 {
			return what + " not measured"
		}
		return show(v, cpu) + " " + what
	}
	return "this computer reports " + part(m.CPUMillis, true, "CPUs") + ", " + part(m.MemoryMiB, false, "GB memory") + ", " + part(m.DiskMiB, false, "GB disk")
}

// limitsLine is the limit set as register prints it.
func limitsLine(l types.RunnerLimits) string {
	return fmt.Sprintf("limits: runs at once %d; in total %s CPUs, %s GB memory, %s GB disk; per run %s CPUs, %s GB memory, %s GB disk",
		l.RunsAtOnce, show(l.Total.CPUMillis, true), show(l.Total.MemoryMiB, false), show(l.Total.DiskMiB, false),
		show(l.PerRun.CPUMillis, true), show(l.PerRun.MemoryMiB, false), show(l.PerRun.DiskMiB, false))
}
