// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package testfloor lets a test declare itself part of a suite's skip
// floor — the small set of tests whose whole purpose is to be falsifiable
// proof a real invariant holds, so that suite reporting green while every
// one of them silently skipped is a lie.
//
// The floor used to be a name regex in scripts/test-report.sh
// (e.g. "^TestPG_ProbeF11_"), which coupled the test's NAME to the gate: a
// rename that did not also touch the script defeated the floor with no
// error anywhere. Mark ties the declaration to the test BODY instead, so
// renaming the function is safe and scripts/test-report.sh finds the same
// probes by scanning `go test -json` output for the Marker string.
package testfloor

import "testing"

// Marker is the sentinel prefix scripts/test-report.sh looks for in a
// test's logged output to recognise it as a floor probe.
const Marker = "WARDYN_FLOOR_PROBE"

// Mark declares t as one of suite's floor probes. Call it as the first line
// of a top-level test func (not a subtest: test-report.sh's skip floor only
// inspects top-level pass/skip/fail, matching how it already read go test
// -json before this package existed).
//
// suite must match the scripts/test-report.sh <suite> argument that is
// meant to enforce this probe (e.g. "unit", "pg", "docker", "k8s"). Files
// with no build tag — internal/api's, notably — compile into every suite's
// binary, so a probe that skips on a missing precondition (WARDYN_TEST_PG
// unset, say) would otherwise trip suites it was never meant to gate; the
// suite tag scopes the marker to the one run that is supposed to prove it.
func Mark(t *testing.T, suite string) {
	t.Helper()
	t.Log(Marker + ":" + suite)
}
