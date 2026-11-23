// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package live

import (
	"strconv"
	"testing"
)

// TestGraderRejected: only exit 1 is a grader rejecting a bad fixture. A run that never started (-1), a
// grader that passed it (0), a crashed one (2 to 124) and docker's own errors (125 and up) are not.
func TestGraderRejected(t *testing.T) {
	for _, tc := range []struct {
		code int
		want bool
	}{
		{-1, false}, {0, false}, {1, true}, {2, false}, {124, false}, {125, false}, {126, false}, {127, false}, {137, false},
	} {
		t.Run(strconv.Itoa(tc.code), func(t *testing.T) {
			if got := graderRejected(tc.code); got != tc.want {
				t.Errorf("graderRejected(%d) = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}
