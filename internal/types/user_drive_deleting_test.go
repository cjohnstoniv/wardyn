// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"
	"time"
)

// Deleting is true when the mark is set or the issue count is odd, for every pair the two columns can hold.
func TestUserDriveDeleting(t *testing.T) {
	at := time.Now()
	for _, tc := range []struct {
		name   string
		mark   *time.Time
		issues int
		want   bool
	}{
		{"admissible: no mark, even count", nil, 0, false},
		{"admissible after a clear: no mark, count moved by two", nil, 2, false},
		{"claimed: mark, even count", &at, 0, true},
		{"issued: mark, odd count", &at, 1, true},
		{"cleared mark over an odd count", nil, 1, true},
		{"taken over an odd count", &at, 3, true},
	} {
		if got := (UserDrive{DeleteStartedAt: tc.mark, DeleteIssues: tc.issues}).Deleting(); got != tc.want {
			t.Errorf("%s: Deleting() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
