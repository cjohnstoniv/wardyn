// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import "time"

// One clock for the revocation cutoff and everything compared against it. revoked_at is stamped
// by POSTGRES; values compared against it (an API token's created_at, a session cookie's iat) are
// stamped by WARDYND — if wardynd's clock runs ahead, a credential minted BEFORE a revoke could
// carry a timestamp AFTER the cutoff and survive it.
//
// Fix: stamp the app value as an ELAPSED TIME (measured at request admission, not INSERT)
// translated against a received database clock anchor:
//
//	created_at = anchor.DatabaseAt - <age sampled after receipt>
//
// The same arithmetic, reversed, compares an app-stamped iat against a database-stamped cutoff.

// maxAppClockAge bounds AppClockAgeMicros: unbounded, a zero time (e.g. a session cookie minted
// before the `iat` claim existed) reports an age of ~2025 years, overflowing the interval. A
// century is far past any real credential lifetime, and clamping can only make a value read as
// OLDER — the fail-closed direction.
const maxAppClockAge = 100 * 365 * 24 * time.Hour

// AppClockAgeMicros returns how long ago the app-clock instant t was, as of the app-clock instant
// now, in microseconds, clamped to [0, maxAppClockAge]. Both arguments must come from the SAME
// clock: a duration between two readings of one clock carries no skew, so it's safe to hand to a
// different clock — passing a value read back from the DATABASE as t would reintroduce the skew.
//
// Clamped at zero so a value stamped in the future reads as "now" rather than a negative
// interval. The ZERO TIME is NOT special-cased here: it means different things to different
// callers, so each states its own answer.
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
