// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runneridentity"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type resumeObservedSubstrate struct {
	*runnertest.Fake
	observed chan struct{}
	// classesDown makes Classes fail, as `docker info` does while Docker is down.
	classesDown atomic.Bool
}

func (s *resumeObservedSubstrate) Classes(ctx context.Context) (substrate.ClassSupport, error) {
	if s.classesDown.Load() {
		return substrate.ClassSupport{}, errors.New("docker: Cannot connect to the Docker daemon")
	}
	return s.Fake.Classes(ctx)
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
	// afterReplay, when set, runs on every session once the runtime's own BeforeReady is done (after Replay,
	// before the runner is online).
	afterReplay func()
	release     chan struct{}
	frames      chan runnerwire.Frame
}

func newResumeRuntimeFixture(t *testing.T) *resumeRuntimeFixture {
	t.Helper()
	return newResumeRuntimeFixtureWith(t, runtimeExtras{}, false)
}

// newResumeRuntimeFixtureWith is the fixture with the runtime's extras, and with the frames of every
// connection (not only the later ones) on f.frames.
func newResumeRuntimeFixtureWith(t *testing.T, extras runtimeExtras, framesFromFirst bool) *resumeRuntimeFixture {
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
	f.runtime, err = runnerRuntimeWith(ctx, runneridentity.Identity{RunnerID: f.runnerID, PrivateKey: key}, f.stateDir, f.sub,
		func(id uuid.UUID) string { return "fake-" + id.String() }, newLocalRelay(ctx), unsized, extras)
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
		if f.afterReplay != nil {
			f.afterReplay()
		}
		if resumed {
			select {
			case <-f.entered: // an earlier resume already announced itself
			default:
				close(f.entered)
			}
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
			if connections > 0 || framesFromFirst {
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

// capsStream reads every frame the runner writes and passes on the caps events, so a frame nobody waits for
// never blocks the runner.
func (f *resumeRuntimeFixture) capsStream(t *testing.T) <-chan runnerwire.Caps {
	t.Helper()
	out := make(chan runnerwire.Caps, 16)
	go func() {
		for {
			select {
			case frame := <-f.frames:
				var event runnerwire.Event
				if runnerwire.Unmarshal(frame.Payload, &event) != nil || event.Kind != runnerwire.EventCaps {
					continue
				}
				var caps runnerwire.Caps
				if runnerwire.Unmarshal(event.Data, &caps) == nil {
					out <- caps
				}
			case <-f.ctx.Done():
				return
			}
		}
	}()
	return out
}

// session connects (fresh or resumed, as the client decides) and waits for the runner to be online.
func (f *resumeRuntimeFixture) session(t *testing.T) (*resumeObservedConn, *runnerwire.Peer) {
	t.Helper()
	go func() { f.connected <- f.client.Connect(f.ctx) }()
	conn := resumeAwait(t, f.ctx, f.conns)
	return conn, resumeAwait(t, f.ctx, f.peers)
}

// drop loses the session's link and waits for the session to end.
func (f *resumeRuntimeFixture) drop(t *testing.T, conn *resumeObservedConn, peer *runnerwire.Peer) {
	t.Helper()
	conn.lose.Store(true)
	if err := peer.Event(f.ctx, runnerwire.Event{Kind: runnerwire.EventPosture}); err == nil {
		t.Fatal("event fault did not fire")
	}
	resumeAwait(t, f.ctx, f.connected)
	resumeAwait(t, f.ctx, f.hubDone)
}

// A macOS VM change made while the link is down reaches the organisation as the first caps of the resumed
// session; a resume with no change sends none, as before.
func TestResumeSendsCapsChangedWhileOffline(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	m.recompute()
	f := newResumeRuntimeFixtureWith(t, runtimeExtras{macOSVM: m}, true)
	close(f.release)
	capsSeen := f.capsStream(t)
	runCtx, stop := context.WithCancel(f.ctx)
	stopped := make(chan error, 1)
	go func() { stopped <- f.runtime.run(runCtx) }()
	defer func() { stop(); resumeAwait(t, f.ctx, stopped) }()

	conn, peer := f.session(t)
	if caps := resumeAwait(t, f.ctx, capsSeen); caps.MacOSVM == nil || caps.MacOSVM.Base.GuestOS != "macOS 26.1" {
		t.Fatalf("a fresh session's caps: %+v", caps.MacOSVM)
	}
	f.drop(t, conn, peer)

	// The base changes while the link is down. Three kicks are a barrier: the run loop takes the third only
	// after the iteration for the first, with its resend, is done.
	man := `{"format":1,"state":"ready","guest_os":"macOS 26.2","agent_version":"1.0.0","min_cpus":4,"min_memory_mib":8192,"desktop":"control","harnesses":["claude-code"]}`
	if err := os.WriteFile(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile), []byte(man), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		m.kick <- struct{}{}
	}
	if got := m.current().Base.GuestOS; got != "macOS 26.2" {
		t.Fatalf("the rig did not change: %q", got)
	}
	conn, peer = f.session(t)
	select {
	case caps := <-capsSeen:
		if caps.MacOSVM == nil || caps.MacOSVM.Base.GuestOS != "macOS 26.2" {
			t.Fatalf("the first caps after the resume: %+v", caps.MacOSVM)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the resumed session sent no caps for a change made while offline")
	}

	// The organisation has taken the caps once an event after them is acknowledged; a send cut off by the
	// link dropping would count as failed, and be sent again.
	if err := peer.EventAck(f.ctx, runnerwire.Event{Kind: runnerwire.EventPosture}); err != nil {
		t.Fatal(err)
	}
	// No change while offline: the next resume sends no caps.
	f.drop(t, conn, peer)
	f.session(t)
	select {
	case caps := <-capsSeen:
		t.Fatalf("a resume with no change sent caps: %+v", caps.MacOSVM)
	case <-time.After(500 * time.Millisecond):
	}
}

// A macOS VM change made between a fresh session's caps (Replay) and the runner going online has no peer to
// be resent to; the caps go once the runner is online.
func TestFreshSessionSendsCapsChangedBeforeOnline(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	m.recompute()
	f := newResumeRuntimeFixtureWith(t, runtimeExtras{macOSVM: m}, true)
	close(f.release)
	var once sync.Once
	f.afterReplay = func() {
		once.Do(func() {
			man := `{"format":1,"state":"ready","guest_os":"macOS 26.2","agent_version":"1.0.0","min_cpus":4,"min_memory_mib":8192,"desktop":"control","harnesses":["claude-code"]}`
			if err := os.WriteFile(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile), []byte(man), 0o600); err != nil {
				t.Error(err)
			}
			for range 3 {
				m.kick <- struct{}{}
			}
		})
	}
	capsSeen := f.capsStream(t)
	runCtx, stop := context.WithCancel(f.ctx)
	stopped := make(chan error, 1)
	go func() { stopped <- f.runtime.run(runCtx) }()
	defer func() { stop(); resumeAwait(t, f.ctx, stopped) }()

	f.session(t)
	if caps := resumeAwait(t, f.ctx, capsSeen); caps.MacOSVM == nil || caps.MacOSVM.Base.GuestOS != "macOS 26.1" {
		t.Fatalf("the fresh session's caps: %+v", caps.MacOSVM)
	}
	select {
	case caps := <-capsSeen:
		if caps.MacOSVM == nil || caps.MacOSVM.Base.GuestOS != "macOS 26.2" {
			t.Fatalf("the caps after the change: %+v", caps.MacOSVM)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a macOS VM change made before the runner was online was never sent")
	}
}

// While Docker is down no caps can be built, but a macOS VM change still reaches the organisation: the
// caps it last acked go again with only macos_vm replaced. A fresh session while Docker is down still
// fails, as before.
func TestMacOSCapsFallbackSendsLastCapsWithTheNewMacOSVM(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	m.recompute()
	f := newResumeRuntimeFixtureWith(t, runtimeExtras{macOSVM: m}, true)
	close(f.release)
	capsSeen := f.capsStream(t)
	runCtx, stop := context.WithCancel(f.ctx)
	stopped := make(chan error, 1)
	go func() { stopped <- f.runtime.run(runCtx) }()
	defer func() { stop(); resumeAwait(t, f.ctx, stopped) }()

	_, peer := f.session(t)
	first := resumeAwait(t, f.ctx, capsSeen)
	if err := peer.EventAck(f.ctx, runnerwire.Event{Kind: runnerwire.EventPosture}); err != nil {
		t.Fatal(err)
	}
	f.sub.classesDown.Store(true)
	man := `{"format":1,"state":"ready","guest_os":"macOS 26.3","agent_version":"1.0.0","min_cpus":4,"min_memory_mib":8192,"desktop":"control","harnesses":["claude-code"]}`
	if err := os.WriteFile(filepath.Join(m.layout.BaseDir(0), macvm.ManifestFile), []byte(man), 0o600); err != nil {
		t.Fatal(err)
	}
	m.kick <- struct{}{}
	var got runnerwire.Caps
	select {
	case got = <-capsSeen:
	case <-time.After(5 * time.Second):
		t.Fatal("no caps were sent for a macOS VM change while Docker is down")
	}
	if got.MacOSVM == nil || got.MacOSVM.Base.GuestOS != "macOS 26.3" {
		t.Fatalf("fallback caps: %+v", got.MacOSVM)
	}
	got.MacOSVM, first.MacOSVM = nil, nil
	a, _ := runnerwire.Marshal(first)
	b, _ := runnerwire.Marshal(got)
	if string(a) != string(b) {
		t.Fatalf("the fallback caps differ from the last acked caps beyond macos_vm:\n%s\n%s", a, b)
	}
}

// A fresh session while Docker is down fails, as before: Replay needs the host's caps.
func TestMacOSCapsFallbackLeavesReplayFailing(t *testing.T) {
	r := newMacLedgerRig(t)
	m := r.open(t)
	m.recompute()
	f := newResumeRuntimeFixtureWith(t, runtimeExtras{macOSVM: m}, true)
	f.sub.classesDown.Store(true)
	go func() { f.connected <- f.client.Connect(f.ctx) }()
	resumeAwait(t, f.ctx, f.conns)
	if err := resumeAwait(t, f.ctx, f.connected); err == nil || !errors.Is(err, errHostCaps) && !strings.Contains(err.Error(), "Docker daemon") {
		t.Fatalf("a fresh session with Docker down: %v", err)
	}
}

// resendMacOSCaps sends nothing when no caps were ever acked, and with no session only marks the caps unsent.
func TestMacOSCapsFallbackNeedsLastCaps(t *testing.T) {
	var dirty atomic.Bool
	var last atomic.Pointer[runnerwire.Caps]
	acked := 0
	send := func(context.Context, *runnerwire.Peer, bool, bool) error { return fmt.Errorf("%w: down", errHostCaps) }
	ack := func(context.Context, *runnerwire.Peer, runnerwire.Caps) error { acked++; return nil }
	cur := func() *runnerwire.MacVMCaps { return &runnerwire.MacVMCaps{Max: 1} }
	resendMacOSCaps(t.Context(), nil, &dirty, &last, send, ack, cur)
	if !dirty.Load() || acked != 0 {
		t.Fatalf("no session: dirty %v, acked %d", dirty.Load(), acked)
	}
	peer := new(runnerwire.Peer)
	resendMacOSCaps(t.Context(), peer, &dirty, &last, send, ack, cur)
	if acked != 0 {
		t.Fatalf("sent %d caps with none acked before", acked)
	}
	last.Store(&runnerwire.Caps{Arch: "arm64"})
	resendMacOSCaps(t.Context(), peer, &dirty, &last, send, ack, cur)
	if acked != 1 {
		t.Fatalf("acked %d, want 1", acked)
	}
	// Another send error is no reason to fall back.
	send = func(context.Context, *runnerwire.Peer, bool, bool) error { return errors.New("link down") }
	resendMacOSCaps(t.Context(), peer, &dirty, &last, send, ack, cur)
	if acked != 1 {
		t.Fatalf("fell back on a link error: acked %d", acked)
	}
}
