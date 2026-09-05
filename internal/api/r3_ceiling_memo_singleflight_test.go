// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCeilingMemoIsSingleFlight is F174.
//
// The memo released its lock between the check and the fill, so two concurrent
// callers both missed, both resolved, and the LOSER returned its own pair rather
// than the memo's: the store was asked twice and the two callers received
// different profiles. That is exactly the divergence the memo was introduced to
// remove — a security admin narrowing a profile mid-request landing a run whose
// egress was clamped under one ceiling and whose grants were filtered under
// another.
//
// The resolver here hands out a DIFFERENT profile per call, so any second
// resolve is visible as a disagreement rather than inferred from a counter.
func TestCeilingMemoIsSingleFlight(t *testing.T) {
	const callers = 8
	memo := &ceilingMemo{}
	var calls atomic.Int64
	// The resolver HOLDS until every caller that is going to enter it has, or
	// until a short bound elapses. That makes the outcome deterministic in both
	// directions rather than dependent on scheduling: under last-writer-wins all
	// eight callers enter concurrently, the barrier fills and releases at once,
	// and the count is 8; under single-flight exactly one enters, waits out the
	// bound, and the count is 1. The ASSERTION is the count and the agreement,
	// never the timing.
	release := make(chan struct{})
	var once sync.Once
	resolve := func(context.Context) (governanceCeiling, error) {
		n := calls.Add(1)
		if n >= callers {
			once.Do(func() { close(release) })
		}
		select {
		case <-release:
		case <-time.After(100 * time.Millisecond):
		}
		return governanceCeiling{Profile: &types.GovernanceProfile{
			ID: uuid.New(), Name: "profile-" + strconv.FormatInt(n, 10),
		}}, nil
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen []string
	)
	start := make(chan struct{})
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			c, err := memo.do(context.Background(), resolve)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			seen = append(seen, c.Profile.Name)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("the resolver ran %d times for one memo, want 1 — the memo must resolve AT MOST ONCE, or two "+
			"reads of governance_assignments in one request can straddle a profile edit", got)
	}
	for _, name := range seen {
		if name != seen[0] {
			t.Fatalf("callers disagreed: %v — every site in one request must read the SAME answer, which is the "+
				"whole reason the memo exists", seen)
		}
	}
	if len(seen) != callers {
		t.Fatalf("only %d of %d callers got an answer", len(seen), callers)
	}
}

// TestCeilingMemoStoresTheFailure: a resolver FAILURE must be answered
// identically by every site too. Re-resolving after a failure could succeed on
// the retry and hand a later site a ceiling an earlier one refused on — the
// disagreement in its most dangerous direction, a refusal followed by a pass.
func TestCeilingMemoStoresTheFailure(t *testing.T) {
	memo := &ceilingMemo{}
	var calls atomic.Int64
	resolve := func(context.Context) (governanceCeiling, error) {
		if calls.Add(1) == 1 {
			return governanceCeiling{}, context.DeadlineExceeded
		}
		return governanceCeiling{Profile: &types.GovernanceProfile{Name: "would-have-succeeded"}}, nil
	}
	if _, err := memo.do(context.Background(), resolve); err == nil {
		t.Fatal("first call: want the resolver's error")
	}
	if _, err := memo.do(context.Background(), resolve); err == nil {
		t.Error("the second site got a ceiling where the first was refused — a memoized failure must stay a failure")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("resolver ran %d times, want 1", got)
	}
}
