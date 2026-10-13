// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// isTerminal is whether in is a person at a keyboard: nothing is asked of a pipe or a file.
func isTerminal(in io.Reader) bool {
	f, ok := in.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// ask offers each limit not given as a flag, Enter taking the suggestion shown. An answer goes through
// the checks its flag has and is then held as that flag, so limits sees it as given. Totals come first,
// then Runs at once, then per run, whose suggestions follow the totals and Runs at once as answered.
func (f *registerLimits) ask(cmd *cobra.Command, in io.Reader, out io.Writer, measured types.RunnerAmounts) error {
	set := cmd.Flags().Changed
	p := &limitPrompt{cmd: cmd, in: bufio.NewReader(in), out: out}
	runs := defaultRuns(measured)
	if set("max-runs") && f.maxRuns >= 1 {
		runs = f.maxRuns
	}
	unset := func(names ...string) bool {
		for _, n := range names {
			if !set(n) {
				return true
			}
		}
		return false
	}
	needTotals, needPer := unset("total-cpu", "total-memory", "total-disk"), unset("per-run-cpu", "per-run-memory", "per-run-disk")
	if !needTotals && !needPer && set("max-runs") {
		return nil
	}
	total := types.ProposeRunnerLimits(measured, runs).Total
	fmt.Fprintln(out, "limits for this runner (press Enter to accept the suggestion, or type a lower value)")
	if needTotals {
		fmt.Fprintln(out, "  total across all its runs: half of what this computer reports")
	}
	if err := p.fill(f.totalFields(&total, measured)); err != nil {
		return err
	}
	if !set("max-runs") {
		text, err := p.line("runs at once", strconv.Itoa(runs), "  ")
		if err != nil {
			return err
		}
		if text != "" {
			if runs, err = typedRuns(text); err != nil {
				return err
			}
			if err := cmd.Flags().Set("max-runs", text); err != nil {
				return err
			}
		}
	}
	per := types.PerRunShare(total, runs)
	if needPer {
		fmt.Fprintln(out, "  per run, the most one run may use: total / runs at once, at least 1 CPU and 1 GB")
	}
	return p.fill(f.perRunFields(&per))
}

// typedRuns is a typed Runs at once, refused with the sentences --max-runs and the limit set use.
func typedRuns(text string) (int, error) {
	n, err := strconv.Atoi(text)
	if err != nil || n < 1 {
		return 0, errors.New("--max-runs is a whole number, 1 or more")
	}
	if n > types.RunnerMaxRunsCeiling {
		return 0, fmt.Errorf("runs at once is a whole number from 1 to %d", types.RunnerMaxRunsCeiling)
	}
	return n, nil
}

type limitPrompt struct {
	cmd *cobra.Command
	in  *bufio.Reader
	out io.Writer
}

// fill asks for each of fields whose flag was not given; one that was given is only laid into its set,
// so what follows is suggested from it.
func (p *limitPrompt) fill(fields []amountField) error {
	for _, a := range fields {
		if p.cmd.Flags().Changed(a.flag) {
			if err := a.apply(); err != nil {
				return err
			}
			continue
		}
		suggested := ""
		if *a.dst > 0 {
			suggested = show(*a.dst, a.cpu)
		}
		text, err := p.line(a.label, suggested, "    ")
		if err != nil {
			return err
		}
		if text == "" {
			continue
		}
		if a.v, err = typedAmount(a, text); err != nil {
			return err
		}
		if err := a.apply(); err != nil {
			return err
		}
		if err := p.cmd.Flags().Set(a.flag, text); err != nil {
			return err
		}
	}
	return nil
}

// typedAmount is a typed CPU count, or whole number of GB, as its flag takes it.
func typedAmount(a amountField, text string) (float64, error) {
	v, err := strconv.ParseFloat(text, 64)
	switch {
	case a.cpu && (err != nil || math.IsNaN(v)):
		return 0, fmt.Errorf("--%s is a number more than 0", a.flag)
	case !a.cpu && (err != nil || v != math.Trunc(v)):
		return 0, fmt.Errorf("--%s is a whole number of GB, 1 or more", a.flag)
	}
	return v, nil
}

// line prints one prompt and reads the answer; an input that ends before any answer is an error rather
// than an Enter, so a closed terminal does not register a runner on the suggestions.
func (p *limitPrompt) line(label, suggested, indent string) (string, error) {
	if suggested != "" {
		label += " [" + suggested + "]"
	}
	fmt.Fprint(p.out, indent+label+": ")
	text, err := p.in.ReadString('\n')
	if err != nil && (!errors.Is(err, io.EOF) || text == "") {
		return "", errors.New("the limits were not answered: input ended")
	}
	return strings.TrimSpace(text), nil
}
