// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// noEndsStore hides every optional interface of the store it wraps, store.RunEndsReader among them.
type noEndsStore struct{ store.Store }

// oneClockSkews are the offsets of the server's clock against the fake database's (f.now): the decision of a
// lost run's end is the database's, so none of them may change it.
var oneClockSkews = []time.Duration{0, 2 * time.Minute, -2 * time.Minute}

// oneClockRun is the fixture's run lost by an outage at the database's now, ending at ends, as the run is in
// hand and as its row is stored.
func oneClockRun(f *lostFixture, ends time.Time) types.AgentRun {
	f.st.mu.Lock()
	f.st.run.EndsAt = &ends
	f.st.mu.Unlock()
	run := f.run
	at := f.now
	run.State, run.EndsAt, run.LostAt, run.LostReason = types.RunRunning, &ends, &at, types.LostOutage
	return run
}

// A lost run still inside its end has its proxy stopped alone, and one past its end has its agent stopped as
// at the end, whatever the server's clock reads against the database's. Both stop functions decide it.
// RED before, with the Go comparison `now.Before(*run.EndsAt)` on the server's clock: a run ending 1 minute
// after the database's now (D+1), on a server 2 minutes ahead, reads now = D+2, Before(D+1) is false, so its
// agent is stopped; a run that ended 1 minute before it (D-1), on a server 2 minutes behind, reads now = D-2,
// Before(D-1) is true, so its agent is kept running. GREEN after: the fake database answers D+1 > D (open)
// and D-1 > D (past) at every skew.
func TestStopLostSandbox_TheEndIsTheDatabasesClock(t *testing.T) {
	for _, c := range []struct {
		name      string
		endsIn    time.Duration
		proxyOnly bool
	}{{"1 min before its end", time.Minute, true}, {"1 min after its end", -time.Minute, false}} {
		for _, skew := range oneClockSkews {
			t.Run(fmt.Sprintf("%s, server %v", c.name, skew), func(t *testing.T) {
				t.Run("stopLostSandbox", func(t *testing.T) {
					f := newLostFixture(t)
					run := oneClockRun(f, f.now.Add(c.endsIn))
					f.srv.cfg.Now = func() time.Time { return f.now.Add(skew) }
					if err := f.srv.stopLostSandbox(context.Background(), run); err != nil {
						t.Fatal(err)
					}
					wantProxy, wantEnd := 0, 1
					if c.proxyOnly {
						wantProxy, wantEnd = 1, 0
					}
					if f.lr.proxyStopCount() != wantProxy || f.lr.endCount() != wantEnd {
						t.Errorf("proxy stops %d, ends %d; want %d and %d", f.lr.proxyStopCount(), f.lr.endCount(), wantProxy, wantEnd)
					}
				})
				t.Run("stopLostSandboxAt", func(t *testing.T) {
					f, gr, _ := newGenStopFixture(t, 3)
					run := oneClockRun(f, f.now.Add(c.endsIn))
					f.srv.cfg.Now = func() time.Time { return f.now.Add(skew) }
					if err := f.srv.stopLostSandboxAt(context.Background(), run, 3); err != nil {
						t.Fatal(err)
					}
					want := "end@3"
					if c.proxyOnly {
						want = "stop@3"
					}
					if got := gr.atCalls(); len(got) != 1 || got[0] != want {
						t.Errorf("generation stops = %v, want [%s]", got, want)
					}
				})
			})
		}
	}
}

// lostRunKeepable's "inside its end plus the files grace" is the database's too. With a 5 minute grace, a run
// that ended 4 minutes before the database's now (D-4) is keepable, and one that ended 6 minutes before (D-6)
// is not. RED before, with `now.Before(run.EndsAt.Add(grace))` on the server's clock: a server 2 minutes ahead
// reads now = D+2, and D-4+5 = D+1, Before is false, so the keepable run is torn down; a server 2 minutes
// behind reads now = D-2, and D-6+5 = D-1, Before(D-2) is true, so the run past its grace is kept. GREEN after:
// the fake database answers D+1 > D (keepable) and D-1 > D (not) at every skew.
func TestLostRunKeepable_TheGraceIsTheDatabasesClock(t *testing.T) {
	for _, c := range []struct {
		name   string
		endsIn time.Duration
		want   bool
	}{{"ended 4 min ago, grace 5", -4 * time.Minute, true}, {"ended 6 min ago, grace 5", -6 * time.Minute, false}} {
		for _, skew := range oneClockSkews {
			t.Run(fmt.Sprintf("%s, server %v", c.name, skew), func(t *testing.T) {
				f := newLostFixture(t)
				f.srv.cfg.EndedRunGrace = 5 * time.Minute
				run := oneClockRun(f, f.now.Add(c.endsIn))
				run.LostAt, run.LostReason = nil, ""
				f.srv.cfg.Now = func() time.Time { return f.now.Add(skew) }
				if got := f.srv.lostRunKeepable(context.Background(), run); got != c.want {
					t.Errorf("lostRunKeepable = %v, want %v", got, c.want)
				}
			})
		}
	}
}

// With no answer from the database the outage stop is the stronger one: a store without RunEndsOpen, and a
// read that fails, both end the agent of a run inside its end (nothing keeps running on an unknown end). The
// keep decision (lostRunKeepable) is the other way: it keeps the run on no answer, so the stop that follows
// contains it and the files are not torn down on a failed read.
func TestStopLostSandbox_NoAnswerFromTheDatabase(t *testing.T) {
	check := func(t *testing.T, f *lostFixture, run types.AgentRun) {
		t.Helper()
		if err := f.srv.stopLostSandbox(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		if f.lr.endCount() != 1 || f.lr.proxyStopCount() != 0 {
			t.Errorf("ends %d, proxy stops %d; want the agent ended (1, 0)", f.lr.endCount(), f.lr.proxyStopCount())
		}
		f.srv.cfg.EndedRunGrace = time.Minute
		past, ended := run, f.now.Add(-time.Hour)
		past.EndsAt, past.LostAt, past.LostReason = &ended, nil, ""
		if !f.srv.lostRunKeepable(context.Background(), past) {
			t.Error("lostRunKeepable = false with no answer from the database; want the run kept")
		}
	}
	t.Run("a store without the read", func(t *testing.T) {
		f := newLostFixture(t)
		run := oneClockRun(f, f.now.Add(time.Minute))
		f.srv.cfg.Store = noEndsStore{f.ls}
		if _, ok := f.srv.cfg.Store.(store.RunEndsReader); ok {
			t.Fatal("fixture: the wrapped store still has RunEndsOpen")
		}
		check(t, f, run)
	})
	t.Run("a read that fails", func(t *testing.T) {
		f := newLostFixture(t)
		run := oneClockRun(f, f.now.Add(time.Minute))
		f.st.endsErr = errors.New("database down")
		check(t, f, run)
	})
}
