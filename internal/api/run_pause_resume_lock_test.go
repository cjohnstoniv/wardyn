// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sandboxRunner is the pause fixture's runner with the one thing it does not model: whether the sandbox is
// frozen. afterThaw runs once, right after a thaw, before the thawing path has cleared its row.
type sandboxRunner struct {
	*pauseRunner
	mu        sync.Mutex
	frozen    bool
	afterThaw func()
}

func (r *sandboxRunner) FreezeSandbox(ctx context.Context, ref string) error {
	r.mu.Lock()
	r.frozen = true
	r.mu.Unlock()
	return r.pauseRunner.FreezeSandbox(ctx, ref)
}

func (r *sandboxRunner) ThawSandbox(ctx context.Context, ref string) error {
	err := r.pauseRunner.ThawSandbox(ctx, ref)
	r.mu.Lock()
	r.frozen = false
	hook := r.afterThaw
	r.afterThaw = nil
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (r *sandboxRunner) isFrozen() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.frozen
}

func newSandboxFixture(t *testing.T, reason types.PauseReason) (*pauseFixture, *sandboxRunner) {
	t.Helper()
	f := newPauseFixture(t, time.Hour)
	sr := &sandboxRunner{pauseRunner: f.rn, frozen: true}
	f.srv.cfg.Runner = sr
	paused := time.Now().UTC()
	f.st.run.PausedAt, f.st.run.PausedReason = &paused, reason
	return f, sr
}

// TestApprovalClosed_AStaleLeadersFreezeCannotLeaveTheSandboxFrozenUnderARunningRow is the interleaving of
// #1819. approvalClosed thaws the sandbox, and before it clears the pause a stale leader's pauseRun freezes
// it again, loses the mark (the run still reads paused), and its compensation, reading that same paused row,
// leaves the freeze alone. The clear then lands: a frozen sandbox under a row that says running, which
// nothing thaws. The run's lock keeps the stale pause out of the window.
func TestApprovalClosed_AStaleLeadersFreezeCannotLeaveTheSandboxFrozenUnderARunningRow(t *testing.T) {
	f, sr := newSandboxFixture(t, types.PauseWaiting)
	f.st.waiting = true
	sr.afterThaw = func() {
		f.srv.pauseRun(context.Background(), f.st, f.run, types.PauseWaiting, time.Hour)
	}

	f.srv.approvalClosed(context.Background(), f.run.ID)

	pausedAt, _ := f.st.paused()
	if pausedAt != nil && !sr.isFrozen() {
		t.Fatal("the row says paused and the sandbox is running: nothing was resumed")
	}
	if pausedAt == nil && sr.isFrozen() {
		t.Fatal("the sandbox is frozen under a row that says running: a stale pause landed inside the resume")
	}
	if pausedAt != nil || sr.isFrozen() {
		t.Fatalf("after the request closed: paused = %v, frozen = %v; want a running run", pausedAt, sr.isFrozen())
	}
}

// TestApprovalClosed_WaitsOutAPauseHoldingTheRun: a pause is mid-compensation (it holds the run's lock) when
// the request closes. The resume waits for it and then thaws, so it cannot interleave with the pause's
// freeze, mark or thaw.
func TestApprovalClosed_WaitsOutAPauseHoldingTheRun(t *testing.T) {
	f, sr := newSandboxFixture(t, types.PauseWaiting)
	_, unlock, err := f.srv.lockRunOp(context.Background(), f.run.ID)
	if err != nil {
		t.Fatalf("lockRunOp: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		f.srv.approvalClosed(context.Background(), f.run.ID)
	}()
	select {
	case <-done:
		t.Fatal("approvalClosed resumed a run whose lock another operation holds")
	case <-time.After(200 * time.Millisecond):
	}
	if _, thaws := f.rn.counts(); thaws != 0 {
		t.Fatalf("thaws = %d while the run's lock was held, want 0", thaws)
	}

	unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("approvalClosed did not resume the run once its lock was released")
	}
	if pausedAt, _ := f.st.paused(); pausedAt != nil || sr.isFrozen() {
		t.Errorf("after the lock was released: paused = %v, frozen = %v; want a running run", pausedAt, sr.isFrozen())
	}
}

