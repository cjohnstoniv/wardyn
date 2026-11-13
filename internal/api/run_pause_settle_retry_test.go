// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// noteFailingStore is pauseStore whose settle record write fails while fails is above zero, counting down, as a
// database that is down for a while does.
type noteFailingStore struct {
	*pauseStore
	fails atomic.Int32
}

func (s *noteFailingStore) NotePauseSettle(ctx context.Context, id uuid.UUID, after time.Duration) (time.Time, error) {
	if s.fails.Add(-1) >= 0 {
		return time.Time{}, errors.New("database unavailable")
	}
	return s.pauseStore.NotePauseSettle(ctx, id, after)
}

// failNotes puts f's store behind a noteFailingStore whose first n settle record writes fail.
func failNotes(f *pauseFixture, n int32) *noteFailingStore {
	ns := &noteFailingStore{pauseStore: f.st}
	ns.fails.Store(n)
	f.srv.cfg.Store = ns
	return ns
}

// TestPauseSettle_AFailedRecordIsWrittenAgain: a freeze or a thaw cut at its bound may still land. A record whose first
// write fails (every inline try) must still land, written again in the background, so the pause sweep settles the run
// once the database answers; a run that is no longer running stops the retries. A freeze's record is first the one its
// pause wrote before the freeze: a cut freeze still writes it again, and a compensation whose own write fails thaws
// under it.
func TestPauseSettle_AFailedRecordIsWrittenAgain(t *testing.T) {
	oldCall, oldEvery := pauseRuntimeCallTimeout, pauseSettleRetryEvery
	pauseRuntimeCallTimeout, pauseSettleRetryEvery = 50*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { pauseRuntimeCallTimeout, pauseSettleRetryEvery = oldCall, oldEvery })

	t.Run("a freeze cut at its bound", func(t *testing.T) {
		f := newPauseFixture(t, pauseWaitingAfter+time.Minute)
		f.st.open, f.st.waiting = true, true
		notes := failNotes(f, retryTries)
		f.srv.cfg.Runner = &hungRuntime{pauseRunner: f.rn}
		f.sweep(t)
		waitFor(t, "the cut freeze's settle record, written again in the background", func() bool { return notes.fails.Load() < 0 })
		if n := f.st.owing(); n != 1 {
			t.Fatalf("%d settle records, want 1", n)
		}
	})
	t.Run("a thaw cut at its bound", func(t *testing.T) {
		f, _, _ := pausedRunClock(t, 0)
		failNotes(f, retryTries)
		f.srv.cfg.Runner = &hungRuntime{pauseRunner: f.rn}
		if err := f.srv.markPresent(context.Background(), f.run.ID, types.ActorHuman, pauseOwner, "presence"); err == nil {
			t.Fatal("a thaw that never answered was reported as a resume")
		}
		waitFor(t, "the thaw's settle record, written again", func() bool { return f.st.owing() == 1 })
	})
	t.Run("a compensation that could not write its settle record again", func(t *testing.T) {
		f := newPauseFixture(t, time.Hour)
		f.st.run.RunLimits.PauseIdleAfterSec = 60
		f.rn.cpu = pct(0)
		// The freeze opens a request, so the mark fails and the compensation runs. Its own write of the record fails,
		// and the record the pause wrote before its freeze still stands, so it thaws and clears that one.
		f.rn.onFreeze = func() { f.st.open = true }
		failNotes(f, retryTries)
		f.sweep(t)
		if freezes, thaws := f.rn.counts(); freezes != 1 || thaws != 1 || f.st.owing() != 0 {
			t.Fatalf("freezes %d thaws %d, settle records %d; want the unmarked freeze thawed under its pause's record and that record cleared",
				freezes, thaws, f.st.owing())
		}
	})
	t.Run("a run that is no longer running stops the retries", func(t *testing.T) {
		f, _, _ := pausedRunClock(t, 0)
		failNotes(f, 1<<30)
		f.srv.cfg.Runner = &hungRuntime{pauseRunner: f.rn}
		_ = f.srv.markPresent(context.Background(), f.run.ID, types.ActorHuman, pauseOwner, "presence")
		key := pauseSettleRetryKey{f.srv, f.run.ID}
		if _, busy := pauseSettleRetries.Load(key); !busy {
			t.Fatal("a settle record that could not be written is not written again")
		}
		f.st.mu.Lock()
		f.st.state = types.RunCompleted
		f.st.mu.Unlock()
		waitFor(t, "the retries to stop", func() bool { _, busy := pauseSettleRetries.Load(key); return !busy })
		if n := f.st.owing(); n != 0 {
			t.Fatalf("%d settle records for a run that ended, want 0", n)
		}
	})
}
