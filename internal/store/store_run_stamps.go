// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// stampRunTimes puts a new run's created_at, ends_at and lifetime_cap_at (args[1], args[24], args[40]: $2, $25,
// $41 of createRunSQL) on the database's clock, from the reading anchor took in the insert's transaction. The
// readers of those columns (the lease sweep, the idle reaper, the revive and end-edit checks) compare them with
// the database's now(), so a value on this server's clock would move every one of them by this replica's skew.
// created_at is the database's reading less the app clock's age of r.CreatedAt; ends_at and the cap keep their
// spans from it. A cap an elevation set is that elevation's own end, which the database judges as stored, so it
// is kept as given. An end at or past the cap as given (the api cut it to the cap) is stored as the cap itself,
// so the replica's skew never moves an elevation-capped end; any other end never passes the cap. A run with no
// CreatedAt is inserted as given.
func stampRunTimes(anchor db.AppClockAnchor, r types.AgentRun, args []any) {
	if r.CreatedAt.IsZero() {
		return
	}
	created := anchor.Translate(r.CreatedAt)
	args[1] = created
	var capAt *time.Time
	if r.LifetimeCapAt != nil {
		at := *r.LifetimeCapAt
		if r.LifetimeCapSource != types.LimitSourceElevation {
			at = created.Add(r.LifetimeCapAt.Sub(r.CreatedAt))
		}
		capAt = &at
	}
	args[40] = capAt
	if r.EndsAt != nil {
		at := created.Add(r.EndsAt.Sub(r.CreatedAt))
		if capAt != nil && (at.After(*capAt) || !r.EndsAt.Before(*r.LifetimeCapAt)) {
			at = *capAt
		}
		args[24] = &at
	}
}
