// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package live

import "testing"

// A negative control must count only the graded failure (exit 1) as a rejection.
// -1 and 2..124 are a grader that never ran or crashed, not a verdict.
func TestClassifyGrade(t *testing.T) {
	cases := map[int]gradeVerdict{
		0:   verdictPass,
		1:   verdictRejected,
		-1:  verdictNoVerdict,
		2:   verdictNoVerdict,
		124: verdictNoVerdict,
		125: verdictNoVerdict,
		137: verdictNoVerdict,
	}
	for code, want := range cases {
		if got := classifyGrade(code); got != want {
			t.Errorf("classifyGrade(%d) = %d, want %d", code, got, want)
		}
	}
}
