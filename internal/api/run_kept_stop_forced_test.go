// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// forcedMarks is the forced end's claim and mark over a computerFolderStore: the claim wins the run's RUNNING
// state under StopKeptRunIf's predicate (the exact lost mark and end) and stamps the mark in the same step.
type forcedMarks struct {
	cs          *computerFolderStore
	marksMu     sync.Mutex
	marked      []store.ForcedStop
	claims      int
	containment string
}

func (m *forcedMarks) StopKeptRunForcedIf(_ context.Context, id uuid.UUID, to types.RunState, lostAt *time.Time, lostReason types.LostReason, endsAt *time.Time) (time.Time, bool, error) {
	m.cs.dispatchTestStore.mu.Lock()
	cur := m.cs.run
	if m.cs.state != types.RunRunning || !sameKeptMark(cur, types.AgentRun{LostAt: lostAt, LostReason: lostReason, EndsAt: endsAt}) {
		m.cs.dispatchTestStore.mu.Unlock()
		return time.Time{}, false, nil
	}
	m.cs.state = to
	m.cs.dispatchTestStore.mu.Unlock()
	stamp := time.Now()
	m.marksMu.Lock()
	defer m.marksMu.Unlock()
	m.marked = append(m.marked, store.ForcedStop{RunID: id, At: stamp})
	m.claims++
	return stamp, true, nil
}

func (m *forcedMarks) ListForcedStops(context.Context) ([]store.ForcedStop, error) {
	m.marksMu.Lock()
	defer m.marksMu.Unlock()
	return append([]store.ForcedStop(nil), m.marked...), nil
}

