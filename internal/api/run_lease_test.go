// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// leaseStore is dispatchTestStore plus store.RunLeaser, with the PG
// conditions of MarkRunEnded and MarkRunEndingSoon.
type leaseStore struct {
	*dispatchTestStore
	warnFor *time.Time
	warnSec int
}

func (s *leaseStore) ListLeasedRuns(ctx context.Context) ([]types.AgentRun, error) {
	run, _ := s.GetRun(ctx, s.run.ID)
	if run.State != types.RunRunning || (run.EndsAt == nil && run.LostAt == nil) {
		return nil, nil
	}
	return []types.AgentRun{run}, nil
}

func (s *leaseStore) MarkRunEnded(_ context.Context, _ uuid.UUID, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != types.RunRunning || s.run.LostAt != nil || s.run.EndsAt == nil || s.run.EndsAt.After(now) {
		return false, nil
	}
	s.run.LostAt, s.run.LostReason = &now, types.LostEnded
	return true, nil
}

func (s *leaseStore) MarkRunEndingSoon(_ context.Context, _ uuid.UUID, endsAt time.Time, sec int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.run.EndsAt == nil || !s.run.EndsAt.Equal(endsAt) || s.run.LostAt != nil {
		return false, nil
	}
	if s.warnFor != nil && s.warnFor.Equal(endsAt) && s.warnSec <= sec {
		return false, nil
	}
	s.warnFor, s.warnSec = &endsAt, sec
	return true, nil
}

func (s *leaseStore) SetRunEndAndWait(_ context.Context, _ uuid.UUID, fromEnd *time.Time, fromWait int, toEnd *time.Time, toWait int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur := s.run.EndsAt
	sameEnd := (fromEnd == nil && cur == nil) || (fromEnd != nil && cur != nil && fromEnd.Equal(*cur))
	if !sameEnd || fromWait != s.run.WaitBudgetSec || s.run.LostAt != nil || s.state.IsTerminal() {
		return false, nil
	}
	s.run.EndsAt, s.run.WaitBudgetSec = toEnd, toWait
	return true, nil
}

func (s *leaseStore) setEnd(endsAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.run.EndsAt = &endsAt
}

func (s *leaseStore) lost() (*time.Time, types.LostReason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.run.LostAt, s.run.LostReason
}

// leaseRunner is finalizeTailRunner that can also keep an ended sandbox.
type leaseRunner struct {
	*finalizeTailRunner
	endErr error
	ends   []string
}

func (r *leaseRunner) EndSandbox(_ context.Context, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends = append(r.ends, ref)
	return r.endErr
}

func (r *leaseRunner) endCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.ends)
}

// countingIdentity counts run-identity revocations: the end keeps the run
// identity, every teardown revokes it.
type countingIdentity struct {
	identity.Provider
	mu      sync.Mutex
	revokes int
}

func (c *countingIdentity) RevokeRun(ctx context.Context, runID uuid.UUID) error {
	c.mu.Lock()
	c.revokes++
	c.mu.Unlock()
	return c.Provider.RevokeRun(ctx, runID)
}

func (c *countingIdentity) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.revokes
}

func (b *raceBroker) count(runID uuid.UUID) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n[runID]
}

type leaseFixture struct {
	srv   *Server
	st    *leaseStore
	rn    *leaseRunner
	brk   *raceBroker
	idp   *countingIdentity
	fa    *fakeApprovals
	audit *syncAudit
	run   types.AgentRun
	now   time.Time
}

// newLeaseFixture is one RUNNING run with a sandbox, created 30 days ago, whose
// end is endsIn from the fixture's clock, and one PENDING approval.
func newLeaseFixture(t *testing.T, endsIn time.Duration) *leaseFixture {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	run := newFinalizeRun()
	run.SandboxRef = "ref-lease"
	run.CreatedAt = now.Add(-30 * 24 * time.Hour)
	endsAt := now.Add(endsIn)
	run.EndsAt = &endsAt
	f := &leaseFixture{
		st:    &leaseStore{dispatchTestStore: &dispatchTestStore{run: run, state: types.RunRunning}},
		rn:    &leaseRunner{finalizeTailRunner: &finalizeTailRunner{fakeRunner: &fakeRunner{}}},
		brk:   &raceBroker{},
		fa:    newFakeApprovals(),
		audit: &syncAudit{},
		run:   run,
		now:   now,
	}
	f.srv = newFinalizeTailServer(t, f.st.dispatchTestStore, f.brk, f.rn, f.audit)
	f.srv.cfg.Store = f.st
	f.srv.cfg.Approvals = f.fa
	f.srv.cfg.EndedRunGrace = 7 * 24 * time.Hour
	f.srv.cfg.Now = func() time.Time { return f.now }
	f.idp = &countingIdentity{Provider: f.srv.cfg.Identity}
	f.srv.cfg.Identity = f.idp
	seedPendingApproval(t, f.fa, run.ID)
	return f
}

