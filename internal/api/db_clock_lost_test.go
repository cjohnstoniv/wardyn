// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fixedDBClock answers DatabaseNow (store.DatabaseClock) with at, or fails it with err. A test embeds it beside
// a fake store that has no DatabaseNow of its own, so the server decides on that database time. Do not embed it
// beside a store that embeds dispatchTestStore at the same depth: two DatabaseNow at one depth cancel out.
type fixedDBClock struct {
	at  time.Time
	err error
}

func (c fixedDBClock) DatabaseNow(context.Context) (time.Time, error) { return c.at, c.err }

// failDBClock has the Postgres store srv decides on fail every read of the database's clock with err (store.PG's
// DBNow).
func failDBClock(t *testing.T, srv *Server, err error) {
	t.Helper()
	pg, ok := srv.cfg.Store.(store.PG)
	if !ok {
		t.Fatalf("the server's store is %T, not store.PG", srv.cfg.Store)
	}
	pg.DBNow = func() (time.Time, error) { return time.Time{}, err }
	srv.cfg.Store = pg
}

// A run's lost mark is stamped at the database's time, the clock every later decision reads it against
// (RunEndsOpen, the forced claim, the revive claim), not this server's.
func TestLostRun_TheMarkIsTheDatabasesTime(t *testing.T) {
	f := newLostFixture(t)
	f.srv.cfg.Now = func() time.Time { return f.now.Add(2 * time.Minute) } // the fake database's clock stays f.now
	f.ls.lapsed = true
	f.sweepTokens(t)
	if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostOutage || !lostAt.Equal(f.now) {
		t.Fatalf("lost = %v %q, want the outage mark at the database's time %v", lostAt, reason, f.now)
	}
}

// Every lost mark is stamped at the database's time, on each path that writes one, whatever this server's clock
// reads. Before, each read the server's clock, so a replica 2 minutes ahead marked runs 2 minutes late on the
// database's clock, and RunEndsOpen and the forced claim then read that mark against now().
func TestLostRun_EveryMarkIsTheDatabasesTime(t *testing.T) {
	ahead := func(f *lostFixture) { f.srv.cfg.Now = func() time.Time { return f.now.Add(2 * time.Minute) } }
	marked := func(t *testing.T, f *lostFixture, reason types.LostReason) {
		t.Helper()
		if lostAt, got := f.st.lost(); lostAt == nil || got != reason || !lostAt.Equal(f.now) {
			t.Fatalf("lost = %v %q, want the %q mark at the database's time %v", lostAt, got, reason, f.now)
		}
	}
	t.Run("reboot watcher", func(t *testing.T) {
		f := newLostFixture(t)
		ahead(f)
		code := 137
		f.srv.keepRebootedRun(context.Background(), f.run, runner.Status{State: types.RunFailed, ExitCode: &code})
		marked(t, f, types.LostReboot)
	})
	t.Run("gone mark", func(t *testing.T) {
		f := newLostFixture(t)
		ahead(f)
		var got time.Time
		claim := func(_ context.Context, at time.Time) (bool, error) { got = at; return false, nil }
		if f.srv.endGoneRun(context.Background(), store.PG{}, f.run, claim); !got.Equal(f.now) {
			t.Fatalf("claim at %v, want the database's time %v", got, f.now)
		}
	})
	t.Run("unconfirmed proxy", func(t *testing.T) {
		f, _, _ := newGenStopFixture(t, 3)
		ahead(f)
		f.srv.loseUnconfirmedRun(context.Background(), f.run, 3)
		marked(t, f, types.LostOutage)
	})
	t.Run("revive re-lose", func(t *testing.T) {
		f, _, _ := newGenStopFixture(t, 3)
		ahead(f)
		f.srv.reloseRun(context.Background(), f.run, 3)
		marked(t, f, types.LostOutage)
	})
}

