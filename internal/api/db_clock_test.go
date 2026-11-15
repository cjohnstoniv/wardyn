// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errClockRead is a failed read of the database's clock.
var errClockRead = errors.New("database clock: connection reset")

// moveDBClock has the Postgres store srv decides on answer clock for the database's now() (store.PG's DBNow),
// for a test that moves the server's clock to reach a run's end.
func moveDBClock(t *testing.T, srv *Server, clock func() time.Time) {
	t.Helper()
	pg, ok := srv.cfg.Store.(store.PG)
	if !ok {
		t.Fatalf("the server's store is %T, not store.PG", srv.cfg.Store)
	}
	pg.DBNow = func() (time.Time, error) { return clock(), nil }
	srv.cfg.Store = pg
}

// The lease sweep decides a run's end on the database's clock (Server.dbNow), so this server's clock, ahead of it
// or behind it, changes no decision: a run ending 1 minute after the database's now is left alone, and one that
// ended 1 minute before it is ended and kept, with its end marked at the database's time. Before, leaseRun read
// the server's clock: 2 minutes ahead ended the run still inside its end, and 2 minutes behind left the one past
// it running.
func TestLeaseRun_TheEndIsTheDatabasesClock(t *testing.T) {
	for _, c := range []struct {
		name   string
		endsIn time.Duration
		ended  bool
	}{{"1 min before its end", time.Minute, false}, {"1 min after its end", -time.Minute, true}} {
		for _, skew := range oneClockSkews {
			t.Run(fmt.Sprintf("%s, server %v", c.name, skew), func(t *testing.T) {
				f := newLeaseFixture(t, c.endsIn)
				f.srv.cfg.Now = func() time.Time { return f.now.Add(skew) } // the fake database's clock stays f.now
				f.sweep(t)
				lostAt, reason := f.st.lost()
				if got := lostAt != nil && reason == types.LostEnded; got != c.ended {
					t.Fatalf("ended = %v (lost %v %q), want %v", got, lostAt, reason, c.ended)
				}
				if c.ended && !lostAt.Equal(f.now) {
					t.Errorf("lost_at = %v, want the database's time %v", lostAt, f.now)
				}
			})
		}
	}
}

// A kept run's grace runs out on the database's clock too: 1 minute inside it the run stays kept, and 1 minute
// past it the run is stopped, whatever this server's clock reads. Before, leaseRun compared the keep with the
// server's clock, so 2 minutes ahead stopped the run inside its grace.
func TestLeaseRun_TheKeptGraceIsTheDatabasesClock(t *testing.T) {
	for _, c := range []struct {
		name    string
		past    time.Duration // the database's time past the end mark
		stopped bool
	}{{"1 min inside the grace", 7*24*time.Hour - time.Minute, false}, {"1 min past the grace", 7*24*time.Hour + time.Minute, true}} {
		for _, skew := range oneClockSkews {
			t.Run(fmt.Sprintf("%s, server %v", c.name, skew), func(t *testing.T) {
				f := newLeaseFixture(t, -time.Minute)
				f.sweep(t) // marks the end at the database's now, f.now
				f.now = f.now.Add(c.past)
				f.srv.cfg.Now = func() time.Time { return f.now.Add(skew) }
				f.sweep(t)
				if got := f.st.State() == types.RunStopped; got != c.stopped {
					t.Errorf("stopped = %v (state %s), want %v", got, f.st.State(), c.stopped)
				}
			})
		}
	}
}

// A failed read of the database's clock ends, stops and marks nothing in the lease sweep: a run past its end stays
// RUNNING with no end mark and no sandbox end, and the next sweep, whose read works, ends it. Catches the error
// arm dropped (the server's clock standing in ends the run on it).
func TestLeaseRun_AClockReadFailureEndsNothing(t *testing.T) {
	f := newLeaseFixture(t, -time.Minute)
	f.st.clockErr = errClockRead
	f.sweep(t)
	if lostAt, _ := f.st.lost(); lostAt != nil || f.st.State() != types.RunRunning || f.rn.endCount() != 0 {
		t.Fatalf("lost %v, state %s, sandbox ends %d; want no mark, RUNNING and no end", lostAt, f.st.State(), f.rn.endCount())
	}
	f.st.clockErr = nil
	f.sweep(t)
	if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostEnded {
		t.Fatalf("after the read works: lost %v %q, want the end mark", lostAt, reason)
	}
}

// A kept run past its grace is not stopped while the database's clock cannot be read: it stays RUNNING and kept.
// Catches the error arm dropped (the server's clock standing in stops the run on it).
func TestLeaseRun_AClockReadFailureStopsNoKeptRun(t *testing.T) {
	f := newLeaseFixture(t, -time.Minute)
	f.sweep(t) // marks the end at the database's now, f.now
	f.now = f.now.Add(7*24*time.Hour + time.Minute)
	f.st.clockErr = errClockRead
	f.sweep(t)
	if f.st.State() != types.RunRunning {
		t.Fatalf("state = %s, want RUNNING: a failed clock read stops nothing", f.st.State())
	}
}