// TestSweepRequestClosedResume_SkipsARunWhoseLockIsHeld: the sweep's backstop resume tries the run's lock and
// leaves a busy run to the next pass; with the lock free it thaws.
func TestSweepRequestClosedResume_SkipsARunWhoseLockIsHeld(t *testing.T) {
	f, sr := newSandboxFixture(t, types.PauseWaiting)
	f.st.open, f.st.waiting = false, false
	_, unlock, err := f.srv.lockRunOp(context.Background(), f.run.ID)
	if err != nil {
		t.Fatalf("lockRunOp: %v", err)
	}

	f.sweep(t)
	if _, thaws := f.rn.counts(); thaws != 0 {
		t.Fatalf("thaws = %d with the run's lock held, want 0", thaws)
	}
	if pausedAt, _ := f.st.paused(); pausedAt == nil {
		t.Fatal("the sweep cleared a pause on a run whose lock another operation holds")
	}

	unlock()
	f.sweep(t)
	if _, thaws := f.rn.counts(); thaws != 1 {
		t.Fatalf("thaws = %d on the next pass, want 1", thaws)
	}
	if pausedAt, _ := f.st.paused(); pausedAt != nil || sr.isFrozen() {
		t.Errorf("after the next pass: paused = %v, frozen = %v; want a running run", pausedAt, sr.isFrozen())
	}
}

// TestSweepRequestClosedResume_LeavesANewerIdlePauseAlone: the candidate was listed as a waiting pause, but by
// the time the sweep holds the run's lock the row is a different pause. The resume decides on the row it
// reads under the lock.
func TestSweepRequestClosedResume_LeavesANewerIdlePauseAlone(t *testing.T) {
	f, _ := newSandboxFixture(t, types.PauseIdle)
	f.srv.resumeWaitingRun(context.Background(), f.st, f.run.ID)
	if _, thaws := f.rn.counts(); thaws != 0 {
		t.Fatalf("thaws = %d for an idle pause, want 0", thaws)
	}
}

// TestMarkPresent_SkipsAPausedRunWhoseLockIsHeldAndRetries: a keystroke into a paused run whose lock another
// operation holds resumes nothing, reports nothing, and is not swallowed by the stamp debounce: the next
// input, once the short backoff has passed, thaws it.
func TestMarkPresent_SkipsAPausedRunWhoseLockIsHeldAndRetries(t *testing.T) {
	f, sr := newSandboxFixture(t, types.PauseIdle)
	now := time.Now().UTC()
	f.srv.cfg.Now = func() time.Time { return now }
	_, unlock, err := f.srv.lockRunOp(context.Background(), f.run.ID)
	if err != nil {
		t.Fatalf("lockRunOp: %v", err)
	}

	if err := f.srv.markPresent(context.Background(), f.run.ID, types.ActorHuman, "alice", "presence"); err != nil {
		t.Fatalf("markPresent with the run's lock held: %v, want it skipped without an error", err)
	}
	if _, thaws := f.rn.counts(); thaws != 0 {
		t.Fatalf("thaws = %d with the run's lock held, want 0", thaws)
	}

	unlock()
	now = now.Add(2 * thawRetryFloor)
	if err := f.srv.markPresent(context.Background(), f.run.ID, types.ActorHuman, "alice", "presence"); err != nil {
		t.Fatalf("markPresent after the lock was released: %v", err)
	}
	if pausedAt, _ := f.st.paused(); pausedAt != nil || sr.isFrozen() {
		t.Errorf("the retry left paused = %v, frozen = %v; want a running run", pausedAt, sr.isFrozen())
	}
}

// TestResolvePendingReauth_HandsBackTheRunsItClosedAndResumesNone: the AWS capture resolves its requests while
// it holds the owner lock, which the run's lock must precede, so it resumes nothing itself and returns the
// run ids for its caller to resume after the lock is released.
func TestResolvePendingReauth_HandsBackTheRunsItClosedAndResumesNone(t *testing.T) {
	f := newReauthFixture(t, nil)
	f.putBlob(t, "alice@example.com", deadSSOBlob())
	if w := f.resolve(t); w.Code != 423 {
		t.Fatalf("first resolve: code = %d, want 423", w.Code)
	}
	loginRun := types.AgentRun{ID: uuid.New(), CreatedBy: "alice@example.com", CreatedAt: time.Now().Add(time.Second)}
	f.st.loginRun = loginRun
	f.putBlob(t, "alice@example.com", liveSSOBlob())

	ctx, unlock, err := f.srv.lockAWSSSOOwner(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("lockAWSSSOOwner: %v", err)
	}
	defer unlock()
	closed := f.srv.resolvePendingReauth(ctx, reauthScope("alice@example.com"), "alice@example.com", loginRun)
	if len(closed) != 1 || closed[0] != f.runID {
		t.Fatalf("resolvePendingReauth returned %v, want the held run %s", closed, f.runID)
	}
}
