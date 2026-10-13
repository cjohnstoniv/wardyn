// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/cjohnstoniv/wardyn/internal/types"
	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// poolLimitFlags are a pool's own limits as flags: they only narrow what its runners allow, and one left
// out is set by its runners. --limits-file is the whole wire block instead, never combined with them.
type poolLimitFlags struct {
	runTypes, barriers []string
	maxRuns            int
	amounts            map[string]*string
	limitsFile         string
	cmd                *cobra.Command
}

// poolAmountFlags are the per-run-type flags: DEFAULT:MAX, in CPUs, GB, Go durations.
var poolAmountFlags = []string{
	"background-cpu", "background-memory", "background-disk", "background-lifetime",
	"interactive-cpu", "interactive-memory", "interactive-disk", "interactive-lifetime", "interactive-idle",
}

func (f *poolLimitFlags) register(cmd *cobra.Command, verb string) {
	f.cmd = cmd
	fl := cmd.Flags()
	fl.StringArrayVar(&f.runTypes, "run-type", nil, "background or interactive: a run type the pool takes (repeatable)")
	fl.StringArrayVar(&f.barriers, "barrier", nil, "fence, wall or vault (CC1, CC2, CC3): a barrier the pool allows (repeatable); none listed allows every barrier its runners offer")
	fl.IntVar(&f.maxRuns, "max-runs", 0, "runs at once across the pool")
	f.amounts = map[string]*string{}
	for _, n := range poolAmountFlags {
		f.amounts[n] = new(string)
		help := "DEFAULT:MAX per run"
		switch {
		case strings.HasSuffix(n, "-cpu"):
			help += ", in CPUs"
		case strings.HasSuffix(n, "-memory"), strings.HasSuffix(n, "-disk"):
			help += ", in GB"
		case n == "background-lifetime":
			help += " as Go durations, or none (no end)"
		case n == "interactive-idle":
			help += " as Go durations, or never"
		default:
			help += " as Go durations"
		}
		fl.StringVar(f.amounts[n], n, "", help)
	}
	fl.StringVar(&f.limitsFile, "limits-file", "", "the pool's "+verb+" limits as the wire JSON block (\"-\" for stdin); not combined with the limit flags")
}

// given reports whether any limit flag (not --limits-file) was given.
func (f *poolLimitFlags) given() bool {
	for _, n := range append([]string{"run-type", "barrier", "max-runs"}, poolAmountFlags...) {
		if f.cmd.Flags().Changed(n) {
			return true
		}
	}
	return false
}

// build is the limits to send: the file's, or cur (nil: none yet) with the flags laid over it, or nil
// when nothing was given. Every check the flags allow is made before any request.
func (f *poolLimitFlags) build(cur *sdk.RunnerPoolLimits) (*sdk.RunnerPoolLimits, error) {
	if f.limitsFile != "" {
		if f.given() {
			return nil, errors.New("--limits-file replaces the limit flags; use one or the other")
		}
		return readPoolLimits(f.limitsFile)
	}
	if !f.given() {
		return nil, nil
	}
	var l sdk.RunnerPoolLimits
	if cur != nil {
		l = *cur
		l.Barriers = slices.Clone(cur.Barriers)
	}
	if err := f.applyRunTypes(&l); err != nil {
		return nil, err
	}
	if f.cmd.Flags().Changed("barrier") {
		l.Barriers = nil
		for _, b := range f.barriers {
			c, err := barrierOf(b)
			if err != nil {
				return nil, err
			}
			l.Barriers = append(l.Barriers, c)
		}
	}
	if f.cmd.Flags().Changed("max-runs") {
		if f.maxRuns < 1 {
			return nil, errors.New("--max-runs is a whole number, 1 or more")
		}
		n := f.maxRuns
		l.MaxConcurrentRuns = &n
	}
	if err := f.applyAmounts(&l); err != nil {
		return nil, err
	}
	return &l, l.Validate()
}

