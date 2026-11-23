// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package storehealth answers how long this replica has seen its database healthy.
package storehealth

import (
	"context"
	"sync"
	"time"
)

const (
	ResetAfter  = 30 * time.Second // no good ping for this long resets the healthy span
	PingEvery   = 5 * time.Second
	PingTimeout = 3 * time.Second
)

// Tracker times the database's good pings. Every reading is wall-clock time (the monotonic reading is
// stripped), so a host's sleep counts as time without a good ping. A nil Tracker is never recovering.
type Tracker struct {
	mu           sync.Mutex
	now          func() time.Time
	grace        time.Duration
	healthySince time.Time
	lastOK       time.Time
}

// New returns a tracker that reads time from now and treats the first grace of every healthy span as recovery.
// A grace of 0 turns Recovering off.
func New(now func() time.Time, grace time.Duration) *Tracker {
	return &Tracker{now: now, grace: grace}
}

// Observe records one ping result; nil is a good ping. In production only Run calls it: a caller of
// Observe(nil) could hide an outage, so it is exported for tests.
func (t *Tracker) Observe(err error) {
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	n := t.now().Round(0)
	if t.healthySince.IsZero() || n.Sub(t.lastOK) >= ResetAfter || n.Before(t.healthySince) {
		t.healthySince = n
	}
	t.lastOK = n
}

// Run pings at once, then every PingEvery, each on a PingTimeout context, until ctx is done. No ping runs
// after it returns.
func (t *Tracker) Run(ctx context.Context, ping func(context.Context) error) {
	tick := time.NewTicker(PingEvery)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		pctx, cancel := context.WithTimeout(ctx, PingTimeout)
		err := ping(pctx)
		cancel()
		t.Observe(err)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// HealthyFor is how long the database has answered without a gap of ResetAfter; 0 when it has not, or has
// not answered for ResetAfter.
func (t *Tracker) HealthyFor() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.healthyForLocked()
}

func (t *Tracker) healthyForLocked() time.Duration {
	n := t.now().Round(0)
	if t.healthySince.IsZero() || n.Sub(t.lastOK) >= ResetAfter {
		return 0
	}
	return max(0, n.Sub(t.healthySince))
}

// Recovering is true until the database has been healthy for the grace. It is false on a nil Tracker and
// when the grace is 0.
func (t *Tracker) Recovering() bool {
	if t == nil || t.grace == 0 {
		return false
	}
	return t.HealthyFor() < t.grace
}

// Grace is the recovery grace; 0 on a nil Tracker.
func (t *Tracker) Grace() time.Duration {
	if t == nil {
		return 0
	}
	return t.grace
}
