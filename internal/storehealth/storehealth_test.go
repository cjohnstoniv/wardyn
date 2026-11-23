// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package storehealth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *clock { return &clock{t: time.Now()} }

func TestTracker_HealthyForCountsFromTheFirstGoodPing(t *testing.T) {
	c := newClock()
	tr := New(c.now, 25*time.Minute)
	if got := tr.HealthyFor(); got != 0 {
		t.Fatalf("before any ping HealthyFor = %s, want 0", got)
	}
	tr.Observe(nil)
	c.advance(5 * time.Second)
	tr.Observe(nil)
	c.advance(5 * time.Second)
	tr.Observe(nil)
	if got := tr.HealthyFor(); got != 10*time.Second {
		t.Fatalf("HealthyFor = %s, want 10s", got)
	}
}

func TestTracker_OneFailedPingDoesNotReset(t *testing.T) {
	c := newClock()
	tr := New(c.now, 25*time.Minute)
	tr.Observe(nil)
	c.advance(10 * time.Second)
	tr.Observe(errors.New("slow"))
	c.advance(10 * time.Second)
	tr.Observe(nil)
	if got := tr.HealthyFor(); got != 20*time.Second {
		t.Fatalf("HealthyFor = %s, want 20s", got)
	}
}

func TestTracker_ThirtySecondsWithoutAGoodPingResets(t *testing.T) {
	c := newClock()
	tr := New(c.now, 25*time.Minute)
	tr.Observe(nil)
	c.advance(ResetAfter)
	if got := tr.HealthyFor(); got != 0 {
		t.Fatalf("HealthyFor after %s without a good ping = %s, want 0", ResetAfter, got)
	}
	tr.Observe(nil)
	c.advance(7 * time.Second)
	if got := tr.HealthyFor(); got != 7*time.Second {
		t.Fatalf("a good ping must start a new span: HealthyFor = %s, want 7s", got)
	}
}

func TestTracker_RecoveringUntilTheGracePasses(t *testing.T) {
	c := newClock()
	tr := New(c.now, time.Minute)
	if !tr.Recovering() {
		t.Fatal("a tracker with no good ping must be recovering")
	}
	tr.Observe(nil)
	for i := 0; i < 11; i++ {
		c.advance(5 * time.Second)
		tr.Observe(nil)
	}
	if !tr.Recovering() {
		t.Fatalf("recovering must hold at %s, below the 1m grace", tr.HealthyFor())
	}
	c.advance(5 * time.Second)
	tr.Observe(nil)
	if tr.Recovering() {
		t.Fatalf("recovering must end at %s, the grace", tr.HealthyFor())
	}
}

func TestTracker_ZeroGraceIsNeverRecovering(t *testing.T) {
	tr := New(newClock().now, 0)
	if tr.Recovering() {
		t.Fatal("a zero grace must never be recovering")
	}
}

func TestTracker_NilIsNeverRecovering(t *testing.T) {
	var tr *Tracker
	if tr.Recovering() {
		t.Fatal("a nil tracker must never be recovering")
	}
	if tr.Grace() != 0 {
		t.Fatal("a nil tracker has a zero grace")
	}
}

func TestTracker_RunPingsUntilCancelled(t *testing.T) {
	tr := New(time.Now, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	ping := func(context.Context) error {
		calls++
		if calls == 2 {
			cancel()
		}
		return nil
	}
	done := make(chan struct{})
	go func() { tr.Run(ctx, ping); close(done) }()
	select {
	case <-done:
	case <-time.After(PingEvery + 5*time.Second):
		t.Fatal("Run did not return after its context ended")
	}
	if calls != 2 {
		t.Fatalf("ping ran %d times, want exactly 2 (none after the cancel)", calls)
	}
}

func TestTracker_StoresWallClockReadingsOnly(t *testing.T) {
	tr := New(time.Now, time.Minute)
	tr.Observe(nil)
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if strings.Contains(tr.healthySince.String(), "m=") || strings.Contains(tr.lastOK.String(), "m=") {
		t.Fatalf("stored times carry a monotonic reading: %s, %s", tr.healthySince, tr.lastOK)
	}
}

func TestTracker_AClockSteppedBackIsNeverNegative(t *testing.T) {
	c := newClock()
	tr := New(c.now, time.Minute)
	tr.Observe(nil)
	c.advance(10 * time.Second)
	tr.Observe(nil)
	c.advance(-time.Minute)
	if got := tr.HealthyFor(); got != 0 {
		t.Fatalf("HealthyFor after a step back = %s, want 0", got)
	}
	if !tr.Recovering() {
		t.Fatal("a clock stepped back must keep the tracker recovering")
	}
	tr.Observe(nil)
	if got := tr.HealthyFor(); got != 0 {
		t.Fatalf("a good ping before the old span restarts it: HealthyFor = %s, want 0", got)
	}
}

func TestTracker_FailedPingsNeverCountAsGood(t *testing.T) {
	c := newClock()
	tr := New(c.now, 25*time.Minute)
	tr.Observe(nil)
	for i := 0; i < 6; i++ {
		c.advance(PingEvery)
		tr.Observe(errors.New("down"))
	}
	if got := tr.HealthyFor(); got != 0 {
		t.Fatalf("HealthyFor after %s of failed pings = %s, want 0", ResetAfter, got)
	}
	tr.Observe(nil)
	c.advance(PingEvery)
	tr.Observe(nil)
	if got := tr.HealthyFor(); got != PingEvery {
		t.Fatalf("the span must restart at the first good ping after the failures: HealthyFor = %s, want %s", got, PingEvery)
	}
}

func TestTracker_AGoodPingAfterAStepBackStartsANewSpan(t *testing.T) {
	c := newClock()
	tr := New(c.now, time.Minute)
	tr.Observe(nil)
	c.advance(-time.Minute)
	tr.Observe(nil)
	c.advance(7 * time.Second)
	tr.Observe(nil)
	if got := tr.HealthyFor(); got != 7*time.Second {
		t.Fatalf("HealthyFor = %s, want 7s: the span counts from the first good ping after the step back", got)
	}
}
