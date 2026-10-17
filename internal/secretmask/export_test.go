// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask

import "testing"

// CountScanned counts, for the rest of t, the entries containsSlice is asked
// to search, and returns the running total.
func CountScanned(t testing.TB) *int {
	n := new(int)
	scanned = n
	t.Cleanup(func() { scanned = nil })
	return n
}
