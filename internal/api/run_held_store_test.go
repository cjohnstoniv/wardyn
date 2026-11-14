// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// unsettledStore is dispatchTestStore whose state write answers store.ErrRunWriteUnsettled, as a COMMIT sent and not
// answered does: land says whether the write committed. Its settle reads the state, or fails with settleErr.
type unsettledStore struct {
	*dispatchTestStore
	land      bool
	settleErr error
}

func (s *unsettledStore) UpdateRunStateIf(ctx context.Context, id uuid.UUID, from, to types.RunState) (bool, error) {
	if s.land {
		if _, err := s.dispatchTestStore.UpdateRunStateIf(ctx, id, from, to); err != nil {
			return false, err
		}
	}
	return false, fmt.Errorf("%w: injected: the connection closed before the commit's reply", store.ErrRunWriteUnsettled)
}

func (s *unsettledStore) SettleRunState(context.Context, uuid.UUID) (types.RunState, error) {
	if s.settleErr != nil {
		return "", s.settleErr
	}
	return s.State(), nil
}

// timedOutWriteStore is dispatchTestStore whose state write the database ended before its COMMIT (a statement
// timeout). Its settle would read the target state: a write that sent no COMMIT must not be settled at all.
type timedOutWriteStore struct{ *dispatchTestStore }

func (s *timedOutWriteStore) UpdateRunStateIf(context.Context, uuid.UUID, types.RunState, types.RunState) (bool, error) {
	return false, errors.New("injected: canceling statement due to statement timeout")
}

func (s *timedOutWriteStore) SettleRunState(context.Context, uuid.UUID) (types.RunState, error) {
	return types.RunFailed, nil
}

// TestCasRunStateHeld_AnUnsettledWriteIsSettledBehindItsRowLock: a holder's state write whose COMMIT was not answered
// is settled by the store's locked read. One that committed reads as applied, one that did not is an error, one
// another writer beat reads as not applied, and a settle that fails keeps the error. A write that failed before its
// COMMIT is answered as it is, with no settle. Catches: settling every error (an error before the COMMIT read as
// applied), and an unsettled write answered as not applied with no error.
func TestCasRunStateHeld_AnUnsettledWriteIsSettledBehindItsRowLock(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name        string
		st          func(*dispatchTestStore) store.Store
		state       types.RunState
		wantApplied bool
		wantErr     bool
	}{
		{"a commit that landed, its answer lost", func(d *dispatchTestStore) store.Store { return &unsettledStore{dispatchTestStore: d, land: true} }, types.RunRunning, true, false},
		{"a commit that did not land", func(d *dispatchTestStore) store.Store { return &unsettledStore{dispatchTestStore: d} }, types.RunRunning, false, true},
		{"a move another writer won", func(d *dispatchTestStore) store.Store { return &unsettledStore{dispatchTestStore: d} }, types.RunKilled, false, false},
		{"a settle that fails", func(d *dispatchTestStore) store.Store {
			return &unsettledStore{dispatchTestStore: d, land: true, settleErr: errors.New("injected: settle timed out")}
		}, types.RunRunning, false, true},
		{"an error before the commit", func(d *dispatchTestStore) store.Store { return &timedOutWriteStore{d} }, types.RunRunning, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newFinalizeRun()
			d := &dispatchTestStore{run: run, state: tc.state}
			srv := New(baseTestConfig(newHarness(t), tc.st(d)))
			applied, err := srv.casRunStateHeld(ctx, run.ID, types.RunRunning, types.RunFailed)
			if applied != tc.wantApplied || (err != nil) != tc.wantErr {
				t.Fatalf("applied %v, err %v; want applied %v, an error %v", applied, err, tc.wantApplied, tc.wantErr)
			}
		})
	}
}

// hungGetPauseStore is pauseStore whose run read never answers: it waits for its context to end.
type hungGetPauseStore struct{ *pauseStore }

func (s *hungGetPauseStore) GetRun(ctx context.Context, _ uuid.UUID) (types.AgentRun, error) {
	<-ctx.Done()
	return types.AgentRun{}, ctx.Err()
}