// Each lost-mark path writes no mark and stops no proxy when the database's clock cannot be read, and says so to
// its caller in that path's own "not done" answer. Each subtest catches its path's error arm dropped (the
// server's clock standing in writes the mark).
func TestLostRun_AClockReadFailureMarksNothing(t *testing.T) {
	unmarked := func(t *testing.T, f *lostFixture) {
		t.Helper()
		if lostAt, _ := f.st.lost(); lostAt != nil || f.lr.proxyStopCount() != 0 || f.st.State() != types.RunRunning {
			t.Fatalf("lost %v, proxy stops %d, state %s; want no mark, no stop, RUNNING", lostAt, f.lr.proxyStopCount(), f.st.State())
		}
	}
	t.Run("lapsed-token sweep", func(t *testing.T) {
		f := newLostFixture(t)
		f.st.clockErr, f.ls.lapsed = errClockRead, true
		f.sweepTokens(t)
		unmarked(t, f)
	})
	t.Run("reboot watcher", func(t *testing.T) {
		f := newLostFixture(t)
		f.st.clockErr = errClockRead
		code := 137
		if !f.srv.keepRebootedRun(context.Background(), f.run, runner.Status{State: types.RunFailed, ExitCode: &code}) {
			t.Fatal("keepRebootedRun = false: its caller would finalize the run")
		}
		unmarked(t, f)
	})
	t.Run("gone mark", func(t *testing.T) {
		f := newLostFixture(t)
		f.st.clockErr = errClockRead
		called := false
		claim := func(context.Context, time.Time) (bool, error) { called = true; return true, nil }
		if f.srv.endGoneRun(context.Background(), store.PG{}, f.run, claim) || called {
			t.Fatalf("endGoneRun claimed (called %v); want no claim", called)
		}
	})
	t.Run("unconfirmed proxy", func(t *testing.T) {
		f := newLostFixture(t)
		f.st.clockErr = errClockRead
		if handled, err := f.srv.loseUnconfirmedRun(context.Background(), f.run, 0); !handled || err == nil {
			t.Fatalf("loseUnconfirmedRun = %v, %v; want handled with an error (unresolved, retried)", handled, err)
		}
		unmarked(t, f)
	})
}

// A revive that failed after its claim and cannot read the database's clock for its lost mark is contained as a
// mark that could not be written is: no mark, its broker revoked once, and its proxy stopped under the revive's
// own generation (a newer revive's proxy survives), answered relostUnwritten. Catches the arm answering
// relostUnknown with nothing revoked or stopped (a fail-open), the revoke or the stop dropped, or a plain
// StopProxy in place of the fenced one.
func TestRelose_AClockReadFailureContainsTheRun(t *testing.T) {
	eachProxyGeneration(t, func(t *testing.T, proxyGen int, survives bool) {
		f, gr, _ := newGenStopFixture(t, proxyGen)
		f.st.clockErr = errClockRead
		fate, err := f.srv.reloseRun(context.Background(), f.run, 3)
		if fate != relostUnwritten || err != nil {
			t.Fatalf("reloseRun = %v, %v; want relostUnwritten, nil", fate, err)
		}
		if lostAt, _ := f.st.lost(); lostAt != nil {
			t.Errorf("lost_at = %v, want no mark", lostAt)
		}
		if n := f.brk.count(f.run.ID); n != 1 {
			t.Errorf("broker revocations = %d, want 1", n)
		}
		assertFencedStop(t, f, gr, survives)
	})
}

// An offline runner's lapsed run gets no lost mark and no proxy stop while the database's clock cannot be read:
// OfflineFromRunner returns the read's error, as a failed listing does, and the runner's next report retries;
// once the read works the mark lands. Catches the error arm dropped (the server's clock standing in marks the
// run) or turned into a skip that reports success.
func TestPG_OfflineFromRunner_AClockReadFailureMarksNothing(t *testing.T) {
	pool, cfg, rn, run, _, _ := runnerExitAPIFixture(t)
	ctx := t.Context()
	st := cfg.Store.(store.PG)
	probe := &offlineContainmentProbe{fakeRunner: &fakeRunner{}}
	cfg.Runner = probe
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET interactive=false, ends_at=NULL, token_renewed_at=now()-interval '2 hours' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	srv := New(cfg)
	failDBClock(t, srv, errClockRead)
	if err := srv.OfflineFromRunner(ctx, rn.ID); !errors.Is(err, errClockRead) {
		t.Fatalf("OfflineFromRunner = %v, want the clock read's error", err)
	}
	if got, _ := st.GetRun(ctx, run.ID); got.LostAt != nil || probe.stops.Load() != 0 {
		t.Fatalf("lost_at %v, proxy stops %d; want no mark and no stop", got.LostAt, probe.stops.Load())
	}
	srv.cfg.Store = st
	if err := srv.OfflineFromRunner(ctx, rn.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetRun(ctx, run.ID); got.LostAt == nil || got.LostReason != types.LostOutage {
		t.Fatalf("after the read works: lost %v %q, want the outage mark", got.LostAt, got.LostReason)
	}
}
