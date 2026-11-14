// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The internal doors judge a run's lifetime cap on the database's clock, the clock that stamps lifetime_cap_at: a
// cap a minute past on this server's clock but a minute ahead on the database's lets a decision through. A failed
// read refuses with 503. Before, the server's clock refused the first and had no arm for the second.
func TestInternalDoors_TheCapIsTheDatabasesClock(t *testing.T) {
	for _, c := range []struct {
		name  string
		clock fixedDBClock
		want  int
	}{
		{"cap ahead on the database's clock", fixedDBClock{at: time.Now().UTC().Add(-2 * time.Minute)}, http.StatusAccepted},
		{"clock read fails", fixedDBClock{err: errClockRead}, http.StatusServiceUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			srv := New(baseTestConfig(h, struct {
				*lifetimeCapRunStore
				fixedDBClock
			}{&lifetimeCapRunStore{capAt: time.Now().UTC().Add(-time.Minute)}, c.clock}))
			tok := h.mintRunToken(t, uuid.New())
			w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok,
				`{"request":{"host":"api.anthropic.com","method":"POST"},"decision":"allow"}`)
			if w.Code != c.want {
				t.Fatalf("code = %d, want %d; body=%s", w.Code, c.want, w.Body.String())
			}
		})
	}
}

// The token renewal does not refuse as past the cap a run whose cap is ahead on the database's clock, and a failed
// clock read refuses with 503 (retried). Before, the server's clock refused the first and had no arm for the second.
func TestTokenRenew_TheCapIsTheDatabasesClock(t *testing.T) {
	for _, c := range []struct {
		name    string
		clock   fixedDBClock
		refused func(code int, reason string) bool
	}{
		{"cap ahead on the database's clock", fixedDBClock{at: time.Now().UTC().Add(-2 * time.Minute)},
			func(code int, reason string) bool { return code == http.StatusForbidden && reason == "run_terminal" }},
		{"clock read fails", fixedDBClock{err: errClockRead},
			func(code int, reason string) bool {
				return code != http.StatusServiceUnavailable || reason != reasonRunRenewReadFailed
			}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			srv := New(baseTestConfig(h, struct {
				*lifetimeCapRunStore
				fixedDBClock
			}{&lifetimeCapRunStore{capAt: time.Now().UTC().Add(-time.Minute)}, c.clock}))
			w := do(t, srv, http.MethodPost, "/api/v1/internal/token/renew", h.mintRunToken(t, uuid.New()), "")
			if c.refused(w.Code, errorReason(w)) {
				t.Fatalf("code = %d reason = %q; body=%s", w.Code, errorReason(w), w.Body.String())
			}
		})
	}
}

// An approval's lifetime-cap refusal reads the database's clock: a cap a minute ahead on it is not refused on a
// server 2 minutes ahead, and a failed read refuses as clock_unreadable with nothing decided.
func TestApprovalDecide_TheCapIsTheDatabasesClock(t *testing.T) {
	d := time.Now().UTC().Truncate(time.Second)
	capAt := d.Add(time.Minute)
	run := types.AgentRun{ID: uuid.New(), State: types.RunRunning, LifetimeCapAt: &capAt}
	ap := types.ApprovalRequest{ID: uuid.New(), RunID: run.ID, State: types.ApprovalPending}
	for _, c := range []struct {
		name     string
		skew     time.Duration
		clockErr error
		want     string
	}{{"server 2 min ahead", 2 * time.Minute, nil, ""}, {"clock read fails", 0, errClockRead, "clock_unreadable"}} {
		t.Run(c.name, func(t *testing.T) {
			s := reviveClockServer(d, c.skew, run, c.clockErr)
			if got := s.refuseIfRunEnded(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil), ap, run, true); got != c.want {
				t.Fatalf("refusal = %q, want %q", got, c.want)
			}
		})
	}
}