// TestStoreOnlyHolders_AHungReadEndsAtItsBound: the pause sweep's resume holds the run's lock on the sweep's context,
// which has no deadline. A read under it that never answers ends at heldStoreCallBound: the resume returns and the
// run's lock is free again.
func TestStoreOnlyHolders_AHungReadEndsAtItsBound(t *testing.T) {
	old := heldStoreCallBound
	heldStoreCallBound = 50 * time.Millisecond
	t.Cleanup(func() { heldStoreCallBound = old })
	f := newPauseFixture(t, time.Minute)
	f.srv.cfg.Store = &hungGetPauseStore{f.st}
	lockFreesWithin(t, f.srv, f.run.ID, "the sweep's resume", func(ctx context.Context) {
		f.srv.resumeClosedRun(ctx, f.st, store.PauseCandidate{Run: f.run})
	})
}

// hungDueLab is tailLab whose containment reads never answer: they wait for their context to end.
type hungDueLab struct{ *tailLab }

func (hungDueLab) ContainmentPastDue(ctx context.Context, _ uuid.UUID, _ time.Duration) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

func (hungDueLab) ContainmentStreakDue(ctx context.Context, _ uuid.UUID, _, _ time.Duration, _ *time.Duration) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

// TestTailOwner_AHungStopOwedReadFreesTheRunLock: a folder pass's stop-owed check runs under the run's lock on the
// cleanup loop's context, which has no deadline. Its containment due read never answers: the check ends at
// heldStoreCallBound and answers owed, so no pass runs, and the tick returns with the run's lock free. Catches: the
// check on the loop's context.
func TestTailOwner_AHungStopOwedReadFreesTheRunLock(t *testing.T) {
	old := heldStoreCallBound
	heldStoreCallBound = 50 * time.Millisecond
	t.Cleanup(func() { heldStoreCallBound = old })
	g := newTailRig(t)
	row := g.owedRow(g.clock.now.Add(-time.Hour))
	g.s.cfg.Store = hungDueLab{g.lab}
	lockFreesWithin(t, g.s, row.run.ID, "the tail owner's folder pass", func(ctx context.Context) { g.s.folderCleanupTick(ctx) })
	if g.count(tailFolder) != 0 {
		t.Fatalf("%d folder passes after a stop-owed read that never answered; want none (a cut read answers owed)", g.count(tailFolder))
	}
}

// hungStampPauseStore is pauseStore whose presence stamp never answers.
type hungStampPauseStore struct{ *pauseStore }

func (*hungStampPauseStore) StampRunActive(ctx context.Context, _ uuid.UUID) (bool, error) {
	<-ctx.Done()
	return false, ctx.Err()
}

// TestThawForExec_AHungPresenceStampFreesTheRunLock: a thaw before an exec stamps the run's presence under the run's
// lock, on a request's context with no deadline. A stamp that never answers ends at heldStoreCallBound, and the thaw
// returns with the run's lock free. Catches: the stamp on the caller's context.
func TestThawForExec_AHungPresenceStampFreesTheRunLock(t *testing.T) {
	old := heldStoreCallBound
	heldStoreCallBound = 50 * time.Millisecond
	t.Cleanup(func() { heldStoreCallBound = old })
	f := newPauseFixture(t, time.Minute)
	f.srv.cfg.Store = &hungStampPauseStore{f.st}
	lockFreesWithin(t, f.srv, f.run.ID, "a thaw before an exec", func(ctx context.Context) {
		_ = f.srv.thawForExec(ctx, f.run, types.ActorHuman, pauseOwner, "presence")
	})
}

// hungAbandonStore is readbackStore whose abandon of a proxy start never answers.
type hungAbandonStore struct {
	*readbackStore
	calls atomic.Int32
}

