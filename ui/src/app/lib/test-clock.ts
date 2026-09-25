/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A fixture date relative to the moment the test runs instead of a literal
// one. A literal future date is a future date only until it isn't: it starts
// failing an "is this expired" assertion the day the wall clock catches up to
// it, for reasons that have nothing to do with the code under test.

/** An ISO-8601 timestamp `hours` from now. Negative hours give a timestamp
 * already in the past, for a fixture that needs to already be expired. */
export function aheadByHours(hours: number): string {
  return new Date(Date.now() + hours * 60 * 60 * 1000).toISOString();
}
