// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reviveClockServer is a Server whose fake database answers d (or fails with clockErr) and whose own clock reads
// d plus skew, with run as the stored row.
func reviveClockServer(d time.Time, skew time.Duration, run types.AgentRun, clockErr error) *Server {
	return &Server{cfg: Config{
		Store:          &dispatchTestStore{run: run, state: types.RunRunning, dbNow: func() time.Time { return d }, clockErr: clockErr},
		Now:            func() time.Time { return d.Add(skew) },
		RunLeaseConfig: RunLeaseConfig{EndedRunGrace: 7 * 24 * time.Hour},
	}}
}

// keptClaim records the end condition a revive's claim was given, and claims nothing.
type keptClaim struct {
	store.RunReviver
	called bool
	kept   *store.EndedKept
}

func (k *keptClaim) MarkRunRevived(_ context.Context, _ uuid.UUID, _ types.LostReason, ended *store.EndedKept, _ int, _ bool) (bool, error) {
	k.called, k.kept = true, ended
	return false, nil
}

// A revive's eligibility reads the database's clock (Server.dbNow): a lost run 1 minute inside its end may be
// revived and one 1 minute past it is refused, whatever this server's clock reads. Before, reviveEligible read the
// server's clock, so 2 minutes ahead refused the run inside its end and 2 minutes behind offered a revive to the
// one past it.
func TestReviveEligible_TheEndIsTheDatabasesClock(t *testing.T) {
	d := time.Now().UTC().Truncate(time.Second)
	for _, c := range []struct {
		name   string
		endsIn time.Duration
		want   string // the refusal's reason; "" is eligible
	}{{"1 min before its end", time.Minute, ""}, {"1 min after its end", -time.Minute, reasonRevivePastEnd}} {
		for _, skew := range oneClockSkews {
			t.Run(fmt.Sprintf("%s, server %v", c.name, skew), func(t *testing.T) {
				ends, lost := d.Add(c.endsIn), d.Add(-time.Hour)
				run := types.AgentRun{ID: uuid.New(), State: types.RunRunning, SandboxRef: "ref-revive",
					EndsAt: &ends, LostAt: &lost, LostReason: types.LostOutage}
				got := ""
				if rerr := reviveClockServer(d, skew, run, nil).reviveEligible(context.Background(), run, false); rerr != nil {
					got = rerr.reason
				}
				if got != c.want {
					t.Errorf("refusal = %q, want %q", got, c.want)
				}
			})
		}
	}
}

