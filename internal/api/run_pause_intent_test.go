// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// diesAfterFreeze runs one pause sweep on a server that dies once its freeze has landed: nothing after the freeze
// runs (runtime.Goexit ends the sweep's goroutine as the process's end would, and the run's lock goes with it). The
// run's request then closes, and the server that comes back has nothing of the dead one in memory.
func diesAfterFreeze(t *testing.T) (*pauseFixture, *Server) {
	t.Helper()
	f := newPauseFixture(t, pauseWaitingAfter+time.Minute)
	f.st.open, f.st.waiting = true, true
	f.rn.onFreeze = runtime.Goexit
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = f.srv.sweepRunPauses(context.Background())
	}()
	<-done
	f.rn.onFreeze = nil
	if freezes, _ := f.rn.counts(); freezes != 1 {
		t.Fatalf("freezes = %d, want the one freeze that landed before the server died", freezes)
	}
	if at, _ := f.st.paused(); at != nil {
		t.Fatal("the run was marked paused, but the server died before its mark")
	}
	f.st.mu.Lock()
	f.st.open, f.st.waiting = false, false
	f.st.mu.Unlock()
	restarted := New(f.srv.cfg)
	restarted.approvalClosed(context.Background(), f.run.ID)
	return f, restarted
}

// TestRunPause_AFreezeThatNeverReachedItsMarkIsThawed: a server that dies between a pause's freeze and its mark leaves
// a frozen agent under a row that is not paused. Nothing that reads the row finds it: the request's close resumes
// nothing, and Resume answered 200 {"paused": false} over the frozen sandbox. The pause records its intent before the
// freeze, so the restarted server's Resume, or its pause sweep once the record is due, thaws the sandbox to its row.
// Resume keeps the record: a freeze still in flight can land after its thaw, and the sweep's settle is what follows it.
func TestRunPause_AFreezeThatNeverReachedItsMarkIsThawed(t *testing.T) {
	t.Run("Resume", func(t *testing.T) {
		f, srv := diesAfterFreeze(t)
		owner := ssoSession(t, pauseOwner, "owner@corp.example", oidc.RoleUser)
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/resume", owner, "")
		_, thaws := f.rn.counts()
		if w.Code != http.StatusOK || thaws != 1 || f.st.owing() != 1 {
			t.Fatalf("Resume = %d %s, thaws %d, settle records %d; want the frozen sandbox thawed before the answer and its record kept for the sweep",
				w.Code, w.Body.String(), thaws, f.st.owing())
		}
		f.st.settleDue()
		if err := srv.sweepRunPauses(context.Background()); err != nil {
			t.Fatalf("sweepRunPauses: %v", err)
		}
		if at, _ := f.st.paused(); at != nil || f.st.owing() != 0 {
			t.Fatalf("paused %v, settle records %d; want the sweep to leave the resumed run running and clear its record", at != nil, f.st.owing())
		}
	})
	t.Run("the pause sweep", func(t *testing.T) {
		f, srv := diesAfterFreeze(t)
		f.st.settleDue()
		if err := srv.sweepRunPauses(context.Background()); err != nil {
			t.Fatalf("sweepRunPauses: %v", err)
		}
		if _, thaws := f.rn.counts(); thaws != 1 || f.st.owing() != 0 {
			t.Fatalf("thaws %d, settle records %d; want the sweep to thaw the unmarked freeze and clear its record", thaws, f.st.owing())
		}
	})
	t.Run("a Resume whose thaw fails keeps the record", func(t *testing.T) {
		old := pauseRuntimeCallTimeout
		pauseRuntimeCallTimeout = 50 * time.Millisecond
		t.Cleanup(func() { pauseRuntimeCallTimeout = old })
		f, srv := diesAfterFreeze(t)
		srv.cfg.Runner = &hungRuntime{pauseRunner: f.rn}
		owner := ssoSession(t, pauseOwner, "owner@corp.example", oidc.RoleUser)
		if w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/"+f.run.ID.String()+"/resume", owner, ""); w.Code != http.StatusBadGateway || f.st.owing() != 1 {
			t.Fatalf("Resume = %d %s, settle records %d; want 502 with the record kept for the sweep", w.Code, w.Body.String(), f.st.owing())
		}
	})
}

// intentFailingStore is pauseStore whose pause intent cannot be written.
type intentFailingStore struct{ *pauseStore }

func (intentFailingStore) NotePauseIntent(context.Context, uuid.UUID, time.Duration) (time.Time, bool, error) {
	return time.Time{}, false, errors.New("database unavailable")
}

// TestRunPause_NoIntentNoFreeze: a pause whose intent cannot be recorded freezes nothing, so no freeze can outlive a
// server that dies before its mark with nothing to say it happened.
func TestRunPause_NoIntentNoFreeze(t *testing.T) {
	f := newPauseFixture(t, pauseWaitingAfter+time.Minute)
	f.st.open, f.st.waiting = true, true
	f.srv.cfg.Store = intentFailingStore{f.st}
	f.sweep(t)
	freezes, _ := f.rn.counts()
	if at, _ := f.st.paused(); freezes != 0 || at != nil {
		t.Fatalf("freezes %d, paused %v; want nothing frozen without its intent recorded", freezes, at != nil)
	}
}

