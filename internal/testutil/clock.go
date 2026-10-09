// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package testutil holds small helpers shared by Go tests
// across packages. It is imported only from *_test.go files, never from
// production code.
package testutil

import "time"

// FutureRFC3339 returns an RFC3339 (UTC) timestamp `hours` from now, mirroring
// the TypeScript aheadByHours (ui/src/app/lib/test-clock.ts). Use it instead
// of a literal calendar date in a fixture, which goes stale once the wall
// clock catches up to it. Negative hours give an already-expired timestamp.
//
// A fixture meant to read as "live" (not merely unexpired) needs to clear
// both modelAccessExpiringWindow (24h) and the AWS SSO refresh skew, or
// grading logic will call it "expiring" — 24*30 is what this codebase uses
// for that case.
func FutureRFC3339(hours int) string {
	return time.Now().UTC().Add(time.Duration(hours) * time.Hour).Format(time.RFC3339)
}
