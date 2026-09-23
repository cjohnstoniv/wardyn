// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package testclock gives test fixtures a date relative to the moment the
// test runs instead of a literal one. A literal future date is a future date
// only until it isn't: the suite has shipped fixtures that were 400 days out
// when written and expired, silently making unrelated code look broken,
// before the next release.
package testclock

import "time"

// FutureRFC3339 returns an RFC3339 timestamp hours from now. Negative hours
// give a timestamp in the past, for a fixture that needs to already be
// expired.
func FutureRFC3339(hours float64) string {
	return time.Now().Add(time.Duration(hours * float64(time.Hour))).UTC().Format(time.RFC3339)
}
