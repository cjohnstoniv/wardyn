// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// limitFlags are `wardyn runner set`'s limit flags: Runs at once, and the total and per-run CPU (in
// CPUs), memory and disk (in GB, as the console says them; 1 GB is 1024 MiB). Each one left out keeps the
// runner's own value.
type limitFlags struct {
	maxRuns                int
	totalCPU, runCPU       float64
	totalMemory, totalDisk int64
	runMemory, runDisk     int64
	cmd                    *cobra.Command
}

var limitFlagNames = []string{"max-runs", "total-cpu", "total-memory", "total-disk", "per-run-cpu", "per-run-memory", "per-run-disk"}

func (f *limitFlags) register(cmd *cobra.Command) {
	f.cmd = cmd
	fl := cmd.Flags()
	fl.IntVar(&f.maxRuns, "max-runs", 0, "Runs at once: how many runs the runner takes at the same time")
	fl.Float64Var(&f.totalCPU, "total-cpu", 0, "CPUs all its runs at once may use together")
	fl.Int64Var(&f.totalMemory, "total-memory", 0, "GB of memory all its runs at once may use together")
	fl.Int64Var(&f.totalDisk, "total-disk", 0, "GB of disk all its runs at once may use together")
	fl.Float64Var(&f.runCPU, "per-run-cpu", 0, "CPUs one run may use")
	fl.Int64Var(&f.runMemory, "per-run-memory", 0, "GB of memory one run may use")
	fl.Int64Var(&f.runDisk, "per-run-disk", 0, "GB of disk one run may use")
}

// changed reports whether any limit flag was given, and whether all of them were.
func (f *limitFlags) changed() (anyOf, all bool) {
	all = true
	for _, n := range limitFlagNames {
		c := f.cmd.Flags().Changed(n)
		anyOf, all = anyOf || c, all && c
	}
	return anyOf, all
}

// apply lays the given flags over cur (nil: the runner has none yet, so every flag is needed) and checks
// what it can before any request: the server checks the rest.
func (f *limitFlags) apply(cur *sdk.RunnerLimits) (sdk.RunnerLimits, error) {
	set := func(n string) bool { return f.cmd.Flags().Changed(n) }
	if _, all := f.changed(); cur == nil && !all {
		return sdk.RunnerLimits{}, errors.New("this runner has no limits yet: set all of --" + strings.Join(limitFlagNames, ", --"))
	}
	var l sdk.RunnerLimits
	if cur != nil {
		l = *cur
	}
	if set("max-runs") {
		if f.maxRuns < 1 {
			return l, errors.New("--max-runs is a whole number, 1 or more")
		}
		l.RunsAtOnce = f.maxRuns
	}
	cpu := func(n string, v float64, dst *int64) error {
		if set(n) {
			if v <= 0 || math.IsNaN(v) || v > math.MaxInt32/1000 {
				return fmt.Errorf("--%s is a number of CPUs, more than 0", n)
			}
			*dst = int64(math.Round(v * 1000))
		}
		return nil
	}
	gb := func(n string, v int64, dst *int64) error {
		if set(n) {
			if v < 1 || v > math.MaxInt32/1024 {
				return fmt.Errorf("--%s is a whole number of GB, 1 or more", n)
			}
			*dst = v * 1024
		}
		return nil
	}
	if err := errors.Join(cpu("total-cpu", f.totalCPU, &l.Total.CPUMillis), cpu("per-run-cpu", f.runCPU, &l.PerRun.CPUMillis),
		gb("total-memory", f.totalMemory, &l.Total.MemoryMiB), gb("per-run-memory", f.runMemory, &l.PerRun.MemoryMiB),
		gb("total-disk", f.totalDisk, &l.Total.DiskMiB), gb("per-run-disk", f.runDisk, &l.PerRun.DiskMiB)); err != nil {
		return l, err
	}
	for _, r := range []struct {
		what       string
		run, total int64
		unit       int64
	}{{"CPU", l.PerRun.CPUMillis, l.Total.CPUMillis, 1000}, {"memory", l.PerRun.MemoryMiB, l.Total.MemoryMiB, 1024}, {"disk", l.PerRun.DiskMiB, l.Total.DiskMiB, 1024}} {
		if r.run > r.total {
			return l, fmt.Errorf("per-run %s (%s) can't be more than total %s (%s)", r.what, amount(r.run, r.unit), r.what, amount(r.total, r.unit))
		}
	}
	return l, nil
}

