// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// endedPausedCase is a paused run that ended, or was kept, after a candidate listing read it RUNNING: a kill or
// stop wins the terminal write, which leaves the pause mark, and a stop that could not remove the sandbox leaves
// its ref.
type endedPausedCase struct {
	name string
	end  func(f *pauseFixture)
}

func endedPausedCases() []endedPausedCase {
	return []endedPausedCase{
		{"killed", func(f *pauseFixture) { f.st.endWith(types.RunKilled) }},
		{"stopped", func(f *pauseFixture) { f.st.endWith(types.RunStopped) }},
		{"kept", func(f *pauseFixture) {
			lost := time.Now().UTC()
			f.st.mu.Lock()
			f.st.run.LostAt = &lost
			f.st.mu.Unlock()
		}},
	}
}

func (s *pauseStore) endWith(state types.RunState) {
	s.mu.Lock()
	s.state = state
	s.mu.Unlock()
}

// newEndedPausedFixture is a run paused waiting on a request that has since closed, listed as a RUNNING
// candidate, and then ended by c.
func newEndedPausedFixture(t *testing.T, c endedPausedCase) (*pauseFixture, store.PauseCandidate) {
	t.Helper()
	f, _ := newSandboxFixture(t, types.PauseWaiting)
	f.st.open, f.st.waiting = false, false
	listed, _ := f.st.GetRun(context.Background(), f.run.ID)
	c.end(f)
	return f, store.PauseCandidate{Run: listed}
}

func assertNothingResumed(t *testing.T, f *pauseFixture) {
	t.Helper()
	if _, thaws := f.rn.counts(); thaws != 0 {
		t.Errorf("thaws = %d for a run that is no longer live, want 0", thaws)
	}
	if pausedAt, _ := f.st.paused(); pausedAt == nil {
		t.Error("the pause was cleared on a run that is no longer live")
	}
	if rows := f.rows("run.resume", "success"); len(rows) != 0 {
		t.Errorf("run.resume success rows = %d for a run that is no longer live, want 0", len(rows))
	}
}

func TestResumeClosedRun_LeavesAnEndedOrKeptRunFrozen(t *testing.T) {
	for _, c := range endedPausedCases() {
		t.Run(c.name, func(t *testing.T) {
			f, cand := newEndedPausedFixture(t, c)
			f.srv.resumeClosedRun(context.Background(), f.st, cand)
			assertNothingResumed(t, f)
		})
	}
}

func TestApprovalClosed_LeavesAnEndedOrKeptRunFrozen(t *testing.T) {
	for _, c := range endedPausedCases() {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newEndedPausedFixture(t, c)
			f.srv.approvalClosed(context.Background(), f.run.ID)
			assertNothingResumed(t, f)
		})
	}
}

// TestResumeRun_RefusesARunThatIsNotLive pins the seam itself: the refusal is errResumeRunNotLive and comes
// before the owner check, the thaw and the clear.
func TestResumeRun_RefusesARunThatIsNotLive(t *testing.T) {
	for _, c := range endedPausedCases() {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newEndedPausedFixture(t, c)
			run, _ := f.st.GetRun(context.Background(), f.run.ID)
			err := f.srv.resumeRun(context.Background(), f.st, run, types.ActorSystem, "wardynd", "request_closed")
			if !errors.Is(err, errResumeRunNotLive) {
				t.Fatalf("resumeRun = %v, want errResumeRunNotLive", err)
			}
			assertNothingResumed(t, f)
		})
	}
}

// TestResumeClosedRun_ThawsALiveRunWithNoOpenRequest is the other side of the gate: a RUNNING run, not kept,
// paused waiting with its request closed, is thawed and cleared.
func TestResumeClosedRun_ThawsALiveRunWithNoOpenRequest(t *testing.T) {
	f, _ := newSandboxFixture(t, types.PauseWaiting)
	f.st.open, f.st.waiting = false, false
	listed, _ := f.st.GetRun(context.Background(), f.run.ID)

	f.srv.resumeClosedRun(context.Background(), f.st, store.PauseCandidate{Run: listed})

	if _, thaws := f.rn.counts(); thaws != 1 {
		t.Errorf("thaws = %d, want 1", thaws)
	}
	if pausedAt, _ := f.st.paused(); pausedAt != nil {
		t.Error("the pause was not cleared on a live run")
	}
	if rows := f.rows("run.resume", "success"); len(rows) != 1 {
		t.Errorf("run.resume success rows = %d, want 1", len(rows))
	}
}

// TestResumeClosedRun_LeavesARunPastItsLifetimeFrozen: a RUNNING paused run whose end time or lifetime cap has
// passed, and which the lifetime sweep has not reached yet, is not thawed; the sweep ends it.
func TestResumeClosedRun_LeavesARunPastItsLifetimeFrozen(t *testing.T) {
	past := time.Now().UTC().Add(-time.Minute)
	for _, c := range []struct {
		name string
		set  func(r *types.AgentRun)
	}{
		{"ends_at", func(r *types.AgentRun) { r.EndsAt = &past }},
		{"lifetime_cap_at", func(r *types.AgentRun) { r.LifetimeCapAt = &past }},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := newSandboxFixture(t, types.PauseWaiting)
			f.st.open, f.st.waiting = false, false
			f.st.mu.Lock()
			c.set(&f.st.run)
			f.st.mu.Unlock()
			listed, _ := f.st.GetRun(context.Background(), f.run.ID)

			f.srv.resumeClosedRun(context.Background(), f.st, store.PauseCandidate{Run: listed})
			assertNothingResumed(t, f)
		})
	}
}

// TestResumeClosedRun_ThawsARunInsideItsLifetime: an end time and a cap still ahead do not stop the resume.
func TestResumeClosedRun_ThawsARunInsideItsLifetime(t *testing.T) {
	ahead := time.Now().UTC().Add(time.Hour)
	f, _ := newSandboxFixture(t, types.PauseWaiting)
	f.st.open, f.st.waiting = false, false
	f.st.mu.Lock()
	f.st.run.EndsAt, f.st.run.LifetimeCapAt = &ahead, &ahead
	f.st.mu.Unlock()
	listed, _ := f.st.GetRun(context.Background(), f.run.ID)

	f.srv.resumeClosedRun(context.Background(), f.st, store.PauseCandidate{Run: listed})

	if _, thaws := f.rn.counts(); thaws != 1 {
		t.Errorf("thaws = %d, want 1", thaws)
	}
}
