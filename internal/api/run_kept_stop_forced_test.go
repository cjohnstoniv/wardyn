// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// keptEndLeaser is a RunLeaser whose StopKeptRunIf makes a RUNNING run terminal in its computer folder store,
// counting its calls. Nothing else of the interface is used.
type keptEndLeaser struct {
	store.RunLeaser
	cs    *computerFolderStore
	calls int
}

func (l *keptEndLeaser) StopKeptRunIf(_ context.Context, _ uuid.UUID, to types.RunState, _ *time.Time, _ types.LostReason, _ *time.Time) (bool, error) {
	l.calls++
	l.cs.dispatchTestStore.mu.Lock()
	defer l.cs.dispatchTestStore.mu.Unlock()
	if l.cs.state != types.RunRunning {
		return false, nil
	}
	l.cs.state = to
	return true, nil
}

// keptEndFixture is a run with a computer folder brought back both ways, kept after its end and due to be
// stopped, whose lock the test holds. It answers the release of that hold.
func keptEndFixture(t *testing.T) (*Server, *computerFolderStore, *recRecorder, types.AgentRun, *agentSandbox, *keptEndLeaser, func()) {
	t.Helper()
	s, cs, audit, run, sb, _ := bringBackFixture(t)
	withoutRunner(cs)
	s.cfg.Runner, s.cfg.Store = &passWindowSandbox{agentSandbox: sb, run: run.ID}, passWindowStore{refStore{cs}}
	ended := time.Now().Add(-8 * 24 * time.Hour)
	run.LostAt, run.LostReason = &ended, types.LostEnded
	_, hold, ok := s.tryLockRunOp(context.Background(), run.ID)
	if !ok {
		t.Fatal("could not take the run's lock")
	}
	return s, cs, audit, run, sb, &keptEndLeaser{cs: cs}, hold
}

func expiredRows(audit *recRecorder) []types.AuditEvent {
	var rows []types.AuditEvent
	for _, ev := range audit.snapshot() {
		if ev.Action == "run.ended.expired" {
			rows = append(rows, ev)
		}
	}
	return rows
}

// A kept run's due end whose lock is held publishes nothing and starts no pass, pass after pass; on the first
// pass with the lock free it ends under it, its folder comes back, and no sweep stops the sandbox during that
// pass back.
func TestStopKeptRun_AHeldLockDefersTheEndToALaterPass(t *testing.T) {
	s, cs, audit, run, sb, leaser, hold := keptEndFixture(t)
	var ran, terminal, orphan bool
	var stops int
	sb.onPass = func() {
		if ran {
			return
		}
		ran = true
		ctx := context.Background()
		cur, err := s.cfg.Store.GetRun(ctx, run.ID)
		if err != nil {
			t.Error(err)
			return
		}
		terminal = isTerminalRunState(cur.State) && cur.SandboxRef != ""
		before := sb.stopCount()
		s.sweepTerminalSandboxRun(ctx, cur)
		if err := s.reconcileOrphanedSandbox(ctx); err != nil {
			t.Error(err)
		}
		if err := s.sweepOrphanedSandboxes(ctx); err != nil {
			t.Error(err)
		}
		stops, orphan = sb.stopCount()-before, s.cfg.Runner.(*passWindowSandbox).orphan
	}
	end := func() {
		s.stopKeptRun(context.Background(), leaser, run, types.RunStopped, "run.ended.expired", map[string]any{"ended_at": run.LostAt})
	}
	for pass := 1; pass <= 2; pass++ {
		end()
		if st, _ := cs.GetRun(context.Background(), run.ID); st.State != types.RunRunning || leaser.calls != 0 || ran || len(expiredRows(audit)) != 0 {
			t.Fatalf("pass %d with the lock held: state %s, %d transitions, pass back %v, %d rows; want RUNNING and none",
				pass, st.State, leaser.calls, ran, len(expiredRows(audit)))
		}
	}
	hold()
	end()
	if st, _ := cs.GetRun(context.Background(), run.ID); st.State != types.RunStopped {
		t.Fatalf("state %s after the lock was freed, want STOPPED", st.State)
	}
	if !ran || !terminal {
		t.Fatalf("pass back ran %v with the run terminal and its ref set %v; want both", ran, terminal)
	}
	if stops != 0 || orphan {
		t.Errorf("during the pass back: %d sandbox stops, orphan verdict %v; want none and false", stops, orphan)
	}
	if len(expiredRows(audit)) != 1 {
		t.Errorf("%d run.ended.expired rows, want the end's one", len(expiredRows(audit)))
	}
}

// keptEndClock fixes the fixture's server clock at start and answers a move of it.
type keptEndClock struct{ now time.Time }

