// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import "time"

// One clock for the revocation cutoff and everything compared against it.
//
// revoked_at is stamped by POSTGRES; the values compared against it (an API
// token's created_at, a session cookie's iat) are stamped by WARDYND. If
// wardynd's clock runs ahead of the database's, a credential minted BEFORE a
// revoke can carry a timestamp AFTER the cutoff and survive the revoke.
//
// Fix: stamp the app value as an ELAPSED TIME (measured at request admission,
// not INSERT) rendered against the database's own now():
//
//	created_at = now() - <the request's age>
//
// The same arithmetic, reversed, compares an app-stamped iat against a
// database-stamped cutoff.

// AppClockAgeSQL renders an app-measured age, in MICROSECONDS, as an interval
// to subtract from the database's own clock. Microseconds as a bigint (not a
// float or pgx interval) matches timestamptz's own resolution exactly.
func AppClockAgeSQL(placeholder string) string {
	return "now() - (" + placeholder + "::bigint * interval '1 microsecond')"
}

// maxAppClockAge bounds AppClockAgeMicros: unbounded, a zero time (e.g. a
// session cookie minted before the `iat` claim existed) reports an age of
// ~2025 years, overflowing the interval. A century is far past every real
// credential lifetime, and clamping can only make a value read as OLDER —
// the fail-closed direction.
const maxAppClockAge = 100 * 365 * 24 * time.Hour

// AppClockAgeMicros returns how long ago the app-clock instant t was, as of
// the app-clock instant now, in microseconds, clamped to [0, maxAppClockAge].
//
// Both arguments must come from the SAME clock: a duration between two
// readings of one clock carries no skew, so it is safe to hand to a
// different clock. Passing a value read back from the DATABASE as t would
// reintroduce the skew.
//
// Clamped at zero so a value stamped in the future reads as "now" rather
// than a negative interval. The ZERO TIME is NOT special-cased here: it
// means different things to different callers, so each states its own answer.
func AppClockAgeMicros(t, now time.Time) int64 {
	age := now.Sub(t)
	if age < 0 {
		age = 0
	}
	if age > maxAppClockAge {
		age = maxAppClockAge
	}
	return age.Microseconds()
}
