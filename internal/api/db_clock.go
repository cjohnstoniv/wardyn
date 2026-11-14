// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// dbNow is the database's now() for a decision about stored run times: a run's end and keep in the lease sweep,
// its lost mark, a revive's eligibility. The database stamps or compares those times, so a decision on this
// server's clock (Config.Now) would move by this replica's skew against it, and two replicas could decide one run
// differently. Read it once per decision and use the same value for any time the decision writes, so a mark and
// the compare-and-set that re-reads it agree. A store without the read (store.DatabaseClock), or one that answers
// zero (the tests' fakes), has this server's clock stand in. A failed read is logged here and returned: the caller
// then ends, stops and stamps nothing, and the next pass or request reads the clock again.
func (s *Server) dbNow(ctx context.Context) (time.Time, error) {
	c, ok := s.cfg.Store.(store.DatabaseClock)
	if !ok {
		return s.cfg.Now(), nil
	}
	at, err := c.DatabaseNow(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading the database's clock failed; the decision waits for the next pass", slog.Any("err", err))
		return time.Time{}, err
	}
	if at.IsZero() {
		return s.cfg.Now(), nil
	}
	return at.UTC(), nil
}

// untilOnDatabaseClock is how long until at on the database's clock, the clock that stamps ends_at and
// lifetime_cap_at. ok is false when the clock cannot be read: the caller decides what bound, if any, it then
// hands on, and the lease sweep still ends the run at its end.
func (s *Server) untilOnDatabaseClock(ctx context.Context, at time.Time) (time.Duration, bool) {
	now, err := s.dbNow(ctx)
	if err != nil {
		return 0, false
	}
	return at.Sub(now), true
}

// podLifetimeCap is the time a run's pods have left to its cap capAt, at least a second: the base of their
// deadline (activeDeadlineSeconds), counted on the database's clock, which stamps lifetime_cap_at. The deadline
// is a backstop behind the lease sweep's kill, and whether a pod carries one at all selects its quota scope
// (Terminating or NotTerminating), the scope the create's fit check judged the run under. So an unreadable
// database clock bounds it on this server's clock instead of sending none: a bound, not a decision.
func (s *Server) podLifetimeCap(ctx context.Context, capAt time.Time) time.Duration {
	left, ok := s.untilOnDatabaseClock(ctx, capAt)
	if !ok {
		left = capAt.Sub(s.cfg.Now())
	}
	return max(left, time.Second)
}