// A revive's expiry re-check (reviveExpiryDue) and its claim's end condition read the database's clock: a run 1
// minute inside its kept end may be claimed and one 1 minute past it is refused, and the claim is handed the
// database's now. A failed read refuses each of the three with 503 and claims nothing. Before, all three read the
// server's clock; the failure subtest catches any of the three error arms dropped (the server's clock standing in
// answers "eligible" for that run).
func TestRevive_TheExpiryAndTheClaimAreTheDatabasesClock(t *testing.T) {
	d := time.Now().UTC().Truncate(time.Second)
	for _, skew := range oneClockSkews {
		for _, c := range []struct {
			name   string
			endsIn time.Duration
			want   string
		}{{"1 min inside its end", time.Minute - 7*24*time.Hour, ""}, {"1 min past its end", -time.Minute - 7*24*time.Hour, reasonRevivePastEnd}} {
			t.Run(fmt.Sprintf("expiry %s, server %v", c.name, skew), func(t *testing.T) {
				ends, lost := d.Add(c.endsIn), d.Add(-8*24*time.Hour)
				run := types.AgentRun{ID: uuid.New(), State: types.RunRunning, SandboxRef: "ref-revive", EndsAt: &ends, LostAt: &lost, LostReason: types.LostOutage}
				got := ""
				if rerr := reviveClockServer(d, skew, run, nil).reviveExpiryDue(context.Background(), run.ID); rerr != nil {
					got = rerr.reason
				}
				if got != c.want {
					t.Errorf("refusal = %q, want %q", got, c.want)
				}
			})
		}
		t.Run(fmt.Sprintf("claim, server %v", skew), func(t *testing.T) {
			lost := d.Add(-time.Hour)
			run := types.AgentRun{ID: uuid.New(), State: types.RunRunning, SandboxRef: "ref-revive", LostAt: &lost, LostReason: types.LostEnded}
			rec := &keptClaim{}
			reviveClockServer(d, skew, run, nil).claimReviveOwing(context.Background(), rec, run, false, store.PolicySlot{}, types.ActorHuman, "sub")
			if rec.kept == nil || !rec.kept.Now.Equal(d) {
				t.Fatalf("claim's end condition = %+v, want Now at the database's time %v", rec.kept, d)
			}
		})
	}
	t.Run("a clock read failure", func(t *testing.T) {
		lost := d.Add(-time.Hour)
		run := types.AgentRun{ID: uuid.New(), State: types.RunRunning, SandboxRef: "ref-revive", LostAt: &lost, LostReason: types.LostEnded}
		s, rec := reviveClockServer(d, 0, run, errClockRead), &keptClaim{}
		if rerr := s.reviveEligible(context.Background(), run, false); rerr == nil || rerr.status != http.StatusServiceUnavailable {
			t.Errorf("reviveEligible = %+v, want a 503", rerr)
		}
		if rerr := s.reviveExpiryDue(context.Background(), run.ID); rerr == nil || rerr.status != http.StatusServiceUnavailable {
			t.Errorf("reviveExpiryDue = %+v, want a 503", rerr)
		}
		if _, rerr := s.claimReviveOwing(context.Background(), rec, run, false, store.PolicySlot{}, types.ActorHuman, "sub"); rerr == nil ||
			rerr.status != http.StatusServiceUnavailable || rec.called {
			t.Errorf("claimReviveOwing = %+v, claimed %v; want a 503 and no claim", rerr, rec.called)
		}
	})
}

// The exec gate on a kept run (keptAgentStopped) reads the run's end on the database's clock. A failed read
// leaves the run inspectable, as a failed read of the persisted stop does, so no one is told its agent is stopped
// because a clock read failed. Before, it read the server's clock, so a server 2 minutes ahead refused an exec into
// a run inside its end.
func TestKeptAgentStopped_TheEndIsTheDatabasesClock(t *testing.T) {
	d := time.Now().UTC().Truncate(time.Second)
	for _, c := range []struct {
		name     string
		endsIn   time.Duration
		clockErr error
		want     bool
	}{{"1 min before its end", time.Minute, nil, false}, {"1 min after its end", -time.Minute, nil, true}, {"clock read fails", -time.Minute, errClockRead, false}} {
		for _, skew := range oneClockSkews {
			t.Run(fmt.Sprintf("%s, server %v", c.name, skew), func(t *testing.T) {
				ends, lost := d.Add(c.endsIn), d.Add(-time.Hour)
				run := types.AgentRun{ID: uuid.New(), State: types.RunRunning, EndsAt: &ends, LostAt: &lost, LostReason: types.LostOutage}
				if got := reviveClockServer(d, skew, run, c.clockErr).keptAgentStopped(context.Background(), run); got != c.want {
					t.Errorf("stopped = %v, want %v", got, c.want)
				}
			})
		}
	}
}

// The unstarted kept machine is dropped by the run's end on the database's clock, and a failed read drops nothing.
func TestKeptMachine_TheUnstartedDropIsTheDatabasesClock(t *testing.T) {
	d := time.Now().UTC().Truncate(time.Second)
	for _, c := range []struct {
		name     string
		endedAgo time.Duration
		clockErr error
		want     bool
	}{
		{"1 min short", keptMachineUnstartedAfter - time.Minute, nil, false},
		{"1 min past", keptMachineUnstartedAfter + time.Minute, nil, true},
		{"clock read fails", 2 * keptMachineUnstartedAfter, errClockRead, false},
	} {
		for _, skew := range oneClockSkews {
			t.Run(fmt.Sprintf("%s, server %v", c.name, skew), func(t *testing.T) {
				ended := d.Add(-c.endedAgo)
				run := types.AgentRun{ID: uuid.New(), EndedAt: &ended}
				if got := reviveClockServer(d, skew, run, c.clockErr).keptMachineUnstartedPast(context.Background(), run); got != c.want {
					t.Errorf("past = %v, want %v", got, c.want)
				}
			})
		}
	}
}

