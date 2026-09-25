// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package testutil holds small, dependency-free helpers shared by Go tests
// across packages. It is imported only from *_test.go files, never from
// production code.
package testutil

import "time"

// FutureRFC3339 returns an RFC3339 (UTC) timestamp `hours` from the moment it
// is called, mirroring the TypeScript aheadByHours
// (ui/src/app/lib/test-clock.ts). Use it instead of a literal calendar date in
// a fixture: a literal future date is a future date only until the wall clock
// catches up to it, and an "is this expired" assertion then starts failing for
// reasons that have nothing to do with the code under test. Negative hours
// give a timestamp already in the past, for a fixture that needs to already
// be expired.
func FutureRFC3339(hours int) string {
	return time.Now().UTC().Add(time.Duration(hours) * time.Hour).Format(time.RFC3339)
}
