// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package live

// gradeVerdict classifies a grader's exit code. Only 0 (pass) and 1 (graded
// failure) are verdicts; anything else means the grader never produced one:
// -1 (docker could not start it, or a signal/timeout), 2..124 (a crashed or
// misused grader), 125+ (docker's own errors).
type gradeVerdict int

const (
	verdictPass gradeVerdict = iota
	verdictRejected
	verdictNoVerdict
)

func classifyGrade(code int) gradeVerdict {
	switch code {
	case 0:
		return verdictPass
	case 1:
		return verdictRejected
	default:
		return verdictNoVerdict
	}
}