func (f *leaseFixture) sweep(t *testing.T) {
	t.Helper()
	if err := f.srv.sweepRunLeases(context.Background()); err != nil {
		t.Fatalf("sweepRunLeases: %v", err)
	}
}

func leaseAuditData(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatalf("audit data: %v", err)
	}
	return m
}

// TestRunLease_EndStopsAndKeepsTheRun: at its end a run is marked ended, its
// PENDING approvals cancelled as run_ended, its broker credentials revoked and
// its sandbox ended (agent stopped, proxy removed) — but it is NOT torn down,
// stays RUNNING, and keeps its run identity. run.ended is audited once; a
// second pass only re-asserts the stop.
func TestRunLease_EndStopsAndKeepsTheRun(t *testing.T) {
	f := newLeaseFixture(t, -time.Minute)
	f.sweep(t)

	if lostAt, reason := f.st.lost(); lostAt == nil || reason != types.LostEnded {
		t.Fatalf("lost = %v %q, want the run marked ended", lostAt, reason)
	}
	if got := f.st.State(); got != types.RunRunning {
		t.Errorf("state = %s, want RUNNING — an ended run is kept, not terminal", got)
	}
	if f.rn.endCount() != 1 || f.rn.stopCount() != 0 {
		t.Errorf("EndSandbox = %d, StopSandbox = %d; want 1 and 0 — the files must survive the end",
			f.rn.endCount(), f.rn.stopCount())
	}
	if f.brk.count(f.run.ID) != 1 {
		t.Errorf("broker revocations = %d, want 1", f.brk.count(f.run.ID))
	}
	if f.idp.count() != 0 {
		t.Errorf("identity revocations = %d, want 0 — a kept run keeps its identity for revive", f.idp.count())
	}
	calls := f.fa.cancelledCalls()
	if len(calls) != 1 || calls[0].Reason != "run_ended" {
		t.Errorf("approval cancels = %+v, want one with reason run_ended", calls)
	}
	ended := f.audit.eventsFor(f.run.ID, "run.ended")
	if len(ended) != 1 || ended[0].Outcome != "success" || leaseAuditData(t, ended[0])["kept"] != true {
		t.Fatalf("run.ended events = %+v, want one success with kept:true", ended)
	}

	f.now = f.now.Add(time.Minute)
	f.sweep(t)
	if len(f.audit.eventsFor(f.run.ID, "run.ended")) != 1 {
		t.Error("a second pass audited run.ended again")
	}
	if f.rn.endCount() != 2 || f.rn.stopCount() != 0 || f.st.State() != types.RunRunning {
		t.Errorf("second pass: EndSandbox = %d, StopSandbox = %d, state %s; want the stop re-asserted and the run still kept",
			f.rn.endCount(), f.rn.stopCount(), f.st.State())
	}
	if f.brk.count(f.run.ID) != 1 {
		t.Errorf("second pass: broker revocations = %d, want still 1", f.brk.count(f.run.ID))
	}
}

// TestRunLease_EndFailsClosed: when the sandbox cannot be ended and kept, the
// run is stopped and torn down outright, with the full revoke cascade — never
// left running past its end with its proxy up.
func TestRunLease_EndFailsClosed(t *testing.T) {
	f := newLeaseFixture(t, -time.Minute)
	f.rn.endErr = errors.New("docker: remove proxy: boom")
	f.sweep(t)

	if got := f.st.State(); got != types.RunStopped {
		t.Fatalf("state = %s, want STOPPED — a run whose end failed must not keep running", got)
	}
	if f.rn.stopCount() != 1 {
		t.Errorf("StopSandbox = %d, want 1 (the full teardown)", f.rn.stopCount())
	}
	if f.idp.count() == 0 {
		t.Error("the run identity was not revoked on the fail-closed teardown")
	}
	ended := f.audit.eventsFor(f.run.ID, "run.ended")
	if len(ended) != 1 || leaseAuditData(t, ended[0])["kept"] != false || leaseAuditData(t, ended[0])["end_error"] == nil {
		t.Errorf("run.ended events = %+v, want one with kept:false and end_error", ended)
	}
}