// The end/wait edit decides on the database's clock: an end the server's clock calls past but the database's does
// not is set; a lifetime cap the database's clock calls passed refuses; a failed read, of the request's clock or of
// the claim's own after the lock, is a 500 that changes nothing.
func TestSetRunEnd_TheDatabasesClockDecides(t *testing.T) {
	gated := types.RunLimits{UserChangesLimits: true, AllowNoEnd: true}
	t.Run("an end the server's clock calls past", func(t *testing.T) {
		f := newEndWaitFixture(t, gated)
		f.st.mu.Lock()
		f.st.dbNow = func() time.Time { return f.now.Add(-2 * time.Minute) }
		f.st.mu.Unlock()
		to := f.now.Add(-time.Minute)
		if code, _ := f.patch(t, ownerSession(t), endsAtBody(to)); code != http.StatusOK {
			t.Fatalf("status = %d, want 200: the end is ahead of the database's now", code)
		}
		if end, _ := f.stored(); end == nil || !end.Equal(to) {
			t.Errorf("stored end = %v, want %v", end, to)
		}
	})
	t.Run("a lifetime cap the database's clock calls passed", func(t *testing.T) {
		f := newEndWaitFixture(t, gated)
		capAt := f.now.Add(time.Minute)
		f.st.mu.Lock()
		f.st.run.LifetimeCapAt = &capAt
		f.st.dbNow = func() time.Time { return f.now.Add(2 * time.Minute) }
		f.st.mu.Unlock()
		if code, _ := f.patch(t, ownerSession(t), endsAtBody(f.now.Add(30*time.Second))); code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: the cap is past on the database's clock", code)
		}
		if end, _ := f.stored(); end == nil || !end.Equal(f.end) {
			t.Errorf("stored end = %v, want the run untouched", end)
		}
	})
	t.Run("a clock read failure", func(t *testing.T) {
		f := newEndWaitFixture(t, gated)
		f.st.mu.Lock()
		f.st.clockErr = errClockRead
		f.st.mu.Unlock()
		if code, _ := f.patch(t, ownerSession(t), endsAtBody(f.now.Add(48*time.Hour))); code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", code)
		}
		if end, _ := f.stored(); end == nil || !end.Equal(f.end) {
			t.Errorf("stored end = %v, want the run untouched", end)
		}
	})
	// The request's own read (the first) answers; every later read fails, so only the claim's read after the
	// lock fails. Catches the claim's error arm dropped (a zero time handed to the claim) or the claim reverted
	// to the server's clock: either way the end would be set and the answer 200.
	t.Run("a clock read failure at the claim", func(t *testing.T) {
		f := newEndWaitFixture(t, gated)
		f.st.mu.Lock()
		f.st.dbNow = func() time.Time { // DatabaseNow calls this under the fake store's lock
			f.st.clockErr = errClockRead
			return f.now
		}
		f.st.mu.Unlock()
		if code, _ := f.patch(t, ownerSession(t), endsAtBody(f.now.Add(time.Hour))); code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500: the claim's own clock read failed", code)
		}
		if end, _ := f.stored(); end == nil || !end.Equal(f.end) {
			t.Errorf("stored end = %v, want the run untouched", end)
		}
	})
}

// The run-secret sweep's grace counts from updated_at on the database's clock: a run terminal past the grace by
// this host's clock but inside it by the database's keeps its secrets, and a failed read evicts nothing. Before,
// the sweep read time.Now.
func TestSweepRunSecrets_TheGraceIsTheDatabasesClock(t *testing.T) {
	for _, c := range []struct {
		name  string
		clock fixedDBClock
	}{{"inside the grace on the database's clock", fixedDBClock{at: time.Now().UTC().Add(-10 * time.Minute)}}, {"clock read fails", fixedDBClock{err: errClockRead}}} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			run := agedRun(types.RunCompleted, RunSecretGrace+5*time.Minute)
			reg := secretmask.NewRegistry()
			reg.Add(run.ID, []byte("aged-secret-value"))
			cfg := baseTestConfig(h, struct {
				*sweepStore
				fixedDBClock
			}{&sweepStore{runs: []types.AgentRun{run}}, c.clock})
			cfg.MaskRegistry = reg
			if n, _ := New(cfg).SweepRunSecrets(context.Background()); n != 0 || len(reg.Snapshot(run.ID)) != 1 {
				t.Fatalf("evicted %d, held %d; want the secret kept", n, len(reg.Snapshot(run.ID)))
			}
		})
	}
}

