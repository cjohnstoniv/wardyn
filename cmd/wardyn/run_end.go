// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// runEndFlags are `wardyn run`'s flags for when a run ends. They are passed to the
// server as ends_after_sec or no_end; the server owns every limit and says in the
// 201 when an end was cut to one.
type runEndFlags struct {
	endsIn string
	noEnd  bool
}

func (f *runEndFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.endsIn, "ends-in", "", "end the run this long after it is created: a Go duration such as 90m or 36h, where d means 24 hours (2d, 1d12h). An end past what your pool, admin or deployment allows is cut to it and the warning says so. Unset takes the default end")
	cmd.Flags().BoolVar(&f.noEnd, "no-end", false, "run with no end, where the pool, the deployment and your admin allow it (otherwise refused). Not with --ends-in")
}

// apply copies the flags into body, so a bad value names its flag before any request.
func (f runEndFlags) apply(body *sdk.CreateRunRequest) error {
	switch {
	case f.noEnd && f.endsIn != "":
		return errors.New("--ends-in and --no-end are mutually exclusive: give a duration or no end")
	case f.noEnd:
		body.NoEnd = true
	case f.endsIn != "":
		sec, err := parseEndsIn(f.endsIn)
		if err != nil {
			return err
		}
		body.EndsAfterSec = &sec
	}
	return nil
}

// endsInDays matches every run of digits and dots before a d; only a plain <n> or <n>.<m> is a number of days.
var endsInDays = regexp.MustCompile(`[\d.]+d`)

var endsInDaysNumber = regexp.MustCompile(`^\d+(?:\.\d+)?$`)

// parseEndsIn reads a Go duration in which d is 24 hours, as whole seconds.
func parseEndsIn(s string) (int, error) {
	var bad error
	expanded := endsInDays.ReplaceAllStringFunc(s, func(m string) string {
		if !endsInDaysNumber.MatchString(m[:len(m)-1]) {
			return m // ".5d" and "1..5d" stay as written, so time.ParseDuration refuses them
		}
		days, err := strconv.ParseFloat(m[:len(m)-1], 64)
		if err != nil {
			bad = err
		}
		return strconv.FormatFloat(days*24, 'f', -1, 64) + "h"
	})
	d, err := time.ParseDuration(expanded)
	if err != nil || bad != nil {
		return 0, fmt.Errorf("--ends-in %q is not a duration such as 90m, 36h or 2d", s)
	}
	if d < time.Second || d/time.Second > math.MaxInt32 {
		return 0, fmt.Errorf("--ends-in %q must be from 1s to %d s", s, math.MaxInt32)
	}
	return int(d / time.Second), nil
}

// endLine is the create output's line for the end the run got.
func endLine(run sdk.AgentRun) string {
	if run.EndsAt == nil {
		return "  ends: never (no end)"
	}
	return fmt.Sprintf("  ends: %s", run.EndsAt.UTC().Format(time.RFC3339))
}
