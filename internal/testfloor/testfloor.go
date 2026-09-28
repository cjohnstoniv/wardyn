// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package testfloor lets a test mark itself part of a suite's skip floor:
// tests whose whole purpose is proving a real invariant holds, so a suite
// reporting green while all of them silently skipped would be a lie.
//
// The floor used to be a name regex in scripts/test-report.sh, coupling
// enforcement to the test's NAME — a rename that missed the script defeated
// the floor with no error. Mark ties the declaration to the test BODY
// instead: scripts/test-report.sh finds each `testfloor.Mark(t, "<suite>")`
// line in source and requires that test to pass AND log the Marker string,
// so skipping before Mark turns the report red instead of dropping out.
package testfloor

import "testing"

// Marker is the sentinel prefix scripts/test-report.sh looks for in a
// test's logged output to recognise it as a floor probe.
const Marker = "WARDYN_FLOOR_PROBE"

// Mark declares t as one of suite's floor probes. Call it as the first line
// of a top-level test, written exactly `testfloor.Mark(t, "<suite>")` — not
// from a helper or subtest: test-report.sh reads that exact line from source
// and only inspects top-level pass/skip/fail.
//
// suite must match the test-report.sh <suite> argument meant to enforce this
// probe. Untagged files compile into every suite's binary, so a probe that
// skips on a missing precondition would otherwise trip suites it was never
// meant to gate; the suite string scopes it to the intended run.
func Mark(t *testing.T, suite string) {
	t.Helper()
	t.Log(Marker + ":" + suite)
}
