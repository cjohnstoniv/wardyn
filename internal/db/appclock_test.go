// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// THE ARITHMETIC THAT PUTS TWO CLOCKS ON ONE, driven with no database.
//
// The end-to-end proof of this finding is PG-shaped — set wardynd's clock ahead
// of Postgres's, mint a token, revoke the principal, and assert the token stops
// authenticating — and lives in internal/store's PG lane. What is decided in Go
// is the arithmetic, and it has exactly two properties that matter, both of
// which fail silently and in the unsafe direction:
//
//   - a NEGATIVE age (the app clock stepped backwards, or is simply ahead of the
//     value it is measuring) must clamp to zero, or the row is written FORWARD of
//     the database's clock — which is the defect itself, re-created by the fix;
//   - the age must be the difference of two readings of ONE clock, never a
//     database instant minus an app instant, which would carry the skew back in.

import (
	"testing"
	"time"
)

func TestAppClockAgeMicrosMeasuresElapsedTime(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		at   time.Time
		want int64
	}{
		{"an instant 250ms ago", now.Add(-250 * time.Millisecond), 250_000},
		{"an instant 90s ago", now.Add(-90 * time.Second), 90_000_000},
		{"this instant", now, 0},
		{"microsecond resolution is exact", now.Add(-1 * time.Microsecond), 1},
	} {
		if got := AppClockAgeMicros(tc.at, now); got != tc.want {
			t.Errorf("%s: AppClockAgeMicros = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestAppClockAgeMicrosClampsAFutureInstantToZero(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for _, ahead := range []time.Duration{time.Nanosecond, time.Second, time.Hour, 400 * 24 * time.Hour} {
		if got := AppClockAgeMicros(now.Add(ahead), now); got != 0 {
			t.Errorf("AppClockAgeMicros(now+%s) = %d, want 0. A negative age renders as now() PLUS that interval, so "+
				"the row is written FORWARD of the database's clock — a credential born after a cutoff that has not "+
				"been written yet, which is the defect this arithmetic exists to close", ahead, got)
		}
	}
}

func TestAppClockAgeMicrosClampsAnAbsurdAge(t *testing.T) {
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	// The reachable case: a session cookie minted before the `iat` claim
	// existed carries the zero time.
	got := AppClockAgeMicros(time.Time{}, now)
	if got != maxAppClockAge.Microseconds() {
		t.Errorf("AppClockAgeMicros(zero time) = %d, want the %s clamp. Unclamped this is ~2025 years, which "+
			"overflows the interval multiplication rather than producing the very old timestamp the caller wanted",
			got, maxAppClockAge)
	}
	// Clamping may only ever make a value read as OLDER — the fail-closed
	// direction for every comparison this feeds.
	if got < (10 * 365 * 24 * time.Hour).Microseconds() {
		t.Errorf("the clamp is %d microseconds, which is inside a credential's plausible lifetime; it must stay far "+
			"past every real one so clamping cannot make something read as NEWER than it is", got)
	}
}

// TestAppClockAgeSQLNamesItsPlaceholderAndSubtracts pins the expression's shape,
// because both call sites concatenate it into their own statement and a change
// to the units or the sign is a change to what gets stored and compared.
func TestAppClockAgeSQLNamesItsPlaceholderAndSubtracts(t *testing.T) {
	got := AppClockAgeSQL("$9")
	for _, want := range []string{"now() -", "$9::bigint", "interval '1 microsecond'"} {
		if !contains(got, want) {
			t.Errorf("AppClockAgeSQL(\"$9\") = %q, missing %q. It must SUBTRACT a MICROSECOND age from the "+
				"database's own now(): adding it would write the row forward of the cutoff, and any other unit "+
				"silently rescales every timestamp it produces", got, want)
		}
	}
	if AppClockAgeSQL("$4") == got {
		t.Error("AppClockAgeSQL ignores its placeholder argument; the two call sites bind different parameter numbers")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