func (c *keptEndClock) Now() time.Time { return c.now }

// dueAt sets the end the run's fresh row is kept until: ended at t (the grace is zero in these fixtures).
// The listing row the test passes to stopKeptRun keeps its own, stale, end.
func dueAt(cs *computerFolderStore, t time.Time) {
	cs.dispatchTestStore.mu.Lock()
	defer cs.dispatchTestStore.mu.Unlock()
	cs.run.LostAt, cs.run.LostReason, cs.run.EndsAt = &t, types.LostEnded, nil
}

// forcedStop is the one failure row a forced stop writes.
type forcedStop struct {
	PastDue int64 `json:"past_due_sec"`
	Stopped bool  `json:"sandbox_stopped"`
}

func oneForcedStop(t *testing.T, audit *recRecorder) forcedStop {
	t.Helper()
	rows := expiredRows(audit)
	if len(rows) != 1 {
		t.Fatalf("%d run.ended.expired rows, want 1", len(rows))
	}
	var data forcedStop
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if rows[0].Outcome != "failure" || !data.Stopped {
		t.Errorf("row %s %s; want failure, sandbox_stopped", rows[0].Outcome, rows[0].Data)
	}
	return data
}

// A due end whose lock is held is stopped without the lock once it is keptEndHoldOffLimit past due, read from
// the run's fresh row: the proxy first, then the sandbox, with one failure row and no terminal state or pass
// back. The visits at +1m and +7m leave a gap longer than the limit; the stop still comes at the first visit
// past the anchor, with the true past_due_sec. A later visit still held adds nothing.
func TestStopKeptRun_AnEndPastDueWithTheLockHeldStopsTheSandbox(t *testing.T) {
	s, cs, audit, run, sb, leaser, hold := keptEndFixture(t)
	defer hold()
	due := time.Now()
	clock := &keptEndClock{now: due}
	s.cfg.Now = clock.Now
	dueAt(cs, due)
	sb.proxyStopped.Store(false)
	var proxyFirst, passed bool
	sb.stopped = func() { proxyFirst = sb.proxyStopped.Load() }
	sb.onPass = func() { passed = true }
	end := func(after time.Duration) {
		clock.now = due.Add(after)
		s.stopKeptRun(context.Background(), leaser, run, types.RunStopped, "run.ended.expired", map[string]any{"ended_at": run.LostAt})
	}
	end(time.Minute)
	if sb.stopCount() != 0 || sb.proxyStopped.Load() || len(expiredRows(audit)) != 0 {
		t.Fatal("the sandbox or its proxy was stopped before the limit")
	}
	end(7 * time.Minute)
	if got := oneForcedStop(t, audit); got.PastDue != int64((7 * time.Minute).Seconds()) {
		t.Errorf("past_due_sec %d, want 420", got.PastDue)
	}
	if !proxyFirst || sb.stopCount() != 1 {
		t.Errorf("proxy stopped before the sandbox %v, sandbox stops %d; want true and 1", proxyFirst, sb.stopCount())
	}
	if st, _ := cs.GetRun(context.Background(), run.ID); st.State != types.RunRunning || leaser.calls != 0 || passed {
		t.Errorf("state %s, %d transitions, pass back %v; want RUNNING, none, none", st.State, leaser.calls, passed)
	}
	end(8 * time.Minute)
	if n := len(expiredRows(audit)); n != 1 || sb.stopCount() != 1 {
		t.Errorf("the next held pass: %d rows, %d stops; want 1 and 1", n, sb.stopCount())
	}
}

// A restart loses nothing: a new Server whose lock is still held stops the sandbox at 5m01s past due.
func TestStopKeptRun_ARestartedServerStopsAtTheSameAnchor(t *testing.T) {
	s, cs, audit, run, sb, leaser, hold := keptEndFixture(t)
	defer hold()
	due := time.Now()
	clock := &keptEndClock{now: due.Add(time.Minute)}
	s.cfg.Now = clock.Now
	dueAt(cs, due)
	end := func(s *Server) {
		s.stopKeptRun(context.Background(), leaser, run, types.RunStopped, "run.ended.expired", map[string]any{"ended_at": run.LostAt})
	}
	end(s)
	if sb.stopCount() != 0 {
		t.Fatal("stopped before the limit")
	}
	restarted := New(s.cfg)
	_, holdRestarted, ok := restarted.tryLockRunOp(context.Background(), run.ID)
	if !ok {
		t.Fatal("could not take the run's lock on the restarted server")
	}
	defer holdRestarted()
	clock.now = due.Add(5*time.Minute + time.Second)
	end(restarted)
	if got := oneForcedStop(t, audit); got.PastDue != 301 || sb.stopCount() != 1 {
		t.Errorf("past_due_sec %d, sandbox stops %d; want 301 and 1", got.PastDue, sb.stopCount())
	}
}

