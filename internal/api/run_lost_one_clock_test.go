// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
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

// An outage run whose end cannot be had from the database. A store without RunEndsOpen has its proxy cut and
// then its agent ended (a read will never succeed there). A read that fails has its proxy cut and nothing else
// stopped: the read's error comes back, so the run stays kept and the lease sweep reads again. The keep
// decision (lostRunKeepable) keeps the run on no answer either way, so the stop that follows can contain it.
// This changes the pin T2 first wrote, which wanted the agent ended with no proxy stop in both cases: an end the
// substrate cannot do sends nothing, so the proxy is cut before any end is tried.
func TestStopLostSandbox_NoAnswerFromTheDatabase(t *testing.T) {
	errRead := errors.New("database down")
	keepsOnNoAnswer := func(t *testing.T, f *lostFixture, run types.AgentRun) {
		t.Helper()
		f.srv.cfg.EndedRunGrace = time.Minute
		past, ended := run, f.now.Add(-time.Hour)
		past.EndsAt, past.LostAt, past.LostReason = &ended, nil, ""
		if !f.srv.lostRunKeepable(context.Background(), past) {
			t.Error("lostRunKeepable = false with no answer from the database; want the run kept")
		}
	}
	t.Run("a store without the read: cut, then end", func(t *testing.T) {
		f := newLostFixture(t)
		run := oneClockRun(f, f.now.Add(time.Minute))
		f.srv.cfg.Store = noEndsStore{f.ls}
		if _, ok := f.srv.cfg.Store.(store.RunEndsReader); ok {
			t.Fatal("fixture: the wrapped store still has RunEndsOpen")
		}
		if err := f.srv.stopLostSandbox(context.Background(), run); err != nil {
			t.Fatal(err)
		}
		if f.lr.endCount() != 1 || f.lr.proxyStopCount() != 1 {
			t.Errorf("ends %d, proxy stops %d; want the proxy cut and the agent ended (1, 1)", f.lr.endCount(), f.lr.proxyStopCount())
		}
		keepsOnNoAnswer(t, f, run)
	})
	t.Run("a read that fails: cut, no end, the read's error", func(t *testing.T) {
		f := newLostFixture(t)
		run := oneClockRun(f, f.now.Add(time.Minute))
		f.st.endsErr = errRead
		err := f.srv.stopLostSandbox(context.Background(), run)
		if !errors.Is(err, errRead) || errors.Is(err, runner.ErrEndUnsupported) || errors.Is(err, runner.ErrPendingOnRunner) {
			t.Errorf("err = %v; want the read's error and neither ErrEndUnsupported nor ErrPendingOnRunner", err)
		}
		if f.lr.endCount() != 0 || f.lr.proxyStopCount() != 1 {
			t.Errorf("ends %d, proxy stops %d; want the proxy cut and no end (0, 1)", f.lr.endCount(), f.lr.proxyStopCount())
		}
		keepsOnNoAnswer(t, f, run)
	})
}

// A cut that fails answers with its own error when the read failed, and an unsupported or queued cut is not let
// through as such (it would send the run to stopKeptRun); the read's error is the answer then.
func TestStopLostSandbox_AReadErrorAndACutThatFails(t *testing.T) {
	errRead, errCut := errors.New("database down"), errors.New("runner refused the cut")
	for _, c := range []struct {
		name    string
		cut     error
		wantCut bool // the cut's own error is the answer
	}{{"cut fails", errCut, true}, {"cut unsupported", runner.ErrEndUnsupported, false}, {"cut queued", runner.ErrPendingOnRunner, false}} {
		t.Run(c.name, func(t *testing.T) {
			f := newLostFixture(t)
			run := oneClockRun(f, f.now.Add(time.Minute))
			f.st.endsErr, f.lr.proxyErr = errRead, c.cut
			err := f.srv.stopLostSandbox(context.Background(), run)
			if c.wantCut && !errors.Is(err, errCut) {
				t.Errorf("err = %v; want the cut's error", err)
			}
			if !c.wantCut && (!errors.Is(err, errRead) || errors.Is(err, runner.ErrEndUnsupported) || errors.Is(err, runner.ErrPendingOnRunner)) {
				t.Errorf("err = %v; want the read's error alone", err)
			}
			if f.lr.endCount() != 0 || f.lr.proxyStopCount() != 1 {
				t.Errorf("ends %d, proxy stops %d; want (0, 1)", f.lr.endCount(), f.lr.proxyStopCount())
			}
		})
	}
}

