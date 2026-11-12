// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// idleTestRun is a run whose engine generation has had nothing open for far longer than its idle timeout,
// driven by hand: no watcher, no clock waits.
func idleTestRun(t *testing.T) (*viaOrgRun, *viaOrgGen) {
	t.Helper()
	run := &viaOrgRun{id: uuid.New(), runner: uuid.New(), bytes: new(atomic.Int64), done: make(chan struct{}),
		streams: map[*viaOrgStream]struct{}{}, idleSince: time.Now().Add(-time.Hour),
		byteCap: viaOrgMaxBytes, idle: time.Minute, period: time.Hour}
	run.engine = newIdleTestGen(t, run.id)
	return run, run.engine
}

func newIdleTestGen(t *testing.T, runID uuid.UUID) *viaOrgGen {
	t.Helper()
	eng, err := proxy.NewViaOrgEngine(context.Background(),
		&proxy.Config{RunID: runID, ControlPlaneURL: "http://127.0.0.1:9", RunToken: "run-token", DecisionBufferSize: 16},
		proxy.ViaOrgEngineOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return &viaOrgGen{engine: eng}
}

// The idle tick never cuts a stream that is open or about to be admitted. While a stream is open the run is
// not idle and its engine stays; once the run is idle the tick retires the engine in the same step as its
// check, so a request that arrives after the check finds the generation retired (and takes a fresh one)
// instead of being admitted to an engine that is then released under it.
func TestViaOrg_IdleTickNeverCutsAnOpenStream(t *testing.T) {
	ctx := context.Background()
	run, gen := idleTestRun(t)

	// An open stream keeps the run, however stale its idle stamp.
	st, refused, retired := run.open(ctx, gen)
	if st == nil || refused != "" || retired {
		t.Fatalf("open = %v %q retired=%v, want a stream", st, refused, retired)
	}
	if got := run.closeIdle(time.Now()); got != nil {
		t.Fatal("an idle tick retired an engine with a stream open")
	}
	if run.engine != gen {
		t.Fatal("an idle tick took the engine of a run with a stream open")
	}
	if st.ctx.Err() != nil {
		t.Fatal("an idle tick cut a stream that was not idle")
	}

	// Once the last stream leaves the run is idle only after the whole timeout.
	run.release(st)
	if got := run.closeIdle(time.Now()); got != nil {
		t.Fatal("an idle tick retired an engine that had just served a stream")
	}

	// Past the timeout the tick retires the engine, and a request admitted after that check is not given it.
	late := time.Now().Add(2 * time.Minute)
	if got := run.closeIdle(late); got != gen {
		t.Fatalf("the idle tick returned %v, want the run's engine generation", got)
	}
	if run.engine != nil {
		t.Fatal("the idle tick left the retired generation in place")
	}
	st, refused, retired = run.open(ctx, gen)
	if st != nil || refused != "" || !retired {
		t.Fatalf("open after the idle check = %v %q retired=%v, want the generation retired", st, refused, retired)
	}
	closeViaOrgGen(run.id, gen)
	if !gen.engine.Closed() {
		t.Fatal("the retired generation's engine was not closed")
	}

	// A fresh generation is not retired by the same stale check: only its own quiet time counts.
	run.engine = newIdleTestGen(t, run.id)
	run.smu.Lock()
	run.idleSince = time.Now()
	run.smu.Unlock()
	if got := run.closeIdle(time.Now()); got != nil {
		t.Fatal("an idle tick retired a generation that was just built")
	}
	fresh := run.closeIdle(late)
	if fresh == nil {
		t.Fatal("an idle generation was not retired past the timeout")
	}
	closeViaOrgGen(run.id, fresh)
}

// The watcher's idle arm retires a run's idle engine on its own tick, keeps the run and its byte total, and
// does not end it: the run is still there for its next request.
func TestViaOrg_WatcherReleasesAnIdleEngineAndKeepsTheRun(t *testing.T) {
	runner := uuid.New()
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	s := &Server{cfg: Config{Store: &flakyRunStore{runner: runner, fail: func(int32) bool { return false }}, BaseCtx: ctx},
		bgWaitBudget: 10 * time.Second}
	run, gen := idleTestRun(t)
	run.runner = runner
	run.period = 2 * time.Millisecond
	run.bytes.Store(7)
	go s.watchViaOrgRun(run)
	defer func() { s.releaseViaOrgRun(run, true, false) }()

	// Wait on the condition, with a deadline of seconds so a slow tick under the race detector stays green.
	released := func() bool {
		run.mu.Lock()
		defer run.mu.Unlock()
		return run.engine == nil && gen.engine.Closed()
	}
	deadline := time.After(30 * time.Second)
	for poll := time.NewTicker(2 * time.Millisecond); !released(); {
		select {
		case <-poll.C:
		case <-deadline:
			t.Fatal("the watcher did not release the idle engine")
		}
	}
	if run.isClosed() {
		t.Fatal("the idle arm ended the run instead of releasing its engine")
	}
	if got := run.bytes.Load(); got != 7 {
		t.Fatalf("the run's byte total = %d after an idle release, want 7", got)
	}
}
