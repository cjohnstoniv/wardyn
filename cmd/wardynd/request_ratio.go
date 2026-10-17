// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// parseRequestRatio reads WARDYN_SANDBOX_REQUEST_RATIO. Empty is unset (0, requests equal limits);
// anything else must parse and lie in (0, 1], so an explicit 0 is refused rather than read as unset.
func parseRequestRatio(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || !(v > 0 && v <= 1) {
		return 0, fmt.Errorf("WARDYN_SANDBOX_REQUEST_RATIO %q must be a number in (0, 1]", s)
	}
	return v, nil
}

// applyRequestRatio parses WARDYN_SANDBOX_REQUEST_RATIO and sets it on the runner (boot, from run).
func applyRequestRatio(s string) error {
	ratio, err := parseRequestRatio(s)
	if err != nil {
		return err
	}
	return runner.SetRequestRatio(ratio)
}
