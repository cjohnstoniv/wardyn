// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// printApplySteps writes what a partial apply made: a count per outcome, then
// one line per step, so an operator sees which writes landed before a failure.
func printApplySteps(w io.Writer, steps []sdk.ApplyStep) {
	var applied, pending, failed int
	for _, s := range steps {
		switch s.Outcome {
		case "applied":
			applied++
		case "pending":
			pending++
		case "failed":
			failed++
		}
	}
	fmt.Fprintf(w, "applied: %d, pending: %d, failed: %d\n", applied, pending, failed)
	for _, s := range steps {
		target := s.Name
		if target == "" {
			target = fmt.Sprintf("%s %s", s.SubjectType, s.Subject)
		}
		fmt.Fprintf(w, "  %s %s %s %s", s.Outcome, s.Kind, s.Op, target)
		if s.Error != "" {
			fmt.Fprintf(w, ": %s", s.Error)
		}
		fmt.Fprintln(w)
	}
}