// amount is v in units of unit, without a trailing ".0".
func amount(v, unit int64) string {
	return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(v)/float64(unit)), ".0")
}

// said is what an update changed, as one line: "runs at once 4; memory 32 GB in total, 8 GB per run".
func (f *limitFlags) said(l sdk.RunnerLimits) string {
	set := func(n string) bool { return f.cmd.Flags().Changed(n) }
	var parts []string
	if set("max-runs") {
		parts = append(parts, fmt.Sprintf("runs at once %d", l.RunsAtOnce))
	}
	for _, r := range []struct {
		what, unit string
		div        int64
		total, run int64
	}{{"CPU", " CPUs", 1000, l.Total.CPUMillis, l.PerRun.CPUMillis}, {"memory", " GB", 1024, l.Total.MemoryMiB, l.PerRun.MemoryMiB},
		{"disk", " GB", 1024, l.Total.DiskMiB, l.PerRun.DiskMiB}} {
		key := strings.ToLower(r.what)
		var bits []string
		if set("total-" + key) {
			bits = append(bits, amount(r.total, r.div)+r.unit+" in total")
		}
		if set("per-run-" + key) {
			bits = append(bits, amount(r.run, r.div)+r.unit+" per run")
		}
		if len(bits) > 0 {
			parts = append(parts, r.what+" "+strings.Join(bits, ", "))
		}
	}
	return strings.Join(parts, "; ")
}

// runnerSetCmd changes a runner: its limits (your own self-hosted runner, or, an admin, a remote-hosted
// runner), or, an admin or security admin, whether a remote-hosted runner is available to people's
// pools. A self-hosted runner's limits are only ever its owner's to change: an administrator's token is
// refused by the server.
func runnerSetCmd(client clientFn) *cobra.Command {
	var limits limitFlags
	var people string
	var ifUnset bool
	cmd := &cobra.Command{
		Use: "set <runner> [--max-runs N] [--total-cpu N --total-memory GB --total-disk GB] " +
			"[--per-run-cpu N --per-run-memory GB --per-run-disk GB] [--if-unset] [--available-to-people on|off]",
		Short: "Change a runner's limits, or (an admin) whether a remote-hosted runner is available to people's pools",
		Args:  cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			lim, _ := limits.changed()
			avail := cmd.Flags().Changed("available-to-people")
			switch {
			case ifUnset && !lim:
				return errors.New("--if-unset goes with the limit flags")
			case !lim && !avail:
				return errors.New("name something to change: --max-runs, a --total-* or --per-run-* flag, or --available-to-people")
			case lim && avail:
				return errors.New("set the limits and --available-to-people one at a time")
			case avail && people != "on" && people != "off":
				return errors.New("--available-to-people is on or off")
			case cmd.Flags().Changed("max-runs") && limits.maxRuns < 1:
				return errors.New("--max-runs is a whole number, 1 or more")
			}
			// What the flags alone say is checked before any request; the rest once the runner is read.
			if _, err := limits.apply(roomyLimits()); err != nil {
				return err
			}
			c := client()
			ref, err := resolveRunner(cmd, c, args[0], func() ([]sdk.RemoteRunnerView, error) { return c.ListRemoteRunners(cmd.Context()) }, nil)
			if err != nil {
				return err
			}
			if avail {
				return setPeoplePools(cmd, c, ref, args[0], people == "on")
			}
			if ref.self == nil {
				return setRemoteLimits(cmd, c, ref.remote, &limits, ifUnset)
			}
			if ifUnset {
				return fmt.Errorf("--if-unset is for a remote-hosted runner; %s is self-hosted", args[0])
			}
			mine, err := c.GetMyRunner(cmd.Context(), *ref.self)
			if err != nil {
				return err
			}
			l, err := limits.apply(mine.Limits)
			if err != nil {
				return err
			}
			v, err := c.SetMyRunnerLimits(cmd.Context(), *ref.self, l)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "limits for %s updated: %s\n", v.Name, limits.said(l))
			return err
		}}
	limits.register(cmd)
	cmd.Flags().BoolVar(&ifUnset, "if-unset", false, "a remote-hosted runner: set the limits only if it still has none when read, else leave what an administrator declared (what setup uses)")
	cmd.Flags().StringVar(&people, "available-to-people", "", "on: anyone may add this remote-hosted runner to their own user pools; off: nobody (admin or security admin; for listed people use `wardyn runner remote people set`)")
	return cmd
}

