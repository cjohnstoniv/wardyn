// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// flakyThaw is a pauseRunner whose thaw fails the first fail times, or every
// time when fail is negative.
type flakyThaw struct {
	*pauseRunner
	fail  int
	tries int
}

func (r *flakyThaw) ThawSandbox(ctx context.Context, ref string) error {
	r.tries++
	if r.fail < 0 || r.tries <= r.fail {
		return errors.New("synthetic thaw failure")
	}
	return r.pauseRunner.ThawSandbox(ctx, ref)
}

// flakyGetStore fails GetRun the first fail times.
type flakyGetStore struct {
	*pauseStore
	fail int
}

func (s *flakyGetStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if s.fail > 0 {
		s.fail--
		return types.AgentRun{}, errors.New("synthetic read failure")
	}
	return s.pauseStore.GetRun(ctx, id)
}

// pausedRunClock is a paused run on a runner whose thaw is flaky, with a clock
// the test moves.
func pausedRunClock(t *testing.T, fail int) (*pauseFixture, *flakyThaw, *time.Time) {
	t.Helper()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	f := newPauseFixture(t, time.Hour, func(c *Config) { c.Now = func() time.Time { return now } })
	paused := now.Add(-time.Minute)
	f.st.run.PausedAt, f.st.run.PausedReason = &paused, types.PauseIdle
	rn := &flakyThaw{pauseRunner: f.rn, fail: fail}
	f.srv.cfg.Runner = rn
	return f, rn, &now
}

func (f *pauseFixture) typed() error {
	return f.srv.markPresent(context.Background(), f.run.ID, types.ActorHuman, pauseOwner, "presence")
}

// TestMarkPresent_FailedThawRetriesAtBackoff (#1482): a keystroke whose thaw
// failed is retried by the next one, once the short backoff has passed, instead
// of being swallowed for the whole presence window.
func TestMarkPresent_FailedThawRetriesAtBackoff(t *testing.T) {
	f, rn, now := pausedRunClock(t, 1)
	if err := f.typed(); err == nil {
		t.Fatal("the first input's failed thaw was reported as handled")
	}
	if at, _ := f.st.paused(); at == nil {
		t.Fatal("a failed thaw cleared paused_at")
	}
	*now = now.Add(time.Second)
	if err := f.typed(); err != nil {
		t.Fatalf("the input a second later: %v; want the thaw retried and successful", err)
	}
	if at, _ := f.st.paused(); at != nil || rn.tries != 2 {
		t.Fatalf("paused %v, thaw attempts %d; want the run resumed on the second attempt", at, rn.tries)
	}
}

// TestMarkPresent_PersistentFailureIsBoundedByBackoff: inputs inside the
// backoff cost nothing, and a failure that keeps failing is retried at a
// doubling interval, one run.resume failure row per attempt.
func TestMarkPresent_PersistentFailureIsBoundedByBackoff(t *testing.T) {
	f, rn, now := pausedRunClock(t, -1)
	for range 50 {
		_ = f.typed()
	}
	if rn.tries != 1 || len(f.rows("run.resume", "failure")) != 1 {
		t.Fatalf("50 inputs at one instant: %d attempts, %d failure rows; want 1 and 1", rn.tries, len(f.rows("run.resume", "failure")))
	}
	for _, step := range []struct {
		after time.Duration
		tries int
	}{
		{time.Second, 2},      // the floor
		{time.Second, 2},      // inside the doubled backoff
		{time.Second, 3},      // the doubled backoff (2s) has passed
		{3 * time.Second, 3},  // 4s backoff: not yet
		{time.Second, 4},      // now
		{time.Hour, 5},        // a long gap retries at once
		{16 * time.Second, 6}, // 16s
		{32 * time.Second, 7}, // 32s; the next doubling is capped at the window
		{presenceStampEvery - time.Second, 7},
		{time.Second, 8},
	} {
		*now = now.Add(step.after)
		_ = f.typed()
		if rn.tries != step.tries {
			t.Fatalf("after +%v: %d attempts, want %d", step.after, rn.tries, step.tries)
		}
	}
	if rows := len(f.rows("run.resume", "failure")); rows != rn.tries {
		t.Errorf("%d failure rows for %d attempts; want one per attempt", rows, rn.tries)
	}
	if at, _ := f.st.paused(); at == nil {
		t.Error("a failing thaw cleared paused_at")
	}
}

// TestMarkPresent_AFailedReadOfThePausedRunRetries: the paused-run read failing
// is a failed thaw too, retried at the backoff.
func TestMarkPresent_AFailedReadOfThePausedRunRetries(t *testing.T) {
	f, rn, now := pausedRunClock(t, 0)
	f.srv.cfg.Store = &flakyGetStore{pauseStore: f.st, fail: 1}
	if err := f.typed(); err == nil {
		t.Fatal("a failed read of the paused run was reported as handled")
	}
	*now = now.Add(time.Second)
	if err := f.typed(); err != nil {
		t.Fatalf("the retry: %v", err)
	}
	if at, _ := f.st.paused(); at != nil || rn.tries != 1 {
		t.Fatalf("paused %v, thaws %d; want resumed by one thaw", at, rn.tries)
	}
}

// TestMarkPresent_AHealthyRunStampsOncePerWindow: the write debounce stays for
// a running agent.
func TestMarkPresent_AHealthyRunStampsOncePerWindow(t *testing.T) {
	f, _, now := pausedRunClock(t, 0)
	f.st.run.PausedAt, f.st.run.PausedReason = nil, ""
	for range 50 {
		if err := f.typed(); err != nil {
			t.Fatal(err)
		}
	}
	if f.st.stamps != 1 {
		t.Fatalf("%d presence writes for 50 inputs; want 1", f.st.stamps)
	}
	*now = now.Add(presenceStampEvery)
	_ = f.typed()
	if f.st.stamps != 2 {
		t.Errorf("%d presence writes after a window; want 2", f.st.stamps)
	}
}

// TestMarkPresent_ThawPathsIgnoreTheBackoff: Resume and an exec thaw the run
// even inside a failed thaw's backoff, and a passive agent signal never thaws.
func TestMarkPresent_ThawPathsIgnoreTheBackoff(t *testing.T) {
	f, rn, _ := pausedRunClock(t, 1)
	if err := f.typed(); err == nil {
		t.Fatal("the first thaw should have failed")
	}
	f.srv.noteAgentActive(context.Background(), f.run.ID)
	if rn.tries != 1 {
		t.Fatalf("a passive agent signal thawed the run (%d attempts)", rn.tries)
	}
	run, err := f.st.GetRun(context.Background(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.srv.thawForExec(context.Background(), run, types.ActorHuman, pauseOwner, "resume"); err != nil {
		t.Fatalf("thawForExec inside the backoff: %v", err)
	}
	if at, _ := f.st.paused(); at != nil || rn.tries != 2 {
		t.Fatalf("paused %v, attempts %d; want the exec path to thaw directly", at, rn.tries)
	}
}

// TestMarkPresent_AReadOnlyAttachViewerNeverThaws: an observer's keystrokes are
// dropped at the holder, before any presence is recorded.
func TestMarkPresent_AReadOnlyAttachViewerNeverThaws(t *testing.T) {
	f, rn, _ := pausedRunClock(t, 0)
	h := &attachHolder{principal: pauseOwner, source: attachSourceWeb,
		onInput: func() { _ = f.typed() }}
	if err := h.writeGated(nil, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if rn.tries != 0 {
		t.Errorf("a read-only viewer's input thawed the run (%d attempts)", rn.tries)
	}
}
