// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

// SetRunRetentionStatementTimeout sets the run retention batch's statement timeout for one test.
func SetRunRetentionStatementTimeout(t *testing.T, v string) {
	t.Helper()
	prev := runRetentionStatementTimeout
	runRetentionStatementTimeout = v
	t.Cleanup(func() { runRetentionStatementTimeout = prev })
}