// TestRunLease_TornDownAtTheEndWhenItCannotBeKept: no grace, or a runner that
// cannot keep a stopped sandbox (Kubernetes), stops the run at its end.
func TestRunLease_TornDownAtTheEndWhenItCannotBeKept(t *testing.T) {
	t.Run("grace 0", func(t *testing.T) {
		f := newLeaseFixture(t, -time.Minute)
		f.srv.cfg.EndedRunGrace = 0
		f.sweep(t)
		if f.st.State() != types.RunStopped || f.rn.stopCount() != 1 || f.rn.endCount() != 0 {
			t.Errorf("state %s, StopSandbox %d, EndSandbox %d; want STOPPED, 1, 0",
				f.st.State(), f.rn.stopCount(), f.rn.endCount())
		}
	})
	t.Run("a runner that cannot keep a sandbox", func(t *testing.T) {
		f := newLeaseFixture(t, -time.Minute)
		plain := f.rn.finalizeTailRunner
		f.srv.cfg.Runner = plain
		f.sweep(t)
		if f.st.State() != types.RunStopped || plain.stopCount() != 1 {
			t.Errorf("state %s, StopSandbox %d; want STOPPED, 1", f.st.State(), plain.stopCount())
		}
		if calls := f.fa.cancelledCalls(); len(calls) != 1 || calls[0].Reason != "run_ended" {
			t.Errorf("approval cancels = %+v, want one with reason run_ended", calls)
		}
	})
}

// TestRunLease_ACrashAfterTheClaimStillEndsTheRun: a crash between the claim
// and the end leaves a run marked kept with its sandbox and broker credentials
// up. The next pass revokes the credentials (once) and re-asserts the end; a
// substrate that cannot keep a sandbox says so for good, so the run is torn
// down rather than kept for the whole grace with nothing ever stopping it.
func TestRunLease_ACrashAfterTheClaimStillEndsTheRun(t *testing.T) {
	claim := func(t *testing.T, f *leaseFixture) {
		t.Helper()
		if applied, err := f.st.MarkRunEnded(context.Background(), f.run.ID, f.now); err != nil || !applied {
			t.Fatalf("MarkRunEnded = %v, %v; want the claim to land", applied, err)
		}
	}
	t.Run("a substrate that cannot keep a sandbox", func(t *testing.T) {
		f := newLeaseFixture(t, -time.Minute)
		claim(t, f)
		f.rn.endErr = runner.ErrEndUnsupported
		f.sweep(t)
		if f.st.State() != types.RunStopped || f.rn.stopCount() != 1 {
			t.Fatalf("state %s, StopSandbox %d; want STOPPED, 1", f.st.State(), f.rn.stopCount())
		}
		if f.brk.count(f.run.ID) == 0 || f.idp.count() == 0 {
			t.Errorf("broker revocations %d, identity revocations %d; want both revoked",
				f.brk.count(f.run.ID), f.idp.count())
		}
		ended := f.audit.eventsFor(f.run.ID, "run.ended")
		if len(ended) != 1 || leaseAuditData(t, ended[0])["kept"] != false || leaseAuditData(t, ended[0])["end_error"] == nil {
			t.Errorf("run.ended events = %+v, want one with kept:false and end_error", ended)
		}
	})
	t.Run("a substrate that can keep one", func(t *testing.T) {
		f := newLeaseFixture(t, -time.Minute)
		claim(t, f)
		f.sweep(t)
		f.sweep(t)
		if f.st.State() != types.RunRunning || f.rn.endCount() != 2 || f.rn.stopCount() != 0 {
			t.Fatalf("state %s, EndSandbox %d, StopSandbox %d; want RUNNING, 2, 0",
				f.st.State(), f.rn.endCount(), f.rn.stopCount())
		}
		if f.brk.count(f.run.ID) != 1 {
			t.Errorf("broker revocations = %d, want 1: revoked once, not on every pass", f.brk.count(f.run.ID))
		}
	})
}