// setRemoteLimits declares a remote-hosted runner's limits at the revision just read (admin). With
// ifUnset, limits found on that same read are left alone; a write that races after it keeps the server's
// revision conflict.
func setRemoteLimits(cmd *cobra.Command, c *sdk.Client, id string, limits *limitFlags, ifUnset bool) error {
	rr, err := c.GetRemoteRunner(cmd.Context(), id)
	if err != nil {
		return err
	}
	if ifUnset && rr.Limits != nil {
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s already has limits; left as they are\n", rr.Name)
		return err
	}
	l, err := limits.apply(rr.Limits)
	if err != nil {
		return err
	}
	v, err := c.UpdateRemoteRunner(cmd.Context(), id, sdk.UpdateRemoteRunnerRequest{Revision: rr.Revision, Limits: &l})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "limits for %s updated: %s\n", v.Name, limits.said(l))
	return err
}

// setPeoplePools makes a remote-hosted runner available to everyone's pools, or to nobody's.
func setPeoplePools(cmd *cobra.Command, c *sdk.Client, ref runnerRef, arg string, on bool) error {
	if ref.self != nil {
		return fmt.Errorf("%s is a self-hosted runner: only remote-hosted runners are made available to people's pools", arg)
	}
	var err error
	msg := "%s is now available to people's pools\n"
	if on {
		_, err = c.SetRemoteRunnerPeoplePolicy(cmd.Context(), ref.remote, sdk.RemoteRunnerPeoplePolicyRequest{PeoplePools: sdk.PeoplePoolsEveryone})
	} else {
		msg = "%s is no longer available to people's pools\n"
		err = c.ClearRemoteRunnerPeoplePolicy(cmd.Context(), ref.remote)
	}
	if held, err := heldForApproval(cmd, err); held || err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), msg, arg)
	return err
}

// proposedLimitFlags is the limit set setup proposes for a runner that reports reported, as the flags
// `wardyn runner set` takes: what the server proposes for a remote-hosted runner at boot
// (types.ProposeRunnerLimits, Runs at once half its CPUs up to types.RemoteRunnerMaxRunsCeiling), in
// whole GB. freeDiskGB, when given, is the runner's free disk in place
// of its report. An amount nobody measured is an error, never a guess; why, when the server gave one,
// is the read it could not make.
func proposedLimitFlags(reported *sdk.RunnerAmounts, why string, freeDiskGB int64) (string, error) {
	var m sdk.RunnerAmounts
	if reported != nil {
		m = *reported
	}
	if freeDiskGB > 0 {
		m.DiskMiB = freeDiskGB * 1024
	}
	switch {
	case m.CPUMillis <= 0 || m.MemoryMiB <= 0:
		if why != "" {
			why = " (" + why + ")"
		}
		return "", fmt.Errorf("the runner reports no CPU or memory to propose from%s: set its limits with `wardyn runner set`", why)
	case m.DiskMiB <= 0:
		return "", errors.New("the runner does not report its free disk: pass --free-disk with the GB free for runs on its host")
	}
	l := types.ProposeRunnerLimits(m, min(max(1, int(m.CPUMillis/1000)/2), types.RemoteRunnerMaxRunsCeiling))
	gb := func(mib int64) int64 { return max(1, mib/1024) }
	cpus := func(millis int64) string { return strconv.FormatFloat(float64(millis)/1000, 'f', -1, 64) }
	return fmt.Sprintf("--max-runs %d --total-cpu %s --total-memory %d --total-disk %d --per-run-cpu %s --per-run-memory %d --per-run-disk %d",
		l.RunsAtOnce, cpus(l.Total.CPUMillis), gb(l.Total.MemoryMiB), gb(l.Total.DiskMiB), cpus(l.PerRun.CPUMillis), gb(l.PerRun.MemoryMiB), gb(l.PerRun.DiskMiB)), nil
}

// roomyLimits is a limit set no flag can be refused against for being past it: the flags' own checks
// (a per-run amount past a total both given) run on it before any request.
func roomyLimits() *sdk.RunnerLimits {
	most := sdk.RunnerAmounts{CPUMillis: math.MaxInt32, MemoryMiB: math.MaxInt32, DiskMiB: math.MaxInt32}
	return &sdk.RunnerLimits{RunsAtOnce: 1, Total: most, PerRun: sdk.RunnerAmounts{CPUMillis: 1000, MemoryMiB: 1, DiskMiB: 1024}}
}