// The unread agent cast's give-up counts from the run's end on the database's clock: a run past the give-up by this
// server's clock but 4 minutes short of it by the database's keeps its obligation, and so does every run while the
// database's clock cannot be read. Before, the server's clock decided; the failure case catches the error arm
// declining the recording.
func TestAgentCastSweep_TheGiveUpIsTheDatabasesClock(t *testing.T) {
	for _, c := range []struct {
		name  string
		clock fixedDBClock
	}{
		{"4 minutes short of the give-up on the database's clock", fixedDBClock{at: time.Now().Add(-5 * time.Minute)}},
		{"clock read fails", fixedDBClock{err: errClockRead}},
	} {
		t.Run(c.name, func(t *testing.T) {
			hung := newRecoveringRunner()
			hung.hang = true
			f, cs, _ := agentCastSweepFixture(t, hung, time.Now().Add(-agentCastReadGiveUp-time.Minute))
			f.srv.cfg.Store = struct {
				*castStore
				fixedDBClock
			}{cs, c.clock}
			if err := f.srv.SweepRunOutputs(t.Context()); !errors.Is(err, errAgentCastUnread) {
				t.Fatalf("sweep: %v, want the unread error", err)
			}
			if cs.marked() != "" || !cs.owing() {
				t.Fatalf("not_recorded = %q, owed %v; want no mark and the obligation kept", cs.marked(), cs.owing())
			}
		})
	}
}

// A promoted run's cap check reads the database's clock: a cap the database calls passed ends the start with the
// lifetime reason, whatever this server's clock reads, and a failed read sends the run back to its queue (a wait,
// not an end) carrying the clock's reason for a failure row. Before, the check read the server's clock, and a
// failed read had no arm. (The server's clock is 2 minutes behind the database at skew 0, so a cap 1 minute in
// the database's past is in the server's future: the old read left the run to start.)
func TestQueuedStart_TheCapIsTheDatabasesClock(t *testing.T) {
	d := time.Now().UTC().Truncate(time.Second)
	for _, skew := range oneClockSkews {
		t.Run(fmt.Sprintf("cap 1 min ago, server %v", skew), func(t *testing.T) {
			capAt := d.Add(-time.Minute)
			anchor := types.AgentRun{ID: uuid.New(), CreatedAt: d.Add(-time.Hour), LifetimeCapAt: &capAt}
			end := reviveClockServer(d, skew-2*time.Minute, anchor, nil).queuedLifetimeEnd(context.Background(), anchor)
			if end == nil || end.token != string(types.RunEndMaxLifetimeReached) || end.wait != "" {
				t.Fatalf("end = %+v, want the lifetime end", end)
			}
		})
	}
	t.Run("a clock read failure", func(t *testing.T) {
		capAt := d.Add(-time.Hour)
		anchor := types.AgentRun{ID: uuid.New(), CreatedAt: d.Add(-2 * time.Hour), LifetimeCapAt: &capAt}
		end := reviveClockServer(d, 0, anchor, errClockRead).queuedLifetimeEnd(context.Background(), anchor)
		if end == nil || end.wait != runnerpool.WaitAtCapacity || end.token != "" || !end.quiet || end.extra["reason"] != queueStartClockUnreadable {
			t.Fatalf("end = %+v, want a quiet requeue (wait %q), no end token, and the clock's reason", end, runnerpool.WaitAtCapacity)
		}
	})
}

// requeueOnlyStore answers RequeuePromotedRun as moved and counts it; every other method is the nil embedded
// interface's and is not called. store.Store (138 methods) and queueStore (17) share no method name, so the
// struct's method set is both.
type requeueOnlyStore struct {
	store.Store
	queueStore
	requeued int
}

