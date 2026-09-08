// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import "time"

// ONE CLOCK FOR THE REVOCATION CUTOFF AND EVERYTHING COMPARED AGAINST IT.
//
// oidc_session_revocations.revoked_at is stamped by POSTGRES (`VALUES ($1,
// now())`). The two things compared against it were stamped by WARDYND: an API
// token's created_at, bound from the Go clock at request admission, and an SSO
// session cookie's `iat`. Two clocks, one inequality — and the direction that
// fails is the security one. With wardynd's clock ahead of the database's by d,
// a credential minted BEFORE a revoke carries a timestamp AFTER the cutoff, so
// it survives the revoke: the admin's "revoke every session for this human"
// silently does not.
//
// The fix is not to stamp the app value with now() — that would re-open F143,
// where a caller who holds a mint request body open across POST /sessions/revoke
// gets a created_at AFTER the cutoff, which is why the API stamps at request
// ADMISSION rather than at INSERT. What both need is the app's measurement of
// an ELAPSED TIME, which no skew rides in on, rendered against the database's
// own now():
//
//	created_at = now() - <the request's age>
//
// That keeps admission-time semantics (F143) and puts the value on the database
// clock (F289). The same arithmetic, used the other way round, is how a
// cookie's app-stamped `iat` is compared against a database-stamped cutoff.

// AppClockAgeSQL renders an app-measured age, in MICROSECONDS, as an interval to
// subtract from the database's own clock. The parameter placeholder is supplied
// by the caller because it differs per statement.
//
// Microseconds, as a bigint, rather than a float or a pgx interval: it is
// timestamptz's own resolution, so the conversion is exact in both directions
// and there is no rounding to reason about.
func AppClockAgeSQL(placeholder string) string {
	return "now() - (" + placeholder + "::bigint * interval '1 microsecond')"
}

// maxAppClockAge bounds what AppClockAgeMicros will report. It exists for the
// one caller that can legitimately hand in a zero time (a session cookie minted
// before the `iat` claim existed): unbounded, that is an age of ~2025 years,
// which multiplies into an interval overflow rather than the very old timestamp
// the caller wanted. A century is far past every real credential lifetime, and
// clamping can only make a value read as OLDER — the fail-closed direction for
// every comparison this is used in.
const maxAppClockAge = 100 * 365 * 24 * time.Hour

// AppClockAgeMicros returns how long ago the app-clock instant t was, as of the
// app-clock instant now, in microseconds — clamped to [0, maxAppClockAge].
//
// BOTH ARGUMENTS MUST COME FROM THE SAME CLOCK. That is the whole point: a
// difference of two readings of one clock is a duration, and a duration carries
// no skew, so it can be handed to a different clock without carrying an offset
// across. Passing a value that was read back from the DATABASE as t would put
// the skew straight back in, in the unsafe direction when the app is behind.
//
// CLAMPED AT ZERO, so a value stamped in the future (the app clock stepped
// backwards between the stamp and this call) becomes "now" rather than a
// negative interval that would push a row's timestamp forward of the database's
// clock — which is the exact shape of the defect this function exists to close.
//
// The ZERO TIME is deliberately NOT special-cased here: it means different
// things to different callers (an unstamped row, versus a pre-D16 cookie that
// must read as revoked), and a helper that guessed which would be wrong for one
// of them. Each caller states its own answer.
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
