// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net/netip"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type resumeObservedSubstrate struct {
	*runnertest.Fake
	observed chan struct{}
}

func (s *resumeObservedSubstrate) Status(ctx context.Context, ref string) (runner.Status, error) {
	status, err := s.Fake.Status(ctx, ref)
	select {
	case s.observed <- struct{}{}:
	case <-ctx.Done():
	}
	return status, err
}

type resumeObservedConn struct {
	runnerwire.Conn
	lose   atomic.Bool
	frames chan runnerwire.Frame
}

func (c *resumeObservedConn) WriteFrame(ctx context.Context, f runnerwire.Frame) error {
	if f.Type == runnerwire.TypeEvent {
		if c.lose.CompareAndSwap(true, false) {
			_ = c.Conn.Close()
			return runnerwire.ErrLinkDown
		}
		if c.frames != nil {
			select {
			case c.frames <- f:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return c.Conn.WriteFrame(ctx, f)
}

type resumeRuntimeFixture struct {
	ctx        context.Context
	runtime    runnerRuntime
	client     *runnerwire.Client
	sub        *resumeObservedSubstrate
	remote     *remote.Substrate
	link       *remote.Link
	runnerID   uuid.UUID
	generation uuid.UUID
	stateDir   string
	peers      chan *runnerwire.Peer
	conns      chan *resumeObservedConn
	connected  chan error
	hubDone    chan error
	entered    chan struct{}
	release    chan struct{}
	frames     chan runnerwire.Frame
}

func newResumeRuntimeFixture(t *testing.T) *resumeRuntimeFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &resumeRuntimeFixture{ctx: ctx, runnerID: uuid.New(), stateDir: t.TempDir(), link: &remote.Link{},
		sub:   &resumeObservedSubstrate{Fake: runnertest.NewFake(), observed: make(chan struct{}, 8)},
		peers: make(chan *runnerwire.Peer, 2), conns: make(chan *resumeObservedConn, 2), connected: make(chan error, 2), hubDone: make(chan error, 2),
		entered: make(chan struct{}), release: make(chan struct{}), frames: make(chan runnerwire.Frame, 8)}
	f.runtime, err = localRunnerRuntime(ctx, runneridentity.Identity{RunnerID: f.runnerID, PrivateKey: key}, f.stateDir, f.sub,
		func(id uuid.UUID) string { return "fake-" + id.String() }, newLocalRelay(ctx, netip.Addr{}), unsized)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.runtime.close() })
	const orgURL = "https://org.example"
	row := types.Runner{ID: f.runnerID, PublicKey: pub, State: types.RunnerClaimed, OrgURLSHA256: federation.OrgURLSHA256(orgURL)}
	f.generation = uuid.New()
	f.remote = remote.New(row.ID.String(), f.link, remote.Options{ExecutionGeneration: func(context.Context, string, string) (uuid.UUID, error) { return f.generation, nil }})
	hub := remote.NewHub(ctx, remote.HubConfig{OrgURL: orgURL,
		Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (remote.Admission, error) {
			return remote.Admission{Substrate: f.remote, Link: f.link,
				Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil }, Event: func(context.Context, runnerwire.Event) error { return nil }}, nil
		},
	})
	t.Cleanup(hub.Close)
	hooks := f.runtime.hooks
	hooks.BeforeReady = func(ctx context.Context, peer *runnerwire.Peer, resumed bool) error {
		if err := f.runtime.hooks.BeforeReady(ctx, peer, resumed); err != nil {
			return err
		}
		if resumed {
			close(f.entered)
			select {
			case <-f.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	hooks.Online = func(peer *runnerwire.Peer, resumed bool) { f.runtime.hooks.Online(peer, resumed); f.peers <- peer }
	connections := 0
	f.client, err = runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: orgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
		Dial: func(context.Context) (runnerwire.Conn, error) {
			org, local := runnerwire.Loopback()
			conn := &resumeObservedConn{Conn: local}
			if connections > 0 {
				conn.frames = f.frames
			}
			connections++
			f.conns <- conn
			go func() { f.hubDone <- hub.Serve(ctx, org, row.ID) }()
			return conn, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cancel)
	return f
}

func resumeAwait[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		var zero T
		return zero
	}
}

func TestLocalRuntimeResumeReplaysRetainedFramesBeforeNewSpool(t *testing.T) {
	f := newResumeRuntimeFixture(t)
	go func() { f.connected <- f.client.Connect(f.ctx) }()
	peer := resumeAwait(t, f.ctx, f.peers)
	for !f.link.Online() {
		select {
		case <-f.ctx.Done():
			t.Fatal(f.ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	run := uuid.New()
	lease, _ := runnerwire.Marshal(runnerwire.Lease{RunID: run, ExecutionGeneration: f.generation, TokenRenewedAt: time.Now(), EndsAt: time.Now().Add(time.Hour)})
	if err := f.link.Send(f.ctx, runnerwire.Frame{Type: runnerwire.TypeLease, Payload: lease}); err != nil {
		t.Fatal(err)
	}
	sandbox, err := f.remote.CreateSandbox(f.ctx, runner.SandboxSpec{RunID: run, RunnerID: f.runnerID.String(), ConfinementClass: types.CC1})
	if err != nil {
		t.Fatal(err)
	}
	// A live execution keeps the run in the periodic lifecycle pass, whose
	// substrate observation is this test's background-iteration signal.
	if _, err := f.remote.Exec(f.ctx, sandbox.Ref, []string{"agent"}); err != nil {
		t.Fatal(err)
	}
	conn := resumeAwait(t, f.ctx, f.conns)
	conn.lose.Store(true)
	if err := peer.Event(f.ctx, runnerwire.Event{Kind: runnerwire.EventPosture}); err == nil {
		t.Fatal("retained event fault did not fire")
	}
	resumeAwait(t, f.ctx, f.connected)
	resumeAwait(t, f.ctx, f.hubDone)
	spool, err := runnerspool.Open(filepath.Join(f.stateDir, "spools", run.String()), run)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	if err := spool.Append(f.ctx, egress.DecisionLog{Request: egress.Request{RunID: run, Time: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	go func() { f.connected <- f.client.Connect(f.ctx) }()
	resumeAwait(t, f.ctx, f.entered)
	runCtx, stop := context.WithCancel(f.ctx)
	stopped := make(chan error, 1)
	go func() { stopped <- f.runtime.run(runCtx) }()
	defer func() { stop(); resumeAwait(t, f.ctx, stopped) }()
	// Two lifecycle observations prove a background iteration completed while
	// resume remains paused before Peer.Start. A premature EventAck blocks that
	// first iteration, and its frame is observed below instead of a second tick.
	resumeAwait(t, f.ctx, f.sub.observed)
	select {
	case frame := <-f.frames:
		t.Fatalf("new event escaped before retained replay and read-loop startup: seq=%d", frame.Seq)
	case <-f.sub.observed:
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	close(f.release)
	resumed := resumeAwait(t, f.ctx, f.peers)
	defer resumed.Close()
	for _, want := range []string{runnerwire.EventPosture, runnerwire.EventSpool} {
		frame := resumeAwait(t, f.ctx, f.frames)
		var event runnerwire.Event
		if err := runnerwire.Unmarshal(frame.Payload, &event); err != nil || event.Kind != want {
			t.Fatalf("resume event=%q want=%q error=%v", event.Kind, want, err)
		}
	}
	for {
		ack, err := spool.Get(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ack.Seq == 1 {
			break
		}
		select {
		case <-f.ctx.Done():
			t.Fatal("new spool row was not durably acknowledged", f.ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	resumed.Close()
	resumeAwait(t, f.ctx, f.connected)
	resumeAwait(t, f.ctx, f.hubDone)
}