// untilOnDatabaseClock counts on the database's clock, and an unreadable clock gives no bound.
func TestUntilOnDatabaseClock(t *testing.T) {
	d := time.Now().UTC().Truncate(time.Second)
	if left, ok := reviveClockServer(d, 2*time.Minute, types.AgentRun{}, nil).untilOnDatabaseClock(context.Background(), d.Add(time.Hour)); !ok || left != time.Hour {
		t.Fatalf("left = %v, %v; want 1h on the database's clock", left, ok)
	}
	if _, ok := reviveClockServer(d, 0, types.AgentRun{}, errClockRead).untilOnDatabaseClock(context.Background(), d.Add(time.Hour)); ok {
		t.Fatal("a failed read gave a bound")
	}
}

// The pod deadline's base is the time left to the cap on the database's clock (1 h here, where this server's clock
// says 58 min), at least a second for a cap already past, and on a failed read it is bounded on this server's clock
// (1 h, the server's clock reading the database's time here) rather than sent as none. Catches the read reverted to
// the server's clock, and the failure arm sending 0 (no pod deadline, so another quota scope) or a second.
func TestPodLifetimeCap(t *testing.T) {
	d := time.Now().UTC().Truncate(time.Second)
	ctx := context.Background()
	for _, c := range []struct {
		name     string
		skew     time.Duration
		clockErr error
		capIn    time.Duration
		want     time.Duration
	}{
		{"database's clock, server 2 min ahead", 2 * time.Minute, nil, time.Hour, time.Hour},
		{"cap already past", 0, nil, -time.Minute, time.Second},
		{"clock read fails: the server's clock bounds it", 0, errClockRead, time.Hour, time.Hour},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := reviveClockServer(d, c.skew, types.AgentRun{}, c.clockErr).podLifetimeCap(ctx, d.Add(c.capIn)); got != c.want {
				t.Fatalf("pod lifetime = %v, want %v", got, c.want)
			}
		})
	}
}

// The seed wait is bounded by the time left to the run's end on the database's clock: 5 minutes left there, where
// this server's clock says the end passed 55 minutes ago, holds for 300 s; an unreadable clock leaves the configured
// 10 minutes. The audit row's wait_sec is the wait the hold used. Catches the call reverted to the server's clock
// (-3300) and a failed read taken as no time left.
func TestHoldForLocalFolder_TheWaitIsBoundedOnTheDatabasesClock(t *testing.T) {
	d := time.Now().UTC().Truncate(time.Second)
	for _, c := range []struct {
		name     string
		clockErr error
		want     float64
	}{{"5 min left on the database's clock", nil, 300}, {"clock read fails", errClockRead, 600}} {
		t.Run(c.name, func(t *testing.T) {
			s, ls, audit, run, _ := localFolderFixture(t)
			s.cfg.LocalFolderSeedWait = 10 * time.Minute
			s.cfg.Now = func() time.Time { return d.Add(time.Hour) }
			ls.dispatchTestStore.dbNow = func() time.Time { return d }
			ls.dispatchTestStore.clockErr = c.clockErr
			seeded := d
			ls.seed.SeededAt = &seeded // the first sync already landed: the hold returns at once
			ends := d.Add(5 * time.Minute)
			run.EndsAt = &ends
			if !s.holdForLocalFolder(context.Background(), run, "sbx") {
				t.Fatalf("the hold refused a seeded folder: hint %q", ls.FailureHint())
			}
			ev := findAudit(audit.snapshot(), run.ID, "run.seed.hold", "success")
			if ev == nil {
				t.Fatal("no run.seed.hold row")
			}
			if got := leaseAuditData(t, *ev)["wait_sec"]; got != c.want {
				t.Fatalf("wait_sec = %v, want %v", got, c.want)
			}
		})
	}
}

