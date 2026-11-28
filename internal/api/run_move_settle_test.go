// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// No settle record of a run's pause acts on another sandbox than the one it was written about: a moved run's source
// record never reaches its destination.

// TestPauseSettle_RetryStopsWhenTheSandboxChanges: a settle record that is being written again stops, writing
// nothing, once the run runs on another sandbox than the one its loop started for, as a moved run does.
func TestPauseSettle_RetryStopsWhenTheSandboxChanges(t *testing.T) {
	oldCall, oldEvery := pauseRuntimeCallTimeout, pauseSettleRetryEvery
	pauseRuntimeCallTimeout, pauseSettleRetryEvery = 50*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { pauseRuntimeCallTimeout, pauseSettleRetryEvery = oldCall, oldEvery })

	f, _, _ := pausedRunClock(t, 0)
	notes := failNotes(f, 1<<30)
	f.srv.cfg.Runner = &hungRuntime{pauseRunner: f.rn}
	_ = f.srv.markPresent(context.Background(), f.run.ID, types.ActorHuman, pauseOwner, "presence")
	key := pauseSettleRetryKey{f.srv, f.run.ID}
	if _, busy := pauseSettleRetries.Load(key); !busy {
		t.Fatal("a settle record that could not be written is not written again")
	}
	f.st.mu.Lock()
	f.st.run.SandboxRef = "ref-destination"
	f.st.mu.Unlock()
	waitFor(t, "the retries to stop", func() bool { _, busy := pauseSettleRetries.Load(key); return !busy })
	written := notes.fails.Load()
	time.Sleep(3 * pauseSettleRetryEvery)
	if notes.fails.Load() != written {
		t.Error("the loop wrote again after the run moved to another sandbox")
	}
	if n := f.st.owing(); n != 0 {
		t.Fatalf("%d settle records for a run on another sandbox, want 0", n)
	}
}

// goneSettleStore lists one due, last settle record for the run whose record has since been deleted.
type goneSettleStore struct {
	*pauseStore
	cleared atomic.Int32
}

func (s *goneSettleStore) DuePauseSettles(context.Context, time.Duration) ([]store.PauseSettle, error) {
	return []store.PauseSettle{{RunID: s.run.ID, DueAt: time.Now().Add(-time.Hour), Last: true}}, nil
}

func (s *goneSettleStore) PauseSettleFor(context.Context, uuid.UUID) (time.Time, error) {
	return time.Time{}, nil
}

func (s *goneSettleStore) ClearPauseSettle(context.Context, uuid.UUID, time.Time) error {
	s.cleared.Add(1)
	return nil
}

// TestPauseSettle_ListedRecordGoneIsNotSettled: a record the sweep listed but that is gone under the run's lock (a
// move's commit deleted it) settles nothing: no freeze, no thaw, no clear.
func TestPauseSettle_ListedRecordGoneIsNotSettled(t *testing.T) {
	f, _, _ := pausedRunClock(t, 0)
	gs := &goneSettleStore{pauseStore: f.st}
	f.srv.cfg.Store = gs
	f.st.mu.Lock()
	f.st.run.PausedAt = nil // the row now says running: a settle would thaw the sandbox
	f.st.mu.Unlock()
	f.srv.settlePauses(context.Background(), f.rn)
	if freezes, thaws := f.rn.counts(); freezes != 0 || thaws != 0 || gs.cleared.Load() != 0 {
		t.Errorf("freezes %d thaws %d clears %d, want nothing settled for a record that is gone", freezes, thaws, gs.cleared.Load())
	}
}
