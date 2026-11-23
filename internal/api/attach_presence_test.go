// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// A writer's presence stamp waits for the keystroke only where a pause may have landed since its last input;
// every other input stamps off the keystroke path, one stamp in flight per holder.

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPresenceStamper_InLineOnlyWhereAPauseMayHaveLanded(t *testing.T) {
	var calls int
	var owed bool
	stamp := func() bool { calls++; return owed }
	var pending []func()
	async := func(fn func()) { pending = append(pending, fn) }
	p := &presenceStamper{}
	t0 := time.Now()

	p.input(t0, stamp, async)
	if calls != 1 || len(pending) != 0 {
		t.Fatalf("first input: stamps=%d queued=%d, want one stamp in line", calls, len(pending))
	}
	p.input(t0.Add(time.Second), stamp, async)
	if calls != 1 || len(pending) != 1 {
		t.Fatalf("input 1s later: stamps=%d queued=%d, want one stamp queued off the keystroke path", calls, len(pending))
	}
	p.input(t0.Add(2*time.Second), stamp, async)
	if calls != 1 || len(pending) != 1 {
		t.Fatalf("input with a stamp in flight: stamps=%d queued=%d, want it skipped", calls, len(pending))
	}
	pending[0]()
	pending = nil
	if calls != 2 {
		t.Fatalf("the queued stamp ran %d stamps in all, want 2", calls)
	}
	p.input(t0.Add(3*time.Second), stamp, async)
	if calls != 2 || len(pending) != 1 {
		t.Fatalf("input after the stamp returned: stamps=%d queued=%d, want another queued", calls, len(pending))
	}
	pending[0]()
	pending = nil

	// Past presenceInLineAfter without input, the keystroke waits for its stamp.
	last := t0.Add(3*time.Second + presenceInLineAfter)
	owed = true
	p.input(last, stamp, async)
	if calls != 4 || len(pending) != 0 {
		t.Fatalf("input after a long quiet: stamps=%d queued=%d, want one more in line", calls, len(pending))
	}
	// That stamp left work owed: the next input is in line too, and once nothing is owed the one after is not.
	owed = false
	p.input(last.Add(time.Second), stamp, async)
	if calls != 5 || len(pending) != 0 {
		t.Fatalf("input after a stamp that left work owed: stamps=%d queued=%d, want it in line", calls, len(pending))
	}
	p.input(last.Add(2*time.Second), stamp, async)
	if calls != 5 || len(pending) != 1 {
		t.Fatalf("input once nothing is owed: stamps=%d queued=%d, want it queued", calls, len(pending))
	}
}

func TestPresenceStamper_OneStampInFlightPerHolder(t *testing.T) {
	var started atomic.Int32
	gate := make(chan struct{})
	var wg sync.WaitGroup
	stamp := func() bool {
		started.Add(1)
		<-gate
		return false
	}
	async := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
	}
	p := &presenceStamper{}
	now := time.Now()
	// The first input stamps in line, and returns once its stamp has: let it.
	first := make(chan struct{})
	go func() {
		defer close(first)
		p.input(now, stamp, async)
	}()
	waitForCount(t, &started, 1)
	gate <- struct{}{}
	<-first

	for i := 1; i <= 20; i++ {
		p.input(now.Add(time.Duration(i)*time.Millisecond), stamp, async)
	}
	waitForCount(t, &started, 2)
	time.Sleep(50 * time.Millisecond)
	if got := started.Load(); got != 2 {
		t.Fatalf("stamps started = %d for 20 quick inputs after the first, want 1 more", got)
	}
	gate <- struct{}{}
	wg.Wait()

	p.input(now.Add(time.Second), stamp, async)
	waitForCount(t, &started, 3)
	if got := started.Load(); got != 3 {
		t.Errorf("stamps started = %d, want the next input to start one more", got)
	}
	gate <- struct{}{}
	wg.Wait()
}