func (m *forcedMarks) ClearForcedStop(_ context.Context, id uuid.UUID, stamp time.Time) (bool, error) {
	m.marksMu.Lock()
	defer m.marksMu.Unlock()
	for i, f := range m.marked {
		if f.RunID == id && f.At.Equal(stamp) {
			m.marked = append(m.marked[:i], m.marked[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func (m *forcedMarks) markCount() int {
	m.marksMu.Lock()
	defer m.marksMu.Unlock()
	return len(m.marked)
}

func (m *forcedMarks) claimCount() int {
	m.marksMu.Lock()
	defer m.marksMu.Unlock()
	return m.claims
}

// keptEndLeaser is a RunLeaser whose StopKeptRunIf makes a RUNNING run terminal in its computer folder store,
// counting its calls, and whose forced claim is forcedMarks'. The run's containment error is a field apart from
// the mark, as it is a column apart.
type keptEndLeaser struct {
	store.RunLeaser
	*forcedMarks
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

func (l *keptEndLeaser) SetRunContainmentError(_ context.Context, _ uuid.UUID, msg string, _ time.Time) error {
	l.marksMu.Lock()
	defer l.marksMu.Unlock()
	l.containment = msg
	return nil
}

func (l *keptEndLeaser) ClearRunContainmentError(context.Context, uuid.UUID) (bool, error) {
	l.marksMu.Lock()
	defer l.marksMu.Unlock()
	had := l.containment != ""
	l.containment = ""
	return had, nil
}

// markedStore is the fixture's store with the forced end's claim and mark.
type markedStore struct {
	passWindowStore
	*forcedMarks
}

// keptEndFixture is a run with a computer folder brought back both ways, kept after its end and due to be
// stopped, whose lock the test holds. It answers the release of that hold.
func keptEndFixture(t *testing.T) (*Server, *computerFolderStore, *recRecorder, types.AgentRun, *agentSandbox, *keptEndLeaser, func()) {
	t.Helper()
	s, cs, audit, run, sb, _ := bringBackFixture(t)
	withoutRunner(cs)
	marks := &forcedMarks{cs: cs}
	s.cfg.Runner, s.cfg.Store = &passWindowSandbox{agentSandbox: sb, run: run.ID}, markedStore{passWindowStore{refStore{cs}}, marks}
	ended := time.Now().Add(-8 * 24 * time.Hour)
	run.LostAt, run.LostReason = &ended, types.LostEnded
	_, hold, ok := s.tryLockRunOp(context.Background(), run.ID)
	if !ok {
		t.Fatal("could not take the run's lock")
	}
	return s, cs, audit, run, sb, &keptEndLeaser{cs: cs, forcedMarks: marks}, hold
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

// dueAtStore sets the end the run's fresh row is kept until: ended at t (the grace is zero in these fixtures).
// The listing row the test passes to stopKeptRun keeps its own, stale, end.
func dueAtStore(cs *computerFolderStore, t time.Time) {
	cs.dispatchTestStore.mu.Lock()
	defer cs.dispatchTestStore.mu.Unlock()
	cs.run.LostAt, cs.run.LostReason, cs.run.EndsAt = &t, types.LostEnded, nil
}

// dueAt is dueAtStore for the fresh row and the listing row alike: the sweep that lists a run sees its mark.
func dueAt(cs *computerFolderStore, run *types.AgentRun, t time.Time) {
	dueAtStore(cs, t)
	run.LostAt, run.LostReason, run.EndsAt = &t, types.LostEnded, nil
}

// forcedStop is the one failure row a forced stop writes.
type forcedStop struct {
	PastDue int64 `json:"past_due_sec"`
	Stopped bool  `json:"sandbox_stopped"`
	Forced  bool  `json:"forced"`
}

// forcedRows are the failure rows a forced stop wrote under action, in order: the rows with past_due_sec.
func forcedRows(t *testing.T, audit *recRecorder, action string) []forcedStop {
	t.Helper()
	var rows []forcedStop
	for _, ev := range audit.snapshot() {
		var data forcedStop
		var raw map[string]any
		if ev.Action != action || json.Unmarshal(ev.Data, &raw) != nil {
			continue
		}
		if _, ok := raw["past_due_sec"]; !ok {
			continue
		}
		if ev.Outcome != "failure" {
			t.Errorf("row %s %s; want a failure row", ev.Outcome, ev.Data)
		}
		if err := json.Unmarshal(ev.Data, &data); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, data)
	}
	return rows
}

func oneForcedStop(t *testing.T, audit *recRecorder) forcedStop {
	t.Helper()
	rows := forcedRows(t, audit, "run.ended.expired")
	if len(rows) != 1 {
		t.Fatalf("%d forced rows, want 1", len(rows))
	}
	if !rows[0].Stopped || !rows[0].Forced {
		t.Errorf("row %+v; want sandbox_stopped and forced", rows[0])
	}
	return rows[0]
}

func waitCh(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// forced is a kept run due 6 minutes ago, whose lock the test holds, with a clock to move.
type forced struct {
	s       *Server
	cs      *computerFolderStore
	audit   *recRecorder
	run     types.AgentRun
	sb      *agentSandbox
	leaser  *keptEndLeaser
	clock   *keptEndClock
	due     time.Time
	release func()
}

func newForced(t *testing.T) *forced {
	t.Helper()
	s, cs, audit, run, sb, leaser, hold := keptEndFixture(t)
	var once sync.Once
	f := &forced{s: s, cs: cs, audit: audit, run: run, sb: sb, leaser: leaser, due: time.Now(), release: func() { once.Do(hold) }}
	t.Cleanup(f.release)
	f.clock = &keptEndClock{now: f.due.Add(6 * time.Minute)}
	s.cfg.Now = f.clock.Now
	dueAt(cs, &f.run, f.due)
	return f
}

// end is the lease sweep's visit of the run's due end, on server s.
func (f *forced) end(s *Server) {
	s.stopKeptRun(context.Background(), f.leaser, f.run, types.RunStopped, "run.ended.expired", map[string]any{"ended_at": f.run.LostAt})
}

// tick is a watcher tick's forced stop retry, a minute on.
func (f *forced) tick(t *testing.T, s *Server) {
	t.Helper()
	f.clock.now = f.clock.now.Add(time.Minute)
	if err := s.sweepForcedStops(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func (f *forced) state() types.RunState {
	st, _ := f.cs.GetRun(context.Background(), f.run.ID)
	return st.State
}

// setRecord is the computer folder's pass record as the store holds it.
func (f *forced) setRecord(rec types.RunLocalFolderStatus) {
	f.cs.smu.Lock()
	defer f.cs.smu.Unlock()
	f.cs.status = rec
}

// claimOnly is a forced end whose process died after the claim: the run is terminal and marked, and nothing
// was stopped.
func (f *forced) claimOnly(t *testing.T) {
	t.Helper()
	if _, ok, err := f.leaser.StopKeptRunForcedIf(context.Background(), f.run.ID, types.RunStopped, f.run.LostAt, f.run.LostReason, f.run.EndsAt); err != nil || !ok {
		t.Fatalf("claim = %v, %v; want applied", ok, err)
	}
}

// proxyCounted counts StopProxy over the fixture's runner.
type proxyCounted struct {
	*passWindowSandbox
	proxyStops atomic.Int32
}

func (r *proxyCounted) StopProxy(ctx context.Context, ref string) error {
	r.proxyStops.Add(1)
	return r.passWindowSandbox.StopProxy(ctx, ref)
}

// getHookStore runs after after each GetRun, as a slow reader that has read its row and not yet acted on it.
type getHookStore struct {
	markedStore
	after func()
}

func (g getHookStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	r, err := g.markedStore.GetRun(ctx, id)
	if g.after != nil {
		g.after()
	}
	return r, err
}

// bumpRefusedStore never moves the folder record's generation on: another owner got there first.
type bumpRefusedStore struct{ markedStore }

func (bumpRefusedStore) BumpRunLocalFolderGen(context.Context, uuid.UUID, store.FolderStatusGuard, time.Time) (types.RunLocalFolderStatus, bool, error) {
	return types.RunLocalFolderStatus{}, false, nil
}

// A due end whose lock is held is taken without the lock once it is keptEndHoldOffLimit past due, read from the
// run's fresh row: the proxy first, then the sandbox, the run's terminal state published, one failure row, and
// no pass back. The visits at +1m and +7m leave a gap longer than the limit; the stop still comes at the first
// visit past the anchor, with the true past_due_sec. A later visit adds nothing.
func TestStopKeptRun_AnEndPastDueWithTheLockHeldStopsTheSandbox(t *testing.T) {
	f := newForced(t)
	f.sb.proxyStopped.Store(false)
	var proxyFirst, passed bool
	f.sb.stopped = func() { proxyFirst = f.sb.proxyStopped.Load() }
	f.sb.onPass = func() { passed = true }
	at := func(after time.Duration) {
		f.clock.now = f.due.Add(after)
		f.end(f.s)
	}
	at(time.Minute)
	if f.sb.stopCount() != 0 || f.sb.proxyStopped.Load() || len(forcedRows(t, f.audit, "run.ended.expired")) != 0 {
		t.Fatal("the sandbox or its proxy was stopped before the limit")
	}
	at(7 * time.Minute)
	if got := oneForcedStop(t, f.audit); got.PastDue != int64((7 * time.Minute).Seconds()) {
		t.Errorf("past_due_sec %d, want 420", got.PastDue)
	}
	if !proxyFirst || f.sb.stopCount() != 1 {
		t.Errorf("proxy stopped before the sandbox %v, sandbox stops %d; want true and 1", proxyFirst, f.sb.stopCount())
	}
	if st := f.state(); st != types.RunStopped || f.leaser.calls != 0 || passed {
		t.Errorf("state %s, %d transitions, pass back %v; want STOPPED, none, none", st, f.leaser.calls, passed)
	}
	at(8 * time.Minute)
	if n := len(forcedRows(t, f.audit, "run.ended.expired")); n != 1 || f.sb.stopCount() != 1 {
		t.Errorf("the next held pass: %d rows, %d stops; want 1 and 1", n, f.sb.stopCount())
	}
}

// A restart loses nothing: a new Server whose lock is still held stops the sandbox at 5m01s past due.
func TestStopKeptRun_ARestartedServerStopsAtTheSameAnchor(t *testing.T) {
	f := newForced(t)
	f.clock.now = f.due.Add(time.Minute)
	f.end(f.s)
	if f.sb.stopCount() != 0 {
		t.Fatal("stopped before the limit")
	}
	restarted := New(f.s.cfg)
	_, holdRestarted, ok := restarted.tryLockRunOp(context.Background(), f.run.ID)
	if !ok {
		t.Fatal("could not take the run's lock on the restarted server")
	}
	defer holdRestarted()
	f.clock.now = f.due.Add(5*time.Minute + time.Second)
	f.end(restarted)
	if got := oneForcedStop(t, f.audit); got.PastDue != 301 || f.sb.stopCount() != 1 {
		t.Errorf("past_due_sec %d, sandbox stops %d; want 301 and 1", got.PastDue, f.sb.stopCount())
	}
}

// Moving the stored end later moves the anchor. The end here comes from the lost run's EndsAt.
func TestStopKeptRun_AnExtensionMovesTheAnchor(t *testing.T) {
	f := newForced(t)
	setEnd := func(at time.Time) {
		lost := at.Add(-time.Hour)
		f.cs.dispatchTestStore.mu.Lock()
		f.cs.run.LostAt, f.cs.run.LostReason, f.cs.run.EndsAt = &lost, types.LostOutage, &at
		f.cs.dispatchTestStore.mu.Unlock()
		f.run.LostAt, f.run.LostReason, f.run.EndsAt = &lost, types.LostOutage, &at
	}
	at := func(after time.Duration) {
		f.clock.now = f.due.Add(after)
		f.end(f.s)
	}
	setEnd(f.due)
	at(4 * time.Minute)
	setEnd(f.due.Add(10 * time.Minute))
	at(6 * time.Minute)
	at(14 * time.Minute)
	if f.sb.stopCount() != 0 || len(forcedRows(t, f.audit, "run.ended.expired")) != 0 {
		t.Fatalf("%d stops, %d rows before the moved anchor; want none", f.sb.stopCount(), len(forcedRows(t, f.audit, "run.ended.expired")))
	}
	at(15 * time.Minute)
	if got := oneForcedStop(t, f.audit); got.PastDue != 300 || f.sb.stopCount() != 1 {
		t.Errorf("past_due_sec %d, sandbox stops %d; want 300 and 1", got.PastDue, f.sb.stopCount())
	}
}

// The fresh row decides: a listing whose mark is not the stored one claims nothing (a stale row, whatever its
// end), and nor does a listing that matches a run that is already terminal.
func TestStopKeptRun_TheFreshRowDecides(t *testing.T) {
	f := newForced(t)
	dueAtStore(f.cs, f.due) // the stored end moved on; the listing row keeps its 8-day-old end
	stale := f.run
	stale.LostAt = ptrTime(f.due.Add(-8 * 24 * time.Hour))
	f.s.stopKeptRun(context.Background(), f.leaser, stale, types.RunStopped, "run.ended.expired", nil)
	if f.sb.stopCount() != 0 || f.leaser.claimCount() != 0 || len(forcedRows(t, f.audit, "run.ended.expired")) != 0 {
		t.Fatalf("a stale listing forced the stop: %d stops, %d claims", f.sb.stopCount(), f.leaser.claimCount())
	}
	f.cs.dispatchTestStore.mu.Lock()
	f.cs.state = types.RunStopped
	f.cs.dispatchTestStore.mu.Unlock()
	f.clock.now = f.due.Add(time.Hour)
	f.end(f.s)
	if f.sb.stopCount() != 0 || f.leaser.claimCount() != 0 {
		t.Errorf("a terminal fresh row was forced: %d stops, %d claims", f.sb.stopCount(), f.leaser.claimCount())
	}
}

// Successive holders do not stretch the window: A holds until 3m past due and B from then on, with visits at
// 3m30s and 5m30s both finding it held. The stop comes at 5m30s.
func TestStopKeptRun_SuccessiveHoldersDoNotStretchTheWindow(t *testing.T) {
	f := newForced(t)
	at := func(after time.Duration) {
		f.clock.now = f.due.Add(after)
		f.end(f.s)
	}
	at(-time.Minute)
	at(2 * time.Minute)
	f.release()
	_, holdB, ok := f.s.tryLockRunOp(context.Background(), f.run.ID)
	if !ok {
		t.Fatal("could not take the run's lock for the second holder")
	}
	defer holdB()
	at(3*time.Minute + 30*time.Second)
	if f.sb.stopCount() != 0 || len(forcedRows(t, f.audit, "run.ended.expired")) != 0 {
		t.Fatalf("stopped at 3m30s: %d stops", f.sb.stopCount())
	}
	at(5*time.Minute + 30*time.Second)
	if got := oneForcedStop(t, f.audit); got.PastDue != 330 || f.sb.stopCount() != 1 {
		t.Errorf("past_due_sec %d, sandbox stops %d; want 330 and 1", got.PastDue, f.sb.stopCount())
	}
}

// A gone run has no end to be past: it is due when it was lost, so a lock held for good is cut 5 minutes
// after that, with or without an end on the row; it ends FAILED.
func TestStopKeptRun_AGoneRunIsDueWhenItWasLost(t *testing.T) {
	for name, endsAt := range map[string]*time.Time{"no end": nil, "an end days away": ptrTime(time.Now().Add(72 * time.Hour))} {
		t.Run(name, func(t *testing.T) {
			f := newForced(t)
			lost := time.Now()
			f.cs.dispatchTestStore.mu.Lock()
			f.cs.run.LostAt, f.cs.run.LostReason, f.cs.run.EndsAt = &lost, types.LostGone, endsAt
			f.cs.dispatchTestStore.mu.Unlock()
			f.run.LostAt, f.run.LostReason, f.run.EndsAt = &lost, types.LostGone, endsAt
			at := func(after time.Duration) {
				f.clock.now = lost.Add(after)
				f.s.stopKeptRun(context.Background(), f.leaser, f.run, types.RunFailed, "run.lost", map[string]any{"reason": string(types.LostGone)})
			}
			at(4 * time.Minute)
			if f.sb.stopCount() != 0 || len(forcedRows(t, f.audit, "run.lost")) != 0 {
				t.Fatal("stopped before the limit")
			}
			at(6 * time.Minute)
			rows := forcedRows(t, f.audit, "run.lost")
			if len(rows) != 1 || rows[0].PastDue != 360 || !rows[0].Stopped || f.sb.stopCount() != 1 || f.state() != types.RunFailed {
				t.Errorf("rows %+v, sandbox stops %d, state %s; want one with past_due_sec 360, stopped, 1 stop, FAILED", rows, f.sb.stopCount(), f.state())
			}
		})
	}
}

// Only a lock found held counts: a try the locker refuses, past the anchor, stops nothing; the same visit
// with the lock held does.
func TestStopKeptRun_ARefusedTryIsNotAHeldLock(t *testing.T) {
	f := newForced(t)
	f.clock.now = f.due.Add(10 * time.Minute)
	f.s.locks.override = exhaustedPool()
	f.end(f.s)
	if f.sb.stopCount() != 0 || f.leaser.claimCount() != 0 {
		t.Fatalf("a refused try forced the stop: %d stops, %d claims", f.sb.stopCount(), f.leaser.claimCount())
	}
	f.s.locks.override = nil
	f.end(f.s)
	if got := oneForcedStop(t, f.audit); got.PastDue != 600 || f.sb.stopCount() != 1 {
		t.Errorf("past_due_sec %d, sandbox stops %d; want 600 and 1", got.PastDue, f.sb.stopCount())
	}
}

// A row that is no longer lost (a revive claimed it since the listing) is not forced, however stale its
// stored end.
func TestStopKeptRun_ARowNoLongerLostIsNotForced(t *testing.T) {
	f := newForced(t)
	past := time.Now().Add(-time.Hour)
	f.cs.dispatchTestStore.mu.Lock()
	f.cs.run.LostAt, f.cs.run.LostReason, f.cs.run.EndsAt = nil, "", &past
	f.cs.dispatchTestStore.mu.Unlock()
	f.end(f.s)
	if f.sb.stopCount() != 0 || f.leaser.claimCount() != 0 {
		t.Errorf("a row with no lost mark was forced: %d stops, %d claims", f.sb.stopCount(), f.leaser.claimCount())
	}
}

// A run the force ended is terminal: a later visit, with the end moved or not, forces nothing more.
func TestStopKeptRun_AForcedRunIsNotForcedAgain(t *testing.T) {
	f := newForced(t)
	f.end(f.s)
	if f.sb.stopCount() != 1 || f.leaser.claimCount() != 1 {
		t.Fatalf("%d stops, %d claims after the first force, want 1 and 1", f.sb.stopCount(), f.leaser.claimCount())
	}
	dueAt(f.cs, &f.run, f.due.Add(10*time.Minute))
	f.clock.now = f.due.Add(16 * time.Minute)
	f.end(f.s)
	if rows := forcedRows(t, f.audit, "run.ended.expired"); len(rows) != 1 || f.sb.stopCount() != 1 || f.leaser.claimCount() != 1 {
		t.Errorf("rows %+v, %d stops, %d claims; want 1 row, 1 stop and 1 claim", rows, f.sb.stopCount(), f.leaser.claimCount())
	}
}

// C-101, T-1521: a force that read the run before another replica's terminal pass began, and acts after it,
// stops nothing. A reads the run and pauses; B ends the run through StopKeptRunIf and its pass back blocks in the
// runner; A resumes. A's claim loses: it makes no stop and writes no row or mark, and B's pass settles at its
// own generation.
func TestForceKeptStop_LosesToATerminalPassThatStartedAfterItsRead(t *testing.T) {
	f := newForced(t)
	f.release() // B, the fixture's server, finds the lock free
	counted := &proxyCounted{passWindowSandbox: f.s.cfg.Runner.(*passWindowSandbox)}
	f.s.cfg.Runner = counted
	read, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	cfgA := f.s.cfg
	cfgA.Store = getHookStore{markedStore: f.s.cfg.Store.(markedStore), after: func() {
		once.Do(func() { close(read); <-resume })
	}}
	a := New(cfgA)
	_, holdA, ok := a.tryLockRunOp(context.Background(), f.run.ID)
	if !ok {
		t.Fatal("could not take the run's lock on A")
	}
	defer holdA()
	aDone := make(chan struct{})
	go func() { defer close(aDone); f.end(a) }()
	waitCh(t, read, "A's read")
	inPass, release := make(chan struct{}), make(chan struct{})
	var passOnce sync.Once
	f.sb.onPass = func() { passOnce.Do(func() { close(inPass); <-release }) }
	bDone := make(chan struct{})
	go func() { defer close(bDone); f.end(f.s) }()
	waitCh(t, inPass, "B's pass back")
	close(resume)
	waitCh(t, aDone, "A's return")
	if f.sb.stopCount() != 0 || len(forcedRows(t, f.audit, "run.ended.expired")) != 0 || f.leaser.markCount() != 0 || f.leaser.claimCount() != 0 {
		t.Errorf("A stopped %d sandboxes, wrote %d rows, %d claims, %d marks; want none", f.sb.stopCount(),
			len(forcedRows(t, f.audit, "run.ended.expired")), f.leaser.claimCount(), f.leaser.markCount())
	}
	if n := counted.proxyStops.Load(); n > 1 {
		t.Errorf("StopProxy = %d, want at most B's one quiesce: A's claim lost, so it stops nothing", n)
	}
	close(release)
	waitCh(t, bDone, "B's end")
	if rec := f.cs.folderStatus(); rec.Gen == 0 || rec.Gen != rec.Settled {
		t.Errorf("record gen %d settled %d after B's pass; want B's generation, settled", rec.Gen, rec.Settled)
	}
}

// The force that wins the claim ends the run itself and runs no pass back: the holder, finding the run
// terminal when it gets to its own end, starts none either. The record is settled at the force's generation
// with no resume and says the changes were not brought back.
func TestForceKeptStop_WinsTheClaimAndTheHolderStartsNoPass(t *testing.T) {
	f := newForced(t)
	var passed bool
	f.sb.onPass = func() { passed = true }
	before := f.cs.folderStatus().Gen
	f.end(f.s)
	if f.state() != types.RunStopped || f.leaser.claimCount() != 1 {
		t.Fatalf("state %s, %d claims; want STOPPED and 1", f.state(), f.leaser.claimCount())
	}
	rec := f.cs.folderStatus()
	if rec.Gen != before+1 || rec.Settled != rec.Gen || rec.Resume != nil || rec.Back == nil || !strings.Contains(rec.Back.Error, "Not brought back") {
		t.Errorf("record %+v after the force (gen was %d); want the next generation, settled, no resume, not brought back", rec, before)
	}
	f.release()
	f.end(f.s) // the holder's end, once the lock is free
	if passed || f.leaser.calls != 1 {
		t.Errorf("pass back %v, StopKeptRunIf calls %d; want none and 1 (refused)", passed, f.leaser.calls)
	}
	oneForcedStop(t, f.audit)
}

// M4: a listing row older than the stored mark claims nothing, though the stored end is long past due.
func TestForceKeptStop_AStaleListingRowClaimsNothing(t *testing.T) {
	f := newForced(t)
	stale := f.run
	stale.EndsAt = ptrTime(f.due.Add(-time.Hour))
	f.s.stopKeptRun(context.Background(), f.leaser, stale, types.RunStopped, "run.ended.expired", nil)
	if f.sb.stopCount() != 0 || f.sb.proxyStopped.Load() || f.leaser.claimCount() != 0 || f.leaser.markCount() != 0 || f.state() != types.RunRunning {
		t.Errorf("a stale listing acted: %d stops, proxy stopped %v, %d claims, state %s", f.sb.stopCount(), f.sb.proxyStopped.Load(), f.leaser.claimCount(), f.state())
	}
	if len(expiredRows(f.audit)) != 0 {
		t.Errorf("a stale listing wrote %d rows", len(expiredRows(f.audit)))
	}
}

// Codex's counterexample (C-073): a revive admitted before the run's end, and still preparing its proxy when the
// force fires, cannot claim the run afterwards. The force takes the run's RUNNING state, which every revive
// claim requires, and the revive is refused with no new proxy started.
func TestForceKeptStop_AReviveBlockedBeforeItsClaimIsRefusedAfterTheForcedEnd(t *testing.T) {
	f := newReviveFixture(t)
	f.st.mu.Lock()
	lost := f.st.run.LostAt
	f.st.mu.Unlock()
	if lost == nil {
		t.Fatal("the fixture's run is not lost")
	}
	f.rr.onEnsure = func() {
		f.now = lost.Add(keptEndHoldOffLimit + time.Minute)
		run, _ := f.rs.GetRun(context.Background(), f.run.ID)
		f.srv.stopKeptRun(context.Background(), f.rs, run, types.RunStopped, "run.lost.expired", map[string]any{"reason": string(run.LostReason)})
	}
	if code := f.revive(t); code == http.StatusOK {
		t.Fatalf("revive: code %d after the forced end, want a refusal", code)
	}
	if got := f.st.State(); got != types.RunStopped {
		t.Errorf("state %s, want STOPPED by the force", got)
	}
	if len(f.rr.replaced) != 0 {
		t.Errorf("%d proxies were started for a run the force ended", len(f.rr.replaced))
	}
}

// H1: a forced stop whose sandbox stop fails is retried on the next watcher tick while the lock is still held:
// the mark stays, the retry is the proxy stop and then the sandbox stop (the record is settled, so rule R stops
// it), and the mark clears once the stop is confirmed. A tick after that does nothing.
func TestForceKeptStop_AFailedStopIsRetriedOnTheNextTickWhileTheLockIsHeld(t *testing.T) {
	f := newForced(t)
	f.sb.stopErr = errors.New("the runner did not answer the stop")
	f.end(f.s)
	if rows := forcedRows(t, f.audit, "run.ended.expired"); len(rows) != 1 || rows[0].Stopped || f.sb.stopCount() != 0 {
		t.Fatalf("rows %+v, sandbox stops %d after a failed stop; want one row, not stopped, and no stop", rows, f.sb.stopCount())
	}
	if f.leaser.markCount() != 1 {
		t.Fatalf("%d marks after a failed stop, want 1", f.leaser.markCount())
	}
	f.sb.stopErr = nil
	f.tick(t, f.s)
	if f.sb.stopCount() != 1 || f.leaser.markCount() != 0 {
		t.Errorf("after the next tick: %d stops, %d marks; want 1 and 0", f.sb.stopCount(), f.leaser.markCount())
	}
	f.tick(t, f.s)
	if f.sb.stopCount() != 1 {
		t.Errorf("a tick after the mark cleared stopped again: %d stops", f.sb.stopCount())
	}
}

// A process that died after the claim leaves the run terminal and marked; the next tick, on a new server, stops
// its sandbox.
func TestForceKeptStop_ACrashAfterTheClaimIsStoppedOnTheNextTick(t *testing.T) {
	f := newForced(t)
	f.claimOnly(t)
	if f.sb.stopCount() != 0 || f.leaser.markCount() != 1 {
		t.Fatalf("%d stops, %d marks after the claim alone; want 0 and 1", f.sb.stopCount(), f.leaser.markCount())
	}
	restarted := New(f.s.cfg)
	f.tick(t, restarted)
	if f.sb.stopCount() != 1 || f.leaser.markCount() != 0 {
		t.Errorf("after the next tick: %d stops, %d marks; want 1 and 0", f.sb.stopCount(), f.leaser.markCount())
	}
}

// N2: containment_error is another column with another writer. The re-assert that clears it under the lock
// (settleContainment) while the force's stops fail leaves forced_stop_at set, and the next tick retries.
func TestForceKeptStop_AReassertClearDuringTheForcedEndKeepsTheMark(t *testing.T) {
	f := newForced(t)
	if err := f.leaser.SetRunContainmentError(context.Background(), f.run.ID, "the stop was queued", f.due); err != nil {
		t.Fatal(err)
	}
	f.sb.stopErr = errors.New("the runner did not answer the stop")
	f.end(f.s)
	if f.leaser.markCount() != 1 {
		t.Fatalf("%d marks after the forced end's failed stop, want 1", f.leaser.markCount())
	}
	reasserting := f.run
	reasserting.ContainmentError = "the stop was queued"
	f.s.settleContainment(context.Background(), f.leaser, reasserting, nil)
	if f.leaser.containment != "" {
		t.Fatal("the re-assert did not clear containment_error")
	}
	if f.leaser.markCount() != 1 {
		t.Fatalf("the re-assert's clear removed the forced mark: %d marks", f.leaser.markCount())
	}
	f.sb.stopErr = nil
	f.tick(t, f.s)
	if f.sb.stopCount() != 1 || f.leaser.markCount() != 0 {
		t.Errorf("after the next tick: %d stops, %d marks; want 1 and 0", f.sb.stopCount(), f.leaser.markCount())
	}
}

// Rule R, lock held: a record that may be in flight (another owner moved its generation on, so the force's bump
// is refused) on a server-hosted sandbox is kept with its proxy stopped, and the mark stays. When the lock is
// free the locked path keeps it for the resume (the record holds one) and the mark clears, the sandbox never stopped.
func TestForceKeptStop_AnUnsettledRecordOnAServerSandboxKeepsItWithItsProxyStopped(t *testing.T) {
	f := newForced(t)
	f.s.cfg.Store = bumpRefusedStore{f.s.cfg.Store.(markedStore)}
	f.setRecord(types.RunLocalFolderStatus{Gen: 3, Settled: 2, Resume: &types.FolderResume{Unsynced: 1}})
	f.end(f.s)
	if f.sb.stopCount() != 0 || !f.sb.proxyStopped.Load() {
		t.Fatalf("%d sandbox stops, proxy stopped %v; want the sandbox kept with its proxy stopped", f.sb.stopCount(), f.sb.proxyStopped.Load())
	}
	if run, _ := f.cs.GetRun(context.Background(), f.run.ID); run.SandboxRef == "" || f.state() != types.RunStopped || f.leaser.markCount() != 1 {
		t.Errorf("ref %q, state %s, %d marks; want the ref kept, STOPPED, marked", run.SandboxRef, f.state(), f.leaser.markCount())
	}
	if rows := forcedRows(t, f.audit, "run.ended.expired"); len(rows) != 1 || rows[0].Stopped {
		t.Errorf("rows %+v; want one, sandbox not stopped", rows)
	}
	f.release()
	f.tick(t, f.s)
	if f.sb.stopCount() != 0 || f.leaser.markCount() != 0 {
		t.Errorf("with the lock free: %d stops, %d marks; want the sandbox kept for its resume and the mark cleared", f.sb.stopCount(), f.leaser.markCount())
	}
}

// Rule R off the server (ruling D10 (a)): a record that may be in flight on a runner, a virtual machine, or
// Kubernetes in server mode (a proxy that cannot be stopped alone) has its sandbox stopped, as the locked path
// stops these.
func TestForceKeptStop_AnUnsettledRecordOffTheServerStopsTheSandbox(t *testing.T) {
	for name, set := range map[string]func(*forced){
		"a runner": func(f *forced) {
			id := f.cs.runner.ID
			f.cs.dispatchTestStore.mu.Lock()
			f.cs.run.RunnerID = &id
			f.cs.dispatchTestStore.mu.Unlock()
		},
		"a virtual machine": func(f *forced) {
			f.cs.dispatchTestStore.mu.Lock()
			f.cs.run.MachineKind = types.MachineLinuxVM
			f.cs.dispatchTestStore.mu.Unlock()
		},
		"kubernetes in server mode": func(f *forced) { f.sb.proxyErr = runner.ErrEndUnsupported },
	} {
		t.Run(name, func(t *testing.T) {
			f := newForced(t)
			f.s.cfg.Store = bumpRefusedStore{f.s.cfg.Store.(markedStore)}
			f.setRecord(types.RunLocalFolderStatus{Gen: 3, Settled: 2, Resume: &types.FolderResume{Unsynced: 1}})
			set(f)
			f.end(f.s)
			if f.sb.stopCount() != 1 {
				t.Errorf("%d sandbox stops, want 1: it cannot be kept with its proxy stopped alone", f.sb.stopCount())
			}
			if rows := forcedRows(t, f.audit, "run.ended.expired"); len(rows) != 1 || !rows[0].Stopped || f.leaser.markCount() != 0 {
				t.Errorf("rows %+v, %d marks; want one row, stopped, mark cleared", rows, f.leaser.markCount())
			}
		})
	}
}

// With the lock free the retry hands a marked run to the locked path: a sandbox kept for its computer folder's
// resume is left with only its proxy stopped, and any other is stopped; either clears the mark.
func TestForceKeptStop_AFreeLockHandsAMarkedRunToTheLockedPath(t *testing.T) {
	for name, tc := range map[string]struct {
		rec   types.RunLocalFolderStatus
		stops int
	}{
		"a resume is kept":     {types.RunLocalFolderStatus{Gen: 3, Settled: 2, Resume: &types.FolderResume{Unsynced: 1}}, 0},
		"no resume is stopped": {types.RunLocalFolderStatus{Gen: 3, Settled: 3}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			f := newForced(t)
			f.claimOnly(t)
			f.setRecord(tc.rec)
			f.release()
			f.tick(t, f.s)
			if f.sb.stopCount() != tc.stops || f.leaser.markCount() != 0 {
				t.Errorf("%d stops, %d marks; want %d and 0", f.sb.stopCount(), f.leaser.markCount(), tc.stops)
			}
			if kept := f.sb.proxyStopped.Load(); kept != (tc.stops == 0) {
				t.Errorf("proxy stopped %v; want %v", kept, tc.stops == 0)
			}
		})
	}
}

// wedgedStop is the fixture's runner whose first StopSandbox never answers until its ctx ends.
type wedgedStop struct {
	*passWindowSandbox
	calls atomic.Int32
}

func (r *wedgedStop) StopSandbox(ctx context.Context, ref string) error {
	if r.calls.Add(1) == 1 {
		<-ctx.Done()
		return ctx.Err()
	}
	return r.passWindowSandbox.StopSandbox(ctx, ref)
}

// A row's bound: a runner that never answers its stop is cut at forcedStopBound and the next marked row is
// stopped on the same tick.
func TestSweepForcedStops_AWedgedRunnerIsCutAtTheRowBoundAndTheNextRowStillRuns(t *testing.T) {
	old := forcedStopBound
	forcedStopBound = 100 * time.Millisecond
	t.Cleanup(func() { forcedStopBound = old })
	f := newForced(t)
	f.claimOnly(t)
	f.release()
	second := store.ForcedStop{RunID: uuid.New(), At: time.Now()}
	f.leaser.marksMu.Lock()
	f.leaser.marked = append(f.leaser.marked, second)
	f.leaser.marksMu.Unlock()
	w := &wedgedStop{passWindowSandbox: f.s.cfg.Runner.(*passWindowSandbox)}
	f.s.cfg.Runner = w
	start := time.Now()
	if err := f.s.sweepForcedStops(context.Background()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("the sweep took %v; the wedged row must be cut at its bound", took)
	}
	if w.calls.Load() != 2 {
		t.Errorf("StopSandbox calls = %d, want 2: the wedged row's and the next row's", w.calls.Load())
	}
	marked, _ := f.leaser.ListForcedStops(context.Background())
	if len(marked) != 1 || marked[0].RunID != f.run.ID {
		t.Errorf("marks left = %+v, want only the wedged row's", marked)
	}
}

// statusErrStop is the fixture's runner whose Status errors and whose StopSandbox calls are counted.
type statusErrStop struct {
	*passWindowSandbox
	stopCalls atomic.Int32
}

func (r *statusErrStop) Status(context.Context, string) (runner.Status, error) {
	return runner.Status{}, errors.New("the runner did not answer the probe")
}

func (r *statusErrStop) StopSandbox(ctx context.Context, ref string) error {
	r.stopCalls.Add(1)
	return r.passWindowSandbox.StopSandbox(ctx, ref)
}

// A probe that errors is not "already gone": the mark stays, the stop is tried on that tick, and a later stop
// that confirms clears it.
func TestSweepForcedStops_AProbeErrorKeepsTheMarkAndRetriesTheStop(t *testing.T) {
	f := newForced(t)
	f.claimOnly(t)
	f.release()
	w := &statusErrStop{passWindowSandbox: f.s.cfg.Runner.(*passWindowSandbox)}
	f.s.cfg.Runner = w
	f.sb.stopErr = errors.New("the runner did not answer the stop")
	f.tick(t, f.s)
	if w.stopCalls.Load() != 1 || f.leaser.markCount() != 1 {
		t.Fatalf("%d stop calls, %d marks after a probe error and a failed stop; want 1 and 1", w.stopCalls.Load(), f.leaser.markCount())
	}
	f.sb.stopErr = nil
	f.tick(t, f.s)
	if w.stopCalls.Load() != 2 || f.leaser.markCount() != 0 {
		t.Errorf("%d stop calls, %d marks after a confirmed stop; want 2 and 0", w.stopCalls.Load(), f.leaser.markCount())
	}
}

// N3: a kept run's sandbox is torn down (STOPPED, its lost mark left on the row) while its failed proxy start is
// still uncontained. The first pass of the proxy start deadline sweep fails the sandbox stop; the next tick's
// retry stops it and contains the start. Only the terminal arm of stopUnconfirmedProxy (the guard
// runIsKept(run) && !isTerminalRunState(run.State)) reaches StopSandbox for such a run: without the guard the
// run takes the stopLostSandbox arm, which only ends the agent (EndSandbox), so the StopSandbox count below stays 0.
func TestReadbackContainment_AFailedSandboxStopOfATornDownKeptRunIsRetriedOnTheNextTick(t *testing.T) {
	f := newReadbackFixture(t)
	gen := f.failWithUnstoppableProxy(t)
	f.st.mu.Lock()
	f.st.state = types.RunStopped
	lost := time.Now().Add(-time.Hour)
	f.st.run.LostAt, f.st.run.LostReason = &lost, types.LostEnded
	ref := f.st.run.SandboxRef
	f.st.mu.Unlock()
	flaky := &stopsFirstFails{hashedReviveRunner: f.hr}
	f.srv.cfg.Runner = flaky
	f.setProxyStopErr(nil)
	ends := f.lr.endCount()

	if err := f.srv.sweepOverdueProxyStarts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := flaky.stopCalls.Load(); n != 1 {
		t.Fatalf("StopSandbox calls after the first pass = %d, want 1: a torn-down kept run is stopped, not merely ended", n)
	}
	if p := f.active(t); p.ContainedAt != nil || p.ContainmentError == "" {
		t.Fatalf("start contained %v, error %q after a failed stop; want unresolved", p.ContainedAt, p.ContainmentError)
	}
	if err := f.srv.sweepOverdueProxyStarts(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := flaky.stopCalls.Load(); n != 2 {
		t.Errorf("StopSandbox calls after the retry tick = %d, want 2", n)
	}
	if p := f.active(t); p.ContainedAt == nil || p.ContainmentError != "" || p.Generation != gen {
		t.Errorf("start %d contained %v, error %q after the retry; want contained", p.Generation, p.ContainedAt, p.ContainmentError)
	}
	if f.lr.endCount() != ends {
		t.Errorf("EndSandbox ran %d more times: the run took the lost-run arm", f.lr.endCount()-ends)
	}
	if run, _ := f.srv.cfg.Store.GetRun(context.Background(), f.run.ID); run.SandboxRef != ref {
		t.Errorf("ref = %q, want %q: this arm stops the sandbox and leaves the ref to the tail", run.SandboxRef, ref)
	}
}

// stopsFirstFails is the readback fixture's runner whose first StopSandbox fails.
type stopsFirstFails struct {
	*hashedReviveRunner
	stopCalls atomic.Int32
}

func (r *stopsFirstFails) StopSandbox(ctx context.Context, ref string) error {
	if r.stopCalls.Add(1) == 1 {
		return errors.New("the runner did not answer the stop")
	}
	return r.hashedReviveRunner.StopSandbox(ctx, ref)
}