// A dispatched run's pod deadline counts on the database's clock: with the database 2 hours ahead of this server,
// a run capped 48 hours after its create gets about 46 hours, not the 48 the server's clock leaves. Catches
// dispatchRun reverted to the server's clock.
func TestPG_Dispatch_PodDeadlineIsTheDatabasesClock(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)
	srv.cfg.RunMaxAge = 48 * time.Hour
	moveDBClock(t, srv, func() time.Time { return time.Now().UTC().Add(2 * time.Hour) })
	got := dispatchAndCaptureSpec(t, srv, fr).Resources.LifetimeCap
	if got <= 45*time.Hour || got > 46*time.Hour {
		t.Fatalf("pod lifetime = %v, want what is left of 48 h on a database clock 2 h ahead (45-46 h)", got)
	}
}

// The re-clamp cuts an end to the database's now plus the new maximum and stamps end_tightened_at at that now; a
// failed read writes nothing. Before, both came from the server's clock.
func TestRunLimitsReclamp_TheCutIsTheDatabasesClock(t *testing.T) {
	tight := types.RunLimits{MaxEndAheadSec: 2 * 86400, MaxWaitSec: 3600}
	t.Run("cut", func(t *testing.T) {
		f, st, profile := newReclampFixture(t, types.RunLimits{MaxEndAheadSec: 30 * 86400, MaxWaitSec: 8 * 3600, UserChangesLimits: true}, 20*day, 8*3600)
		st.setProfile(profile, tight)
		db := f.now.Add(-10 * time.Minute)
		f.st.mu.Lock()
		f.st.dbNow = func() time.Time { return db }
		f.st.mu.Unlock()
		sweepLimits(t, f)
		run := st.current()
		if cut := db.Add(2 * day); run.EndsAt == nil || !run.EndsAt.Equal(cut) || run.EndTightenedAt == nil || !run.EndTightenedAt.Equal(db) {
			t.Fatalf("ends %v tightened %v; want %v and %v", run.EndsAt, run.EndTightenedAt, cut, db)
		}
	})
	t.Run("clock read fails", func(t *testing.T) {
		f, st, profile := newReclampFixture(t, types.RunLimits{MaxEndAheadSec: 30 * 86400, MaxWaitSec: 8 * 3600, UserChangesLimits: true}, 20*day, 8*3600)
		st.setProfile(profile, tight)
		from := *st.current().EndsAt
		f.st.mu.Lock()
		f.st.clockErr = errClockRead
		f.st.mu.Unlock()
		if err := f.srv.sweepRunLimits(context.Background()); err == nil {
			t.Fatal("sweepRunLimits = nil, want the clock error")
		}
		if run := st.current(); run.EndsAt == nil || !run.EndsAt.Equal(from) {
			t.Fatalf("ends %v, want unchanged %v", run.EndsAt, from)
		}
	})
}

// The undispatched finalize counts its grace on the database's clock: a run past the grace by this server's clock
// but inside it by the database's is left alone, and a failed read finalizes nothing.
func TestUndispatchedFinalize_TheGraceIsTheDatabasesClock(t *testing.T) {
	for _, c := range []struct {
		name    string
		clock   fixedDBClock
		wantErr bool
	}{
		{"inside the grace on the database's clock", fixedDBClock{at: time.Now().UTC().Add(-10 * time.Minute)}, false},
		{"clock read fails", fixedDBClock{err: errClockRead}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			run := execRun(t, "")
			run.SandboxRef = "" // never dispatched
			run.CreatedAt = time.Now().UTC().Add(-undispatchedGrace - time.Minute)
			fake := &bootReconcileStore{run: run}
			cfg := baseTestConfig(h, struct {
				*bootReconcileStore
				fixedDBClock
			}{fake, c.clock})
			cfg.Runner = &fakeRunner{}
			cfg.Broker = h.broker
			srv := testServer(t, cfg)
			if err := srv.finalizeUndispatchedRuns(context.Background()); (err != nil) != c.wantErr {
				t.Fatalf("finalizeUndispatchedRuns = %v, want error %v", err, c.wantErr)
			}
			if to, got := fake.finalTransition(); got {
				t.Fatalf("finalized to %q; want the run left alone", to)
			}
		})
	}
}
