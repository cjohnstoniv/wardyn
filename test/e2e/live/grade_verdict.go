// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package live

// graderRejected reports whether a grader's exit code is the graded rejection the negative control
// expects of a known-bad fixture. Every grader under test/e2e/tasks exits 0 to pass and 1 to fail, and
// nothing else, so only 1 is a rejection. -1 (gradeExit could not start docker, or the process was
// killed by a signal, which includes the 60-second timeout), 0 (the grader passed a bad fixture),
// 2 to 124 (a crashed or misused grader) and 125 and above (docker's own errors) are not.
func graderRejected(code int) bool { return code == 1 }
