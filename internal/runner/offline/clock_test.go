// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

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
