// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package srvclock

import (
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

func TestClockUsesOrgTimeAndMonotonicElapsed(t *testing.T) {
	server := time.Now().UTC()
	local := server.Add(48 * time.Hour)
	elapsed := time.Duration(0)
	c := NewClock(func(time.Time) time.Duration { return elapsed })
	if _, err := c.TokenRemaining(server); err == nil {
		t.Fatal("lease accepted before HELLO")
	}
	if err := c.Hello(runnerwire.Hello{ServerTime: server}, local); err != nil {
		t.Fatal(err)
	}
	for _, jump := range []time.Duration{0, 24 * time.Hour, -72 * time.Hour} {
		local = local.Add(jump)
		elapsed += time.Minute
		got, err := c.TokenRemaining(server)
		if err != nil || got != identity.RunTokenLapseAfter-elapsed {
			t.Fatalf("wall jump %v: duration=%v error=%v", jump, got, err)
		}
	}
	elapsed = identity.RunTokenLapseAfter
	if got, err := c.TokenRemaining(server); err != nil || got != 0 {
		t.Fatalf("lapse=%v error=%v", got, err)
	}
}

func TestClockBoundsForgedFutureLeaseAndRefusesMissingTime(t *testing.T) {
	c := NewClock(func(time.Time) time.Duration { return 0 })
	now := time.Now()
	if err := c.Hello(runnerwire.Hello{}, now); err == nil {
		t.Fatal("missing org clock accepted")
	}
	if err := c.Hello(runnerwire.Hello{ServerTime: now}, now); err != nil {
		t.Fatal(err)
	}
	if got, err := c.TokenRemaining(now.Add(365 * 24 * time.Hour)); err != nil || got != identity.RunTokenLapseAfter {
		t.Fatalf("future lease duration=%v err=%v", got, err)
	}
	if _, err := c.TokenRemaining(time.Time{}); err == nil {
		t.Fatal("zero renewal accepted")
	}
}

// TestAllowanceRemaining: the allowance runs from the renewal stamp on the monotonic clock, never exceeds its span
// however far ahead the stamp is, and is the token's own leg when given the token's span.
func TestAllowanceRemaining(t *testing.T) {
	server := time.Now().UTC()
	elapsed := time.Duration(0)
	c := NewClock(func(time.Time) time.Duration { return elapsed })
	const span = 20 * time.Hour
	if _, err := c.AllowanceRemaining(server, span); err == nil {
		t.Fatal("allowance computed before HELLO")
	}
	if err := c.Hello(runnerwire.Hello{ServerTime: server}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AllowanceRemaining(time.Time{}, span); err == nil {
		t.Fatal("zero renewal accepted")
	}
	if got, err := c.AllowanceRemaining(server, span); err != nil || got != span {
		t.Fatalf("fresh stamp: remaining=%v err=%v, want %v", got, err, span)
	}
	elapsed = 3 * time.Hour
	if got, err := c.AllowanceRemaining(server, span); err != nil || got != span-elapsed {
		t.Fatalf("after 3h: remaining=%v err=%v, want %v", got, err, span-elapsed)
	}
	// A stamp an hour old has used an hour of the span; one far in the future cannot grant more than the span.
	if got, err := c.AllowanceRemaining(server.Add(-time.Hour), span); err != nil || got != span-time.Hour-elapsed {
		t.Fatalf("old stamp: remaining=%v err=%v, want %v", got, err, span-time.Hour-elapsed)
	}
	if got, err := c.AllowanceRemaining(server.Add(365*24*time.Hour), span); err != nil || got != span {
		t.Fatalf("forged future stamp: remaining=%v err=%v, want the span %v", got, err, span)
	}
	elapsed = span
	if got, err := c.AllowanceRemaining(server, span); err != nil || got != 0 {
		t.Fatalf("spent: remaining=%v err=%v, want 0", got, err)
	}
	elapsed = span + time.Minute
	if got, err := c.AllowanceRemaining(server, span); err != nil || got > 0 {
		t.Fatalf("past the span: remaining=%v err=%v, want none left", got, err)
	}
	elapsed = 10 * time.Minute
	want, err := c.TokenRemaining(server)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c.AllowanceRemaining(server, identity.RunTokenLapseAfter); err != nil || got != want {
		t.Fatalf("token span: remaining=%v err=%v, want TokenRemaining's %v", got, err, want)
	}
}