// TestRunLease_GraceExpiryTearsDown: a kept run past the ended-run grace is
// stopped and torn down; one inside it is not.
func TestRunLease_GraceExpiryTearsDown(t *testing.T) {
	f := newLeaseFixture(t, -time.Minute)
	f.sweep(t)
	f.now = f.now.Add(7*24*time.Hour - time.Minute)
	f.sweep(t)
	if f.st.State() != types.RunRunning || f.rn.stopCount() != 0 {
		t.Fatalf("inside the grace: state %s, StopSandbox %d; want RUNNING, 0", f.st.State(), f.rn.stopCount())
	}

	f.now = f.now.Add(2 * time.Minute)
	f.sweep(t)
	if f.st.State() != types.RunStopped || f.rn.stopCount() != 1 {
		t.Fatalf("past the grace: state %s, StopSandbox %d; want STOPPED, 1", f.st.State(), f.rn.stopCount())
	}
	if f.idp.count() == 0 {
		t.Error("the grace teardown did not revoke the run identity")
	}
	if len(f.audit.eventsFor(f.run.ID, "run.ended.expired")) != 1 {
		t.Error("no run.ended.expired audit row")
	}
}

// TestRunLease_EndingSoonWarnings: run.ending_soon goes out once per threshold
// per end, only the closest one when a pass finds a run inside several, never
// the 24-hour one on a lease of two days or less, and again after the end moves.
func TestRunLease_EndingSoonWarnings(t *testing.T) {
	warned := func(f *leaseFixture) []float64 {
		var out []float64
		for _, ev := range f.audit.eventsFor(f.run.ID, "run.ending_soon") {
			out = append(out, leaseAuditData(t, ev)["threshold_sec"].(float64))
		}
		return out
	}
	f := newLeaseFixture(t, 30*time.Hour)
	f.sweep(t)
	if got := warned(f); len(got) != 0 {
		t.Fatalf("30 h out: warnings %v, want none", got)
	}
	f.now = f.now.Add(7 * time.Hour) // 23 h left
	f.sweep(t)
	f.sweep(t)
	if got := warned(f); len(got) != 1 || got[0] != 86400 {
		t.Fatalf("23 h out: warnings %v, want one at 86400", got)
	}
	f.now = f.now.Add(22*time.Hour + 55*time.Minute) // 5 min left: inside both 1 h and 10 min
	f.sweep(t)
	f.sweep(t)
	if got := warned(f); len(got) != 2 || got[1] != 600 {
		t.Fatalf("5 min out: warnings %v, want the 10-minute one only, once", got)
	}

	f.st.setEnd(f.now.Add(50 * time.Minute)) // extended: the warnings re-arm
	f.sweep(t)
	if got := warned(f); len(got) != 3 || got[2] != 3600 {
		t.Fatalf("after the end moved: warnings %v, want a fresh 1-hour one", got)
	}

	short := newLeaseFixture(t, 20*time.Hour)
	short.st.run.CreatedAt = short.now.Add(-4 * time.Hour) // a 24 h lease
	short.sweep(t)
	if got := warned(short); len(got) != 0 {
		t.Errorf("24 h lease, 20 h out: warnings %v, want none", got)
	}
}

// TestCompletionWatcher_LeavesAKeptRunAlone: ending a run stops its agent,
// which returns the completion watcher's Wait. The watcher must see the run is
// kept and not finalize it — finalizing tears down the files it is kept for.
func TestCompletionWatcher_LeavesAKeptRunAlone(t *testing.T) {
	f := newLeaseFixture(t, -time.Minute)
	f.sweep(t) // kept

	f.srv.startCompletionWatcher(f.run.ID, f.run.SandboxRef, "exec-lease")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if f.st.State() != types.RunRunning || f.rn.stopCount() != 0 {
			t.Fatalf("the watcher finalized a kept run: state %s, StopSandbox %d", f.st.State(), f.rn.stopCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestReconcileFinalize_LeavesAKeptRunAlone is the reconciler's side of the
// same rule: an adopted watcher probing the stopped agent must not finalize.
func TestReconcileFinalize_LeavesAKeptRunAlone(t *testing.T) {
	f := newLeaseFixture(t, -time.Minute)
	f.sweep(t)
	f.srv.reconcileFinalize(context.Background(), f.run.ID, types.RunFailed, f.run.SandboxRef, "reconciled exit")
	if f.st.State() != types.RunRunning || f.rn.stopCount() != 0 {
		t.Errorf("state %s, StopSandbox %d; want the kept run left alone", f.st.State(), f.rn.stopCount())
	}
}
