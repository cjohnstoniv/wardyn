// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package offline owns runner-local containment and replay state. It never
// grants authority: the organisation supplies every lease and pending action.
package offline

import (
	"errors"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// Clock maps org wall timestamps onto elapsed local monotonic time. Receipt
// times must come directly from time.Now, retaining their monotonic component.
type Clock struct {
	mu       sync.Mutex
	server   time.Time
	received time.Time
	since    func(time.Time) time.Duration
}

// NewClock uses time.Since in production; tests may supply elapsed monotonic time.
func NewClock(since func(time.Time) time.Duration) *Clock {
	if since == nil {
		since = time.Since
	}
	return &Clock{since: since}
}

// Hello is RunnerHooks.Clock, after H2 has verified the organisation binding.
func (c *Clock) Hello(h runnerwire.Hello, received time.Time) error {
	if h.ServerTime.IsZero() || received.IsZero() {
		return errors.New("offline clock requires org time and a local receipt")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.server, c.received = h.ServerTime, received
	return nil
}

// Remaining computes a duration once. Enforcement must subsequently subtract
// only monotonic elapsed time, never another wall-clock reading.
func (c *Clock) Remaining(at time.Time) (time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.server.IsZero() || at.IsZero() {
		return 0, errors.New("offline lease has no authenticated clock or deadline")
	}
	elapsed := c.since(c.received)
	if elapsed < 0 {
		return 0, errors.New("offline monotonic clock moved backwards")
	}
	return at.Sub(c.server.Add(elapsed)), nil
}

// TokenRemaining clamps a future renewal stamp so it cannot grant more than
// one lapse window. Repeated leases must not reset an existing token deadline.
func (c *Clock) TokenRemaining(renewed time.Time) (time.Duration, error) {
	if renewed.IsZero() {
		return 0, errors.New("offline lease has no renewal stamp")
	}
	remaining, err := c.Remaining(renewed.Add(identity.RunTokenLapseAfter))
	return min(remaining, identity.RunTokenLapseAfter), err
}
