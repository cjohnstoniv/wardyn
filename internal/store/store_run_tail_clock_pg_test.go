// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/testfloor"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// offsetSecs is the seeded offset of a run's time from the database's now(), in seconds (negative: in the
// past), for a failure message; nil is a NULL column.
func offsetSecs(p *float64) string {
	if p == nil {
		return "NULL"
	}
	return strconv.FormatFloat(*p, 'f', -1, 64) + " s"
}

// TestPG_StopKeptRunForcedIf_HoldOff pins the hold-off in PG.StopKeptRunForcedIf's WHERE against Postgres: it
// reads lost_at when fromLostAt is set and ends_at when not, adds the hold-off to it, and claims the run only
// when that is at or before the database's now(); a NULL time or a run that does not exist is no claim, never
// an error. Each case seeds lost_at and ends_at as seconds from now() in SQL, so nothing sleeps and no Go clock
// reaches a comparison, and passes the row's own mark back so only the hold-off decides. It turns red if the
// CASE reads the wrong column (the "ends_at" cases carry the other column on the opposite side of the line), if
// the hold-off is dropped or its sign flips (the cases a minute either side of the boundary: a margin, since
// the seed, the read and the claim are three statements, each with its own now()), or if a NULL time stops
// being no claim.
func TestPG_StopKeptRunForcedIf_HoldOff(t *testing.T) {
	testfloor.Mark(t, "pg")
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	secs := func(n float64) *float64 { return &n }

	for name, tc := range map[string]struct {
		lostAt, endsAt *float64 // seconds from now() the column is seeded at; nil: NULL
		fromLostAt     bool
		after          time.Duration
		want           bool
	}{
		"lost_at past":             {secs(-600), nil, true, 300 * time.Second, true},
		"lost_at inside":           {secs(-200), nil, true, 300 * time.Second, false},
		"ends_at past":             {secs(-10), secs(-600), false, 300 * time.Second, true},
		"ends_at inside":           {secs(-900), secs(-200), false, 300 * time.Second, false},
		"no ends_at":               {secs(-900), nil, false, 0, false},
		"no lost_at":               {nil, secs(-900), true, 0, false},
		"just past the boundary":   {secs(-360), nil, true, 300 * time.Second, true},
		"just before the boundary": {secs(-240), nil, true, 300 * time.Second, false},
	} {
		// RunRunning is the state the lease code holds a lost run in; the columns allow lost_at on it.
		r := persistRun(t, ctx, pool, newRun(types.RunRunning))
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at = now() + $2::float8 * interval '1 second', lost_reason = 'outage',
			ends_at = now() + $3::float8 * interval '1 second' WHERE id = $1`, r.ID, tc.lostAt, tc.endsAt); err != nil {
			t.Fatalf("%s: seed the row: %v", name, err)
		}
		seeded, err := pg.GetRun(ctx, r.ID)
		if err != nil {
			t.Fatalf("%s: read the row: %v", name, err)
		}
		_, got, err := pg.StopKeptRunForcedIf(ctx, r.ID, types.RunStopped, seeded.LostAt, seeded.LostReason, seeded.EndsAt, tc.fromLostAt, tc.after)
		if err != nil || got != tc.want {
			t.Errorf("%s: lost_at %s, ends_at %s, fromLostAt %v, after %v: claimed = %v, %v; want %v, nil",
				name, offsetSecs(tc.lostAt), offsetSecs(tc.endsAt), tc.fromLostAt, tc.after, got, err, tc.want)
		}
	}

	for _, fromLostAt := range []bool{true, false} {
		_, got, err := pg.StopKeptRunForcedIf(ctx, uuid.New(), types.RunStopped, nil, types.LostOutage, nil, fromLostAt, 0)
		if err != nil || got {
			t.Errorf("a run that does not exist, fromLostAt %v: claimed = %v, %v; want false, nil", fromLostAt, got, err)
		}
	}
}

// TestPG_RunEndsOpen pins the SQL of PG.RunEndsOpen against Postgres: endsOpen is true for a run with no end
// or an end after now(), and keepable is true for no end or an end plus the grace after now(), both on the
// database's clock; a run that does not exist is store.ErrNotFound. Each case seeds ends_at as seconds from
// now() in SQL, so nothing sleeps and no Go clock reaches a comparison. It turns red if the grace is dropped
// (the "ended inside the grace" case), if it is added to the wrong side or applied to endsOpen, or if a NULL
// end stops being true for both.
func TestPG_RunEndsOpen(t *testing.T) {
	testfloor.Mark(t, "pg")
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	secs := func(n float64) *float64 { return &n }

	for name, tc := range map[string]struct {
		endsAt       *float64 // seconds from now() ends_at is seeded at; nil: NULL
		grace        time.Duration
		wantEndsOpen bool
		wantKeepable bool
	}{
		"no end":                 {nil, 0, true, true},
		"end ahead":              {secs(60), 0, true, true},
		"ended inside the grace": {secs(-60), 120 * time.Second, false, true},
		"ended past the grace":   {secs(-60), 30 * time.Second, false, false},
	} {
		r := persistRun(t, ctx, pool, newRun(types.RunRunning))
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET ends_at = now() + $2::float8 * interval '1 second' WHERE id = $1`, r.ID, tc.endsAt); err != nil {
			t.Fatalf("%s: seed the row: %v", name, err)
		}
		endsOpen, keepable, err := pg.RunEndsOpen(ctx, r.ID, tc.grace)
		if err != nil || endsOpen != tc.wantEndsOpen || keepable != tc.wantKeepable {
			t.Errorf("%s: ends_at %s, grace %v: RunEndsOpen = endsOpen %v, keepable %v, %v; want endsOpen %v, keepable %v, nil",
				name, offsetSecs(tc.endsAt), tc.grace, endsOpen, keepable, err, tc.wantEndsOpen, tc.wantKeepable)
		}
	}

	if _, _, err := pg.RunEndsOpen(ctx, uuid.New(), 0); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a run that does not exist: RunEndsOpen error = %v; want store.ErrNotFound", err)
	}
}