func (s *hungAbandonStore) AbandonProxyStart(ctx context.Context, _ uuid.UUID, _ int) error {
	s.calls.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

// TestReadback_AFailedReviveWhoseAbandonNeverAnswersFreesTheRunLock: a revive whose replace fails abandons its
// start under the run's lock, detached from the replace's end. An abandon that never answers ends at
// heldStoreCallBound: the revive answers 502 and the run's lock is free. Catches: the abandon on a context with its
// cancel removed and no bound.
func TestReadback_AFailedReviveWhoseAbandonNeverAnswersFreesTheRunLock(t *testing.T) {
	old := heldStoreCallBound
	heldStoreCallBound = 50 * time.Millisecond
	t.Cleanup(func() { heldStoreCallBound = old })
	f := newReadbackFixture(t)
	if code := f.revive(t); code != http.StatusOK {
		t.Fatalf("first revive: code %d, want 200", code)
	}
	f.srv.liveRevived.Delete(f.run.ID)
	f.rr.replaceErr = errors.New("docker: inspect failed")
	hung := &hungAbandonStore{readbackStore: f.rb}
	f.srv.cfg.Store = hung
	var code int
	lockFreesWithin(t, f.srv, f.run.ID, "a failed revive's abandon", func(context.Context) { code = f.revive(t) })
	if code != http.StatusBadGateway || hung.calls.Load() != 1 {
		t.Fatalf("revive code %d, %d abandons; want 502 and the abandon reached once", code, hung.calls.Load())
	}
}

// unsettledStopStore is leaseStore whose kept-run stop commits and answers store.ErrRunWriteUnsettled; with hang,
// its stop never answers instead. Its settle reads the state.
type unsettledStopStore struct {
	*leaseStore
	hang bool
}

func (s unsettledStopStore) StopKeptRunIf(ctx context.Context, id uuid.UUID, to types.RunState, lostAt *time.Time, reason types.LostReason, endsAt *time.Time) (bool, error) {
	if s.hang {
		<-ctx.Done()
		return false, ctx.Err()
	}
	if _, err := s.leaseStore.StopKeptRunIf(ctx, id, to, lostAt, reason, endsAt); err != nil {
		return false, err
	}
	return false, fmt.Errorf("%w: injected: the connection closed before the commit's reply", store.ErrRunWriteUnsettled)
}

func (s unsettledStopStore) SettleRunState(context.Context, uuid.UUID) (types.RunState, error) {
	return s.State(), nil
}

// TestStopKeptRun_TheStopIsBoundedAndSettled: the kept run's stop is a held state write. One whose COMMIT was not
// answered and that committed is settled as applied, so the run's tail runs (its one sandbox stop); one that never
// answers ends at heldWriteBound with the run's lock free and no tail. Catches: the stop on the caller's context (a
// dispatch's has no deadline), and an unsettled stop answered as failed, which skips the tail of a run it ended.
func TestStopKeptRun_TheStopIsBoundedAndSettled(t *testing.T) {
	setup := func(t *testing.T, hang bool) (*leaseFixture, *holdSeen, unsettledStopStore) {
		t.Helper()
		f := newLeaseFixture(t, -time.Minute)
		seen := &holdSeen{}
		f.srv.cfg.Runner = &holdSeeingLeaseRunner{leaseRunner: f.rn, seen: seen}
		us := unsettledStopStore{leaseStore: f.st, hang: hang}
		f.srv.cfg.Store = us
		return f, seen, us
	}
	t.Run("an unsettled stop that committed", func(t *testing.T) {
		f, seen, us := setup(t, false)
		f.srv.stopKeptRun(context.Background(), us, f.run, types.RunStopped, "run.ended", nil)
		if f.st.State() != types.RunStopped || len(seen.calls()) != 1 {
			t.Fatalf("state %s, %d stops; want STOPPED and the tail's one stop", f.st.State(), len(seen.calls()))
		}
	})
	t.Run("a stop that never answers", func(t *testing.T) {
		old := heldWriteBound
		heldWriteBound = 50 * time.Millisecond
		t.Cleanup(func() { heldWriteBound = old })
		f, seen, us := setup(t, true)
		lockFreesWithin(t, f.srv, f.run.ID, "the kept run's stop", func(ctx context.Context) {
			f.srv.stopKeptRun(ctx, us, f.run, types.RunStopped, "run.ended", nil)
		})
		if f.st.State() != types.RunRunning || len(seen.calls()) != 0 {
			t.Fatalf("state %s, %d stops; want RUNNING and no tail", f.st.State(), len(seen.calls()))
		}
	})
}