// Moving the stored end later moves the anchor, though the listing row stays stale. The end here comes from
// the run's EndsAt.
func TestStopKeptRun_AnExtensionMovesTheAnchor(t *testing.T) {
	s, cs, audit, run, sb, leaser, hold := keptEndFixture(t)
	defer hold()
	due := time.Now()
	clock := &keptEndClock{now: due}
	s.cfg.Now = clock.Now
	setEnd := func(at time.Time) {
		cs.dispatchTestStore.mu.Lock()
		defer cs.dispatchTestStore.mu.Unlock()
		cs.run.LostAt, cs.run.LostReason, cs.run.EndsAt = nil, "", &at
	}
	end := func(after time.Duration) {
		clock.now = due.Add(after)
		s.stopKeptRun(context.Background(), leaser, run, types.RunStopped, "run.ended.expired", map[string]any{"ended_at": run.LostAt})
	}
	setEnd(due)
	end(4 * time.Minute)
	setEnd(due.Add(10 * time.Minute))
	end(6 * time.Minute)
	end(14 * time.Minute)
	if sb.stopCount() != 0 || len(expiredRows(audit)) != 0 {
		t.Fatalf("%d stops, %d rows before the moved anchor; want none", sb.stopCount(), len(expiredRows(audit)))
	}
	end(15 * time.Minute)
	if got := oneForcedStop(t, audit); got.PastDue != 300 || sb.stopCount() != 1 {
		t.Errorf("past_due_sec %d, sandbox stops %d; want 300 and 1", got.PastDue, sb.stopCount())
	}
}

// The fresh row decides, not the listing's: a listing that says the end passed long ago does not force a
// run whose stored end is recent, nor one that is already terminal.
func TestStopKeptRun_TheFreshRowDecides(t *testing.T) {
	s, cs, audit, run, sb, leaser, hold := keptEndFixture(t)
	defer hold()
	due := time.Now()
	clock := &keptEndClock{now: due.Add(time.Minute)}
	s.cfg.Now = clock.Now
	dueAt(cs, due)
	end := func() {
		s.stopKeptRun(context.Background(), leaser, run, types.RunStopped, "run.ended.expired", map[string]any{"ended_at": run.LostAt})
	}
	end()
	if sb.stopCount() != 0 || len(expiredRows(audit)) != 0 {
		t.Fatalf("a stale listing forced the stop: %d stops, %d rows", sb.stopCount(), len(expiredRows(audit)))
	}
	cs.dispatchTestStore.mu.Lock()
	cs.state = types.RunStopped
	cs.dispatchTestStore.mu.Unlock()
	clock.now = due.Add(time.Hour)
	end()
	if sb.stopCount() != 0 || len(expiredRows(audit)) != 0 {
		t.Errorf("a terminal fresh row was forced: %d stops, %d rows", sb.stopCount(), len(expiredRows(audit)))
	}
}

// Successive holders do not stretch the window: A holds until 3m past due and B from then on, with visits at
// 3m30s and 5m30s both finding it held. The stop comes at 5m30s.
func TestStopKeptRun_SuccessiveHoldersDoNotStretchTheWindow(t *testing.T) {
	s, cs, audit, run, sb, leaser, holdA := keptEndFixture(t)
	due := time.Now()
	clock := &keptEndClock{now: due}
	s.cfg.Now = clock.Now
	dueAt(cs, due)
	end := func(after time.Duration) {
		clock.now = due.Add(after)
		s.stopKeptRun(context.Background(), leaser, run, types.RunStopped, "run.ended.expired", map[string]any{"ended_at": run.LostAt})
	}
	end(-time.Minute)
	end(2 * time.Minute)
	holdA()
	_, holdB, ok := s.tryLockRunOp(context.Background(), run.ID)
	if !ok {
		t.Fatal("could not take the run's lock for the second holder")
	}
	defer holdB()
	end(3*time.Minute + 30*time.Second)
	if sb.stopCount() != 0 || len(expiredRows(audit)) != 0 {
		t.Fatalf("stopped at 3m30s: %d stops, %d rows", sb.stopCount(), len(expiredRows(audit)))
	}
	end(5*time.Minute + 30*time.Second)
	if got := oneForcedStop(t, audit); got.PastDue != 330 || sb.stopCount() != 1 {
		t.Errorf("past_due_sec %d, sandbox stops %d; want 330 and 1", got.PastDue, sb.stopCount())
	}
}
