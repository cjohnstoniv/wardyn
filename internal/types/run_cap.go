// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// RunCapInputs are what a new run's end and lifetime cap are decided from. End and Span count from the run's
// created_at, so they hold on whichever clock created_at is read; Elevation is an instant, the end of the
// elevation the run is created under, which the database judges as it is stored. Which of the span and the
// elevation's end comes first therefore depends on the clock created_at is on: the create decides them on its
// own clock for its own reads, and the insert decides them again once created_at is on the database's clock,
// and that decision is the one the row keeps.
type RunCapInputs struct {
	End        *time.Duration // the end asked for or defaulted; nil is No end
	Span       time.Duration  // the pool's, governance's or the deployment's maximum; 0 is none
	SpanSource LimitSource
	Elevation  *time.Time
	// EndAtCap ends a run that has no end of its own at its cap, not at No end (the record path).
	EndAtCap bool
}

// At is the end, the lifetime cap and the cap's source of a run created at created: the cap is the earlier of
// the span and the elevation's end (the span on a tie), and the end is never past the cap.
func (c RunCapInputs) At(created time.Time) (end, capAt *time.Time, source LimitSource) {
	if c.Span > 0 {
		at := created.Add(c.Span)
		capAt, source = &at, c.SpanSource
	}
	if c.Elevation != nil && (capAt == nil || c.Elevation.Before(*capAt)) {
		at := *c.Elevation
		capAt, source = &at, LimitSourceElevation
	}
	switch {
	case c.End != nil:
		at := created.Add(*c.End)
		end = &at
	case c.EndAtCap:
		end = capAt
	}
	if end != nil && capAt != nil && end.After(*capAt) {
		end = capAt
	}
	return end, capAt, source
}