// The generation path cuts first as well. A read that fails: "stop@3" and no "end@3", the read's error. No
// reader: "stop@3", then "end@3". RED before, where an unknown end called only "end@3".
func TestStopLostSandboxAt_AnUnknownEndCutsTheProxyFirst(t *testing.T) {
	errRead := errors.New("database down")
	t.Run("a read that fails", func(t *testing.T) {
		f, gr, _ := newGenStopFixture(t, 3)
		run := oneClockRun(f, f.now.Add(time.Minute))
		f.st.endsErr = errRead
		err := f.srv.stopLostSandboxAt(context.Background(), run, 3)
		if !errors.Is(err, errRead) {
			t.Errorf("err = %v; want the read's error", err)
		}
		if got := gr.atCalls(); len(got) != 1 || got[0] != "stop@3" {
			t.Errorf("generation stops = %v, want [stop@3]", got)
		}
	})
	t.Run("a store without the read", func(t *testing.T) {
		f, gr, gs := newGenStopFixture(t, 3)
		run := oneClockRun(f, f.now.Add(time.Minute))
		f.srv.cfg.Store = noEndsStore{gs}
		if err := f.srv.stopLostSandboxAt(context.Background(), run, 3); err != nil {
			t.Fatal(err)
		}
		if got := gr.atCalls(); len(got) != 2 || got[0] != "stop@3" || got[1] != "end@3" {
			t.Errorf("generation stops = %v, want [stop@3 end@3]", got)
		}
	})
	t.Run("a newer revive owns the run: no end call", func(t *testing.T) {
		f, gr, _ := newGenStopFixture(t, 4)
		run := oneClockRun(f, f.now.Add(time.Minute))
		f.st.endsErr = errRead
		if err := f.srv.stopLostSandboxAt(context.Background(), run, 3); err != nil {
			t.Fatal(err)
		}
		if got := gr.atCalls(); len(got) != 1 || got[0] != "stop@3" {
			t.Errorf("generation stops = %v, want [stop@3] and no end", got)
		}
	})
}

// An outage run on a runner that can stop a proxy but whose end is unsupported (a Kubernetes runner), with every
// end-hold slot taken, loses its proxy on the visit that finds it lost, as at the base. RED before: with the
// read failing the stop made one end call, got ErrEndUnsupported and went to stopKeptRun, whose forceKeptStop
// is held off at a fresh lost mark, so the proxy stops were 0. After: a failed read cuts the proxy (1), makes no
// end call and returns the read's error, so the run stays RUNNING and lost (kept); with no reader the proxy is
// cut (1), the end is tried, and the held-off stopKeptRun leaves the run as it is.
func TestLoseRunStopped_AnUnknownEndStillCutsTheProxyWithEveryEndHoldTaken(t *testing.T) {
	errRead := errors.New("database down")
	for _, noReader := range []bool{false, true} {
		name := "a read that fails"
		if noReader {
			name = "a store without the read"
		}
		t.Run(name, func(t *testing.T) {
			f := newLostFixture(t)
			run := oneClockRun(f, f.now.Add(time.Minute))
			run.LostAt, run.LostReason = nil, ""
			f.lr.endErr = runner.ErrEndUnsupported
			takeEndHolds(t, f.srv, maxEndHolds)
			if noReader {
				f.srv.cfg.Store = noEndsStore{f.ls}
			} else {
				f.st.endsErr = errRead
			}
			o, err := f.srv.loseRunStopped(context.Background(), f.ls, f.ls, run, types.LostOutage, types.RunFailed, time.Hour, f.now, 0)
			if o != loseApplied {
				t.Fatalf("outcome = %v, want loseApplied", o)
			}
			if f.lr.proxyStopCount() != 1 {
				t.Errorf("proxy stops = %d, want 1 on this visit", f.lr.proxyStopCount())
			}
			wantEnds := 0
			if noReader {
				wantEnds = 1
			}
			if f.lr.endCount() != wantEnds {
				t.Errorf("ends = %d, want %d", f.lr.endCount(), wantEnds)
			}
			if !noReader && (!errors.Is(err, errRead) || errors.Is(err, runner.ErrEndUnsupported)) {
				t.Errorf("err = %v; want the read's error", err)
			}
			cur, _ := f.ls.GetRun(context.Background(), run.ID)
			if lostAt, reason := f.ls.lost(); cur.State != types.RunRunning || lostAt == nil || reason != types.LostOutage {
				t.Errorf("run is %s, lost %v %q; want RUNNING with its outage mark (kept, revivable)", cur.State, lostAt, reason)
			}
		})
	}
}