// applyRunTypes keeps the run types --run-type names (each with its limits so far) or, without it, the
// ones the pool takes and any its flags name; a new pool limited by nothing else takes both.
func (f *poolLimitFlags) applyRunTypes(l *sdk.RunnerPoolLimits) error {
	want := map[string]bool{}
	if f.cmd.Flags().Changed("run-type") {
		for _, t := range f.runTypes {
			if t != string(types.RunnerPoolRunBackground) && t != string(types.RunnerPoolRunInteractive) {
				return fmt.Errorf("--run-type is background or interactive, not %q", t)
			}
			want[t] = true
		}
	} else {
		want["background"], want["interactive"] = l.Background != nil, l.Interactive != nil
		for _, n := range poolAmountFlags {
			if f.cmd.Flags().Changed(n) {
				want[strings.SplitN(n, "-", 2)[0]] = true
			}
		}
		if !want["background"] && !want["interactive"] {
			want["background"], want["interactive"] = true, true
		}
	}
	for _, n := range poolAmountFlags {
		if t := strings.SplitN(n, "-", 2)[0]; f.cmd.Flags().Changed(n) && !want[t] {
			return fmt.Errorf("--%s is for a run type the pool takes: add --run-type %s", n, t)
		}
	}
	switch {
	case !want["background"]:
		l.Background = nil
	case l.Background == nil:
		l.Background = &sdk.RunnerPoolBackgroundLimits{}
	}
	switch {
	case !want["interactive"]:
		l.Interactive = nil
	case l.Interactive == nil:
		l.Interactive = &sdk.RunnerPoolInteractiveLimits{}
	}
	return nil
}

func (f *poolLimitFlags) applyAmounts(l *sdk.RunnerPoolLimits) error {
	set := func(n string) (string, bool) { return *f.amounts[n], f.cmd.Flags().Changed(n) }
	amount := func(n string, unit float64, dst **types.RunnerPoolAmount) error {
		v, ok := set(n)
		if !ok {
			return nil
		}
		def, maxV, err := splitPair(n, v, func(s string) (int, error) {
			x, err := strconv.ParseFloat(s, 64)
			if err != nil || x <= 0 || math.IsNaN(x) || x*unit > math.MaxInt32 {
				return 0, fmt.Errorf("--%s: %q is not a number more than 0", n, s)
			}
			return int(math.Round(x * unit)), nil
		})
		if err != nil {
			return err
		}
		*dst = &types.RunnerPoolAmount{Default: def, Cap: maxV}
		return nil
	}
	duration := func(n, open string, dst **types.RunnerPoolDuration) error {
		v, ok := set(n)
		if !ok {
			return nil
		}
		if v == open && open != "" {
			*dst = &types.RunnerPoolDuration{Unlimited: true}
			return nil
		}
		def, maxV, err := splitPair(n, v, func(s string) (int, error) {
			d, err := time.ParseDuration(s)
			if err != nil || d < time.Second || d.Seconds() > math.MaxInt32 {
				return 0, fmt.Errorf("--%s: %q is not a Go duration of a second or more", n, s)
			}
			return int(d.Seconds()), nil
		})
		if err != nil {
			return err
		}
		*dst = &types.RunnerPoolDuration{DefaultSec: def, MaxSec: maxV}
		return nil
	}
	var errs []error
	if b := l.Background; b != nil {
		errs = append(errs, amount("background-cpu", 1000, &b.CPUMillis), amount("background-memory", 1024, &b.MemoryMiB),
			amount("background-disk", 1024, &b.DiskMiB), duration("background-lifetime", "none", &b.Lifetime))
	}
	if i := l.Interactive; i != nil {
		errs = append(errs, amount("interactive-cpu", 1000, &i.CPUMillis), amount("interactive-memory", 1024, &i.MemoryMiB),
			amount("interactive-disk", 1024, &i.DiskMiB), duration("interactive-lifetime", "", &i.Lifetime), duration("interactive-idle", "never", &i.Idle))
	}
	return errors.Join(errs...)
}

// splitPair reads DEFAULT:MAX with parse, and refuses a default past the maximum.
func splitPair(name, v string, parse func(string) (int, error)) (int, int, error) {
	a, b, ok := strings.Cut(v, ":")
	if !ok {
		return 0, 0, fmt.Errorf("--%s is DEFAULT:MAX", name)
	}
	def, err := parse(a)
	if err != nil {
		return 0, 0, err
	}
	maxV, err := parse(b)
	if err != nil {
		return 0, 0, err
	}
	if def > maxV {
		return 0, 0, fmt.Errorf("--%s: the default (%s) can't be more than the maximum (%s)", name, a, b)
	}
	return def, maxV, nil
}

// barrierOf reads a barrier as the console names it or as its class.
func barrierOf(s string) (types.ConfinementClass, error) {
	switch strings.ToLower(s) {
	case "fence", "cc1":
		return types.CC1, nil
	case "wall", "cc2":
		return types.CC2, nil
	case "vault", "cc3":
		return types.CC3, nil
	}
	return "", fmt.Errorf("--barrier is fence, wall or vault (CC1, CC2, CC3), not %q", s)
}