func (s *requeueOnlyStore) RequeuePromotedRun(context.Context, store.RunQueueEntry, string) (bool, error) {
	s.requeued++
	return true, nil
}

// A promoted run whose cap check cannot read the database's clock goes back to its queue through endPromoted
// without kicking the queue, so a clock that stays unreadable is retried on the queue's sweep tick, not promoted
// again at once in a loop; an owner sign-in wait still kicks. The kicker is held "running", so a kick only marks
// it pending and starts no promotion pass. Catches endPromoted passing true or end.quiet as requeuePromoted's
// kick, and requeuePromoted kicking whatever it is told.
func TestQueuedStart_AClockReadFailureRequeuesWithoutAKick(t *testing.T) {
	d := time.Now().UTC().Truncate(time.Second)
	capAt := d.Add(-time.Hour)
	anchor := types.AgentRun{ID: uuid.New(), CreatedAt: d.Add(-2 * time.Hour), LifetimeCapAt: &capAt}
	for _, c := range []struct {
		name   string
		end    func() *queueEnd
		kicked bool
	}{
		{"clock read fails", func() *queueEnd {
			return reviveClockServer(d, 0, anchor, errClockRead).queuedLifetimeEnd(context.Background(), anchor)
		}, false},
		{"owner sign-in wait", func() *queueEnd { return &queueEnd{wait: runnerpool.WaitOwnerSignIn} }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			rs := &requeueOnlyStore{}
			s := &Server{cfg: Config{Store: rs}}
			s.queue.kick.running.Store(true)
			s.endPromoted(&queuedStart{run: anchor, base: context.Background()}, c.end())
			if rs.requeued != 1 {
				t.Fatalf("requeued %d, want the run back in its queue once", rs.requeued)
			}
			if got := s.queue.kick.pending.Load(); got != c.kicked {
				t.Fatalf("kicked = %v, want %v", got, c.kicked)
			}
		})
	}
}

// requeueFailsPG is the Postgres store whose requeue write fails, as it does when the database that could not
// answer the clock read cannot take the write either.
type requeueFailsPG struct{ store.PG }

func (requeueFailsPG) RequeuePromotedRun(context.Context, store.RunQueueEntry, string) (bool, error) {
	return false, errors.New("store: requeue promoted run: connection reset")
}

// A double fault (the cap check's clock read fails and the requeue that answer asks for cannot be written either)
// fails the promoted run, as every start whose requeue fails is (overseer ruling), and its run.queue.start failure
// row names the clock read. Catches the clock's reason dropped from the row (it would read queue_start_refused) and
// a double fault that leaves the run PENDING with no row.
func TestPG_QueuedStart_AClockAndRequeueFaultFailsTheRunNamingTheClock(t *testing.T) {
	e := newQueueEnv(t)
	pool := e.cappedPool("Busy", 1)
	waiting, _ := e.waitingPerson(pool, "")
	e.exec(`WITH moved AS (UPDATE agent_runs SET state = 'PENDING', admitted_at = now() WHERE id = $1 RETURNING id)
		DELETE FROM run_queue WHERE run_id IN (SELECT id FROM moved)`, waiting.ID)
	run := e.run(waiting.ID)
	pg := e.pg
	pg.DBNow = func() (time.Time, error) { return time.Time{}, errClockRead }
	e.srv.cfg.Store = requeueFailsPG{PG: pg}
	end := e.srv.queuedLifetimeEnd(context.Background(), run)
	if end == nil || end.wait == "" || !end.quiet {
		t.Fatalf("end = %+v, want a quiet requeue", end)
	}
	e.srv.endPromoted(e.startOf(queueAuthority{Sub: e.personP}, run), end)
	if got := e.run(waiting.ID); got.State != types.RunFailed {
		t.Fatalf("state = %s, want FAILED: the requeue could not be written", got.State)
	}
	rows := e.audits(run.ID, "run.queue.start")
	if len(rows) != 1 || rows[0].Outcome != "failure" || leaseAuditData(t, rows[0])["reason"] != queueStartClockUnreadable {
		t.Fatalf("run.queue.start rows = %+v, want one failure naming %q", rows, queueStartClockUnreadable)
	}
}
