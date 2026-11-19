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
// created_at is the database's reading less the app clock's age of r.CreatedAt.
//
// A run that carries the create's CapInputs has its end, its cap and the cap's source (args[41]: $42) decided
// from them again at that created_at: a span counted from created_at and an elevation's end are compared only
// here, on the database's clock, so a replica ahead of it or behind it never picks the later of the two.
//
// A run given without them keeps its end's and its cap's spans from created_at, except a cap an elevation set:
// that is the elevation's own end, which the database judges as stored, so it is kept as given. An end at or
// past the cap as given is stored as the cap itself; any other end never passes the cap. A run with no
// CreatedAt is inserted as given.
func stampRunTimes(anchor db.AppClockAnchor, r types.AgentRun, args []any) {
	if r.CreatedAt.IsZero() {
		return
	}
	created := anchor.Translate(r.CreatedAt)
	args[1] = created
	if r.CapInputs != nil {
		end, capAt, source := r.CapInputs.At(created)
		args[24], args[40], args[41] = end, capAt, string(source)
		return
	}
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
