// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The runner_goaway notice: an action queued for a runner on a replica that does not hold its
// session ends that session on the replica that does, so the fresh one reads the action.

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/livebus"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func runnerGoawayMessage(id uuid.UUID, at time.Time) livebus.Message {
	m := livebus.Message{Kind: livebus.KindRunnerGoaway, Run: id, Origin: "replica-b"}
	if !at.IsZero() {
		m.Data = mustJSON(runnerGoawayNotice{At: at})
	}
	return m
}

// A replica that never held the runner's session does nothing: it does not even build a hub. A
// notice without a time is ignored, and announcing with no bus sends nothing.
func TestRunnerGoawayNoticeIsANoOpWithoutASession(t *testing.T) {
	srv := newHarness(t).srv
	id := uuid.New()
	srv.onRunnerGoawayNotice(runnerGoawayMessage(id, srv.cfg.Now()))
	srv.WaitBackground()
	if srv.runnerStreams.hub != nil {
		t.Fatal("a notice for a runner this replica never served built a hub")
	}

	srv.noteRunnerSession(id, "s1")
	srv.onRunnerGoawayNotice(runnerGoawayMessage(id, time.Time{}))
	srv.onRunnerGoawayNotice(livebus.Message{Kind: livebus.KindRunnerGoaway, Run: id, Data: []byte("{")})
	srv.onRunnerGoawayNotice(runnerGoawayMessage(id, srv.cfg.Now().Add(-time.Hour))) // began after it was sent
	srv.WaitBackground()
	if srv.runnerStreams.hub != nil {
		t.Fatal("a notice that is malformed, untimed or older than the session reached the hub")
	}

	srv.announceRunnerGoaway(id) // no bus: nothing to send, nothing to panic on
	srv.WaitBackground()
}

func TestRunnerSessionBeganIsWhenTheLatestFreshSessionBegan(t *testing.T) {
	srv := newHarness(t).srv
	id := uuid.New()
	if _, _, ok := srv.runnerSessionBegan(id); ok {
		t.Fatal("a runner with no session has a start time")
	}
	before := srv.cfg.Now()
	srv.noteRunnerSession(id, "s1")
	session, got, ok := srv.runnerSessionBegan(id)
	if !ok || session != "s1" || got.Before(before) || got.After(srv.cfg.Now()) {
		t.Fatalf("began = %q %v, %v; want s1 at noteRunnerSession", session, got, ok)
	}
}

// Two Servers on one database are two wardynds. The runner's session is on A; a kill served by B
// finds the runner offline and queues. B's notice ends A's session within one notice. Once the fresh
// session applied the kill, re-asserting it ends nothing. A notice older than a session that began
// after it does not.
func TestRunnerGoawayPG_AKillQueuedOnBEndsTheSessionOnA(t *testing.T) {
	f := newAdmissionFixture(t)
	a := f.srv
	a.cfg.LiveBus = livebus.New(f.pool, "replica-a")
	a.registerLiveBus()
	probed := make(chan struct{}, 1)
	a.cfg.LiveBus.Handle("probe", func(livebus.Message) {
		select {
		case probed <- struct{}{}:
		default:
		}
	})
	a.cfg.LiveBus.Start(f.ctx)
	cfgB := a.cfg
	cfgB.LiveBus = livebus.New(f.pool, "replica-b")
	b := New(cfgB)
	cfgB.LiveBus.Start(f.ctx)
	// The listeners connect asynchronously: publish until A has heard B.
	haWait(t, 10*time.Second, "A's listener", func() bool {
		if err := cfgB.LiveBus.Publish(f.ctx, "probe", uuid.Nil, nil); err != nil {
			t.Fatal(err)
		}
		select {
		case <-probed:
			return true
		case <-time.After(100 * time.Millisecond):
			return false
		}
	})

	first := f.connect(t, nil, f.liveRow())
	if !f.waitReady(first) {
		t.Fatalf("the session on A never reached READY: %v", first.result())
	}
	if err := b.runnerEndpoint(f.rn.ID).sub.KillSandbox(f.ctx, f.run.SandboxRef); !errors.Is(err, runner.ErrPendingOnRunner) {
		t.Fatalf("B's kill = %v, want queued: B holds no session", err)
	}
	select {
	case <-first.done:
		if !errors.Is(first.err, runnerwire.ErrGoaway) {
			t.Fatalf("A's session ended with %v, want a GOAWAY", first.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a kill queued on B did not end the runner's session on A")
	}

	// The fresh session began after the notice that is now stale: it is left alone. One sent after it ends it.
	second := f.connect(t, nil, f.liveRow())
	if !f.waitReady(second) {
		t.Fatalf("the fresh session never reached READY: %v", second.result())
	}
	// The fresh session applied the kill. Re-asserting it, as the lease sweep does every pass on
	// every replica, queues nothing and announces nothing: the session stays up.
	haWait(t, 10*time.Second, "the kill applied", func() bool {
		return countRows(t, f, `SELECT count(*) FROM runner_pending_actions WHERE run_id=$1 AND kind='kill' AND outcome='success'`) == 1
	})
	for range 3 {
		if err := b.queueRunnerTeardown(f.ctx, f.rn.ID, types.RunnerActionKill, f.run.SandboxRef); err != nil {
			t.Fatalf("re-asserting the applied kill = %v", err)
		}
	}
	select {
	case <-second.done:
		t.Fatalf("re-asserting an applied kill ended the fresh session: %v", second.err)
	case <-time.After(300 * time.Millisecond):
	}
	if n := countRows(t, f, `SELECT count(*) FROM runner_pending_actions WHERE run_id=$1 AND kind='kill'`); n != 1 {
		t.Fatalf("%d kill rows after the re-asserts, want the one applied", n)
	}

	_, began, ok := a.runnerSessionBegan(f.rn.ID)
	if !ok {
		t.Fatal("A noted no fresh session")
	}
	a.onRunnerGoawayNotice(runnerGoawayMessage(f.rn.ID, began.Add(-time.Second)))
	select {
	case <-second.done:
		t.Fatalf("a notice older than the session ended it: %v", second.err)
	case <-time.After(300 * time.Millisecond):
	}
	a.onRunnerGoawayNotice(runnerGoawayMessage(f.rn.ID, a.cfg.Now()))
	select {
	case <-second.done:
		if !errors.Is(second.err, runnerwire.ErrGoaway) {
			t.Fatalf("the session ended with %v, want a GOAWAY", second.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a notice newer than the session did not end it")
	}
}

func countRows(t *testing.T, f *admissionFixture, q string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, q, f.run.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
