// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// hungRuntime is pauseRunner whose freeze and thaw never answer: each waits for its context to end.
type hungRuntime struct {
	*pauseRunner
	calls atomic.Int32
}

func (r *hungRuntime) FreezeSandbox(ctx context.Context, _ string) error { return r.hang(ctx) }
func (r *hungRuntime) ThawSandbox(ctx context.Context, _ string) error   { return r.hang(ctx) }

func (r *hungRuntime) hang(ctx context.Context) error {
	r.calls.Add(1)
	<-ctx.Done()
	return ctx.Err()
}

// statusHungRuntime is pauseRunner whose thaw answers at once but whose status read never answers: it waits for
// its context to end.
type statusHungRuntime struct {
	*pauseRunner
	statusCalls atomic.Int32
}

func (r *statusHungRuntime) Status(ctx context.Context, _ string) (runner.Status, error) {
	r.statusCalls.Add(1)
	<-ctx.Done()
	return runner.Status{}, ctx.Err()
}

// TestRunPause_AHungRuntimeCallEndsAtItsBound: a pause's freeze, a resume's thaw and a settle's move each run
// under the run's lock, on the sweep's context or a person's, and neither has a deadline. A runtime call that
// never answers must end at pauseRuntimeCallTimeout: the caller returns and the run's lock is free again. A
// freeze ended that way may still land, so it leaves a settle record.
func TestRunPause_AHungRuntimeCallEndsAtItsBound(t *testing.T) {
	old := pauseRuntimeCallTimeout
	pauseRuntimeCallTimeout = 50 * time.Millisecond
	t.Cleanup(func() { pauseRuntimeCallTimeout = old })

	// returns runs fn on a context with no deadline, and fails unless fn comes back, having reached the
	// runtime, with the run's lock free.
	returns := func(t *testing.T, f *pauseFixture, what string, fn func(context.Context)) {
		t.Helper()
		rn := &hungRuntime{pauseRunner: f.rn}
		f.srv.cfg.Runner = rn
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel) // ends a call the bound did not, so a red run leaks nothing
		done := make(chan struct{})
		go func() { defer close(done); fn(ctx) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatalf("%s held the run's lock past its bound: its runtime call never ended", what)
		}
		if rn.calls.Load() == 0 {
			t.Fatalf("%s never reached the runtime", what)
		}
		_, unlock, ok := f.srv.tryLockRunOp(context.Background(), f.run.ID)
		if !ok {
			t.Fatalf("%s left the run's lock held", what)
		}
		unlock()
	}

	t.Run("the sweep's freeze", func(t *testing.T) {
		f := newPauseFixture(t, pauseWaitingAfter+time.Minute)
		f.st.open, f.st.waiting = true, true
		returns(t, f, "the pause", func(ctx context.Context) { _ = f.srv.sweepRunPauses(ctx) })
		if at, _ := f.st.paused(); at != nil {
			t.Fatal("a freeze that never answered marked the run paused")
		}
		if n := f.st.owing(); n != 1 {
			t.Fatalf("a freeze ended by its bound may still land: %d settle records, want 1", n)
		}
	})
	t.Run("a person's thaw", func(t *testing.T) {
		f, _, _ := pausedRunClock(t, 0)
		var err error
		returns(t, f, "the resume", func(ctx context.Context) {
			err = f.srv.markPresent(ctx, f.run.ID, types.ActorHuman, pauseOwner, "presence")
		})
		if err == nil {
			t.Fatal("a thaw that never answered was reported as a resume")
		}
		if at, _ := f.st.paused(); at == nil {
			t.Fatal("a thaw that never answered cleared paused_at")
		}
	})
	t.Run("a thaw cut at its bound is left to the settle", func(t *testing.T) {
		f, _, now := pausedRunClock(t, 0)
		returns(t, f, "the resume", func(ctx context.Context) {
			_ = f.srv.markPresent(ctx, f.run.ID, types.ActorHuman, pauseOwner, "presence")
		})
		if n := f.st.owing(); n != 1 {
			t.Fatalf("a thaw ended by its bound may still land: %d settle records, want 1", n)
		}
		// The runtime answers again and the row still reads paused: the settle freezes the sandbox back to it.
		f.srv.cfg.Runner = f.rn
		f.st.settleDue()
		f.sweep(t)
		freezes, _ := f.rn.counts()
		if at, _ := f.st.paused(); at == nil || freezes != 1 || f.st.owing() != 0 {
			t.Fatalf("paused = %v, freezes = %d, settle records = %d; want the paused run frozen again and its record cleared",
				at != nil, freezes, f.st.owing())
		}
		// A second person's input, once the retry backoff has passed and with the runtime answering, resumes the run.
		*now = now.Add(time.Second)
		_, thawsBefore := f.rn.counts()
		if err := f.typed(); err != nil {
			t.Fatalf("the second input's resume: %v; want it to succeed", err)
		}
		_, thaws := f.rn.counts()
		if at, _ := f.st.paused(); at != nil || thaws != thawsBefore+1 {
			t.Fatalf("paused = %v, thaws = %d (was %d); want the run resumed by one thaw", at != nil, thaws, thawsBefore)
		}
	})
	t.Run("a thaw that lands while its status read is cut is left to the settle", func(t *testing.T) {
		f, _, now := pausedRunClock(t, 0)
		rn := &statusHungRuntime{pauseRunner: f.rn}
		f.srv.cfg.Runner = rn
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel) // ends a call the bound did not, so a red run leaks nothing
		done := make(chan error, 1)
		go func() { done <- f.srv.markPresent(ctx, f.run.ID, types.ActorHuman, pauseOwner, "presence") }()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a resume whose status read never answered was reported as a resume")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("the resume held the run's lock past its bound: its status read never ended")
		}
		if _, thaws := f.rn.counts(); thaws != 1 || rn.statusCalls.Load() == 0 {
			t.Fatalf("thaws = %d, status reads = %d; want the thaw answered and the status read reached", thaws, rn.statusCalls.Load())
		}
		_, unlock, ok := f.srv.tryLockRunOp(context.Background(), f.run.ID)
		if !ok {
			t.Fatal("the resume left the run's lock held")
		}
		unlock()
		// The thaw landed, so the agent runs, but the row still reads paused: only a settle record repairs that.
		if at, _ := f.st.paused(); at == nil || f.st.owing() != 1 {
			t.Fatalf("paused = %v, settle records = %d; want the row still paused and one settle record", at != nil, f.st.owing())
		}
		f.srv.cfg.Runner = f.rn
		f.st.settleDue()
		f.sweep(t)
		freezes, _ := f.rn.counts()
		if at, _ := f.st.paused(); at == nil || freezes != 1 || f.st.owing() != 0 {
			t.Fatalf("paused = %v, freezes = %d, settle records = %d; want the running agent frozen back to its paused row and the record cleared",
				at != nil, freezes, f.st.owing())
		}
		// A later input, once the retry backoff has passed and with the runtime answering, resumes the run.
		*now = now.Add(time.Second)
		_, thawsBefore := f.rn.counts()
		if err := f.typed(); err != nil {
			t.Fatalf("the later input's resume: %v; want it to succeed", err)
		}
		_, thaws := f.rn.counts()
		if at, _ := f.st.paused(); at != nil || thaws != thawsBefore+1 {
			t.Fatalf("paused = %v, thaws = %d (was %d); want the run resumed by one thaw", at != nil, thaws, thawsBefore)
		}
	})
	t.Run("the settle's move", func(t *testing.T) {
		f := newPauseFixture(t, time.Second)
		if _, err := f.st.NotePauseSettle(context.Background(), f.run.ID, 0); err != nil {
			t.Fatal(err)
		}
		f.st.settleDue()
		returns(t, f, "the settle", func(ctx context.Context) { _ = f.srv.sweepRunPauses(ctx) })
		if n := f.st.owing(); n != 1 {
			t.Fatalf("a settle whose move never answered cleared its record: %d left, want 1", n)
		}
	})
}
