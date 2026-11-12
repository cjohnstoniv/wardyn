// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package offline owns runner-local containment and replay state. It never
// grants authority: the organisation supplies every lease and pending action.
package offline

import (
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner/srvclock"
)

// Clock maps org wall timestamps onto elapsed local monotonic time (srvclock.Clock).
type Clock = srvclock.Clock

// NewClock uses time.Since in production; tests may supply elapsed monotonic time.
func NewClock(since func(time.Time) time.Duration) *Clock { return srvclock.NewClock(since) }
