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

// A due end held off for keptEndHoldOffLimit is stopped without the lock: the proxy first, then the sandbox,
// with one failure row and no terminal state or pass back. A later pass still held off adds nothing.
func TestStopKeptRun_AnEndHeldOffPastTheLimitStopsTheSandbox(t *testing.T) {
	s, cs, audit, run, sb, leaser, hold := keptEndFixture(t)
	defer hold()
	now := time.Now()
	s.cfg.Now = func() time.Time { return now }
	sb.proxyStopped.Store(false)
	var proxyFirst, passed bool
	sb.stopped = func() { proxyFirst = sb.proxyStopped.Load() }
	sb.onPass = func() { passed = true }
	end := func() {
		s.stopKeptRun(context.Background(), leaser, run, types.RunStopped, "run.ended.expired", map[string]any{"ended_at": run.LostAt})
	}
	start := now
	for ; ; now = now.Add(time.Minute) {
		end()
		if len(expiredRows(audit)) > 0 {
			break
		}
		if sb.stopCount() != 0 || sb.proxyStopped.Load() {
			t.Fatal("the sandbox or its proxy was stopped before the hold-off limit")
		}
		if now.Sub(start) > keptEndHoldOffLimit+time.Minute {
			t.Fatal("no forced stop")
		}
	}
	rows := expiredRows(audit)
	var data struct {
		HeldOff int64 `json:"lock_held_off_sec"`
		Stopped bool  `json:"sandbox_stopped"`
	}
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if rows[0].Outcome != "failure" || data.HeldOff != int64(keptEndHoldOffLimit.Seconds()) || !data.Stopped {
		t.Errorf("row %s %s; want failure, lock_held_off_sec %d, sandbox_stopped", rows[0].Outcome, rows[0].Data, int64(keptEndHoldOffLimit.Seconds()))
	}
	if !proxyFirst || sb.stopCount() != 1 {
		t.Errorf("proxy stopped before the sandbox %v, sandbox stops %d; want true and 1", proxyFirst, sb.stopCount())
	}
	if st, _ := cs.GetRun(context.Background(), run.ID); st.State != types.RunRunning || leaser.calls != 0 || passed {
		t.Errorf("state %s, %d transitions, pass back %v; want RUNNING, none, none", st.State, leaser.calls, passed)
	}
	now = now.Add(time.Minute)
	end()
	if n := len(expiredRows(audit)); n != 1 || sb.stopCount() != 1 {
		t.Errorf("the next held-off pass: %d rows, %d stops; want 1 and 1", n, sb.stopCount())
	}
}