// TestRunPause_AnIntentIsClearedOnlyByThePauseThatWroteIt: a pause whose mark lands deletes the record it wrote. A
// freeze that fails leaves its record to the sweep, since the error can hide a freeze that landed (a reply lost on
// its way back). A record already held when the pause began is another settle's and is left as it was.
func TestRunPause_AnIntentIsClearedOnlyByThePauseThatWroteIt(t *testing.T) {
	waiting := func(t *testing.T) *pauseFixture {
		f := newPauseFixture(t, pauseWaitingAfter+time.Minute)
		f.st.open, f.st.waiting = true, true
		return f
	}
	t.Run("a pause that lands its mark", func(t *testing.T) {
		f := waiting(t)
		f.sweep(t)
		if at, _ := f.st.paused(); at == nil || f.st.owing() != 0 {
			t.Fatalf("paused %v, settle records %d; want the run paused and no record left", at != nil, f.st.owing())
		}
	})
	t.Run("a freeze the runtime refuses is left to the sweep", func(t *testing.T) {
		f := waiting(t)
		rn := &frozenRunner{pauseRunner: f.rn}
		rn.failFreezes.Store(1)
		f.srv.cfg.Runner = rn
		f.sweep(t)
		if at, _ := f.st.paused(); at != nil || f.st.owing() != 1 {
			t.Fatalf("paused %v, settle records %d; want the run left unmarked and its record kept", at != nil, f.st.owing())
		}
		f.st.mu.Lock()
		f.st.open, f.st.waiting = false, false
		f.st.mu.Unlock()
		f.st.settleDue()
		f.sweep(t)
		if _, thaws := rn.counts(); thaws != 1 || f.st.owing() != 0 {
			t.Fatalf("thaws %d, settle records %d; want the sweep to thaw the sandbox to its row once due and clear the record", thaws, f.st.owing())
		}
	})
	t.Run("a record held before the pause", func(t *testing.T) {
		f := waiting(t)
		due, err := f.st.NotePauseSettle(context.Background(), f.run.ID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		f.sweep(t)
		if at, _ := f.st.paused(); at == nil {
			t.Fatal("the pause did not mark the run")
		}
		if got, _ := f.st.PauseSettleFor(context.Background(), f.run.ID); !got.Equal(due) {
			t.Fatalf("the record held before the pause reads due %v, want %v kept", got, due)
		}
	})
}

// TestRunPause_AnIntentComesDueAfterTheFreezeAndTheMark: the record a pause writes before its freeze is not due until
// the freeze's bound and the mark's have passed, so the sweep does not settle it while that pause may still be
// freezing or marking.
func TestRunPause_AnIntentComesDueAfterTheFreezeAndTheMark(t *testing.T) {
	began := time.Now()
	f, srv := diesAfterFreeze(t)
	due, err := f.st.PauseSettleFor(context.Background(), f.run.ID)
	if want := began.Add(pauseRuntimeCallTimeout + heldWriteBound); err != nil || due.Before(want) {
		t.Fatalf("the intent is due %v (%v); want no sooner than %v", due, err, want)
	}
	if err := srv.sweepRunPauses(context.Background()); err != nil {
		t.Fatalf("sweepRunPauses: %v", err)
	}
	if _, thaws := f.rn.counts(); thaws != 0 || f.st.owing() != 1 {
		t.Fatalf("thaws %d, settle records %d; want an intent that is not yet due left alone by the sweep", thaws, f.st.owing())
	}
}

// hungIntentStore is pauseStore whose pause intent write and settle record read never answer: each waits for its
// context to end.
type hungIntentStore struct{ *pauseStore }

func (hungIntentStore) NotePauseIntent(ctx context.Context, _ uuid.UUID, _ time.Duration) (time.Time, bool, error) {
	<-ctx.Done()
	return time.Time{}, false, ctx.Err()
}

func (hungIntentStore) PauseSettleFor(ctx context.Context, _ uuid.UUID) (time.Time, error) {
	<-ctx.Done()
	return time.Time{}, ctx.Err()
}

// TestRunPause_AHungIntentCallFreesTheRunLock: the pause writes its intent, and a person's thaw reads the run's settle
// record, under the run's lock on a context with no deadline. A call that never answers ends at heldStoreCallBound:
// the pause freezes nothing, the thaw answers an error rather than a run that is not paused, and the run's lock is
// free again.
func TestRunPause_AHungIntentCallFreesTheRunLock(t *testing.T) {
	old := heldStoreCallBound
	heldStoreCallBound = 50 * time.Millisecond
	t.Cleanup(func() { heldStoreCallBound = old })
	t.Run("the pause's intent", func(t *testing.T) {
		f := newPauseFixture(t, pauseWaitingAfter+time.Minute)
		f.st.open, f.st.waiting = true, true
		f.srv.cfg.Store = hungIntentStore{f.st}
		lockFreesWithin(t, f.srv, f.run.ID, "the pause's intent write", func(ctx context.Context) { _ = f.srv.sweepRunPauses(ctx) })
		if freezes, _ := f.rn.counts(); freezes != 0 {
			t.Fatalf("freezes %d; want nothing frozen without its intent recorded", freezes)
		}
	})
	t.Run("a thaw's read of the settle record", func(t *testing.T) {
		f := newPauseFixture(t, time.Minute)
		f.srv.cfg.Store = hungIntentStore{f.st}
		var err error
		lockFreesWithin(t, f.srv, f.run.ID, "a thaw's settle record read", func(ctx context.Context) {
			err = f.srv.thawForExec(ctx, f.run, types.ActorHuman, pauseOwner, "presence")
		})
		if err == nil {
			t.Fatal("a settle record read that never answered was taken as no record held")
		}
	})
}
