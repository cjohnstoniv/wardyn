// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerio

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

type localRig struct {
	server *Server
	sub    *remote.Substrate
	fake   *runnertest.Fake
	rp     *runnerwire.Peer
}

// localAttachRig connects an org Substrate (with Caps.LocalAttach) to a runnerio Server over a
// loopback link.
func localAttachRig(t *testing.T, times ...func(*localTimes)) *localRig {
	t.Helper()
	fake := runnertest.NewFake()
	server, err := New(contextSubstrate{fake}, "laptop", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range times {
		f(&server.times)
	}
	org, run := runnerwire.Loopback()
	link := &remote.Link{}
	sub := remote.New("laptop", link, remote.Options{})
	op := link.Attach(t.Context(), org)
	var rp *runnerwire.Peer
	rp = runnerwire.NewPeer(run, runnerwire.PeerConfig{OnCall: func(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error) {
		return server.Call(ctx, rp, id, method, args)
	}})
	go rp.Run(t.Context())
	server.HoldSandboxes(func(string) bool { return true })
	server.Online(rp)
	caps, _ := json.Marshal(runnerwire.Caps{Version: "test", LocalAttach: true})
	sub.HandleEvent(runnerwire.Event{Kind: runnerwire.EventCaps, Data: caps})
	t.Cleanup(func() { rp.Close(); op.Close(); server.Offline(rp) })
	return &localRig{server: server, sub: sub, fake: fake, rp: rp}
}

// run creates a sandbox for a new run and binds its output, as a create with exec output does.
func (r *localRig) run(t *testing.T) (uuid.UUID, string) {
	t.Helper()
	id := uuid.New()
	sb, err := r.fake.CreateSandbox(t.Context(), runner.SandboxSpec{RunID: id})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.server.Output(id); err != nil {
		t.Fatal(err)
	}
	if err := r.server.BindOutput(id, sb.Ref); err != nil {
		t.Fatal(err)
	}
	return id, placement.RunnerRefPrefix("laptop") + sb.Ref
}

type localCLI struct {
	conn   net.Conn
	frames chan localFrame
}

func dialLocal(t *testing.T, s *Server) *localCLI {
	t.Helper()
	a, b := net.Pipe()
	go s.ServeLocal(b)
	c := &localCLI{conn: a, frames: make(chan localFrame, 4096)}
	go func() {
		defer close(c.frames)
		for {
			typ, p, err := ReadLocalFrame(a)
			if err != nil {
				return
			}
			c.frames <- localFrame{typ, p}
		}
	}()
	t.Cleanup(func() { _ = a.Close() })
	return c
}

func (c *localCLI) send(t *testing.T, typ byte, v any) {
	t.Helper()
	var err error
	if b, ok := v.([]byte); ok {
		err = WriteLocalFrame(c.conn, typ, b)
	} else {
		err = WriteLocalJSON(c.conn, typ, v)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// next is the next frame within d; ok is false on timeout or a closed connection.
func (c *localCLI) next(d time.Duration) (localFrame, bool) {
	select {
	case f, ok := <-c.frames:
		return f, ok
	case <-time.After(d):
		return localFrame{}, false
	}
}

func (c *localCLI) expect(t *testing.T, typ byte) localFrame {
	t.Helper()
	f, ok := c.next(5 * time.Second)
	if !ok || f.typ != typ {
		t.Fatalf("frame = %q %s (ok %v), want %q", f.typ, f.b, ok, typ)
	}
	return f
}

func (c *localCLI) hello(t *testing.T, run uuid.UUID) string {
	t.Helper()
	c.send(t, LocalHello, LocalHelloMsg{V: LocalProtocol, RunID: run})
	var ok LocalHelloOKMsg
	if err := json.Unmarshal(c.expect(t, LocalHelloOK).b, &ok); err != nil || len(ok.Nonce) < 40 || ok.RunnerID != "laptop" {
		t.Fatalf("hello_ok = %+v, %v", ok, err)
	}
	return ok.Nonce
}

// data reads LocalData frames until want bytes arrived or d passed with none.
func (c *localCLI) data(want int, d time.Duration) []byte {
	var got []byte
	for len(got) < want {
		f, ok := c.next(d)
		if !ok || f.typ != LocalData {
			break
		}
		got = append(got, f.b...)
	}
	return got
}

func (c *localCLI) expectEnd(t *testing.T, d time.Duration, reason string) {
	t.Helper()
	for {
		f, ok := c.next(d)
		if !ok {
			t.Fatalf("no end frame within %v", d)
		}
		if f.typ == LocalData {
			continue
		}
		var r LocalReasonMsg
		_ = json.Unmarshal(f.b, &r)
		if f.typ != LocalEnd || (reason != "" && r.Reason != reason) {
			t.Fatalf("frame %q %+v, want end %q", f.typ, r, reason)
		}
		return
	}
}

func ctx5(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func readTee(t *testing.T, ls runner.LocalSession, n int) []byte {
	t.Helper()
	got := make([]byte, 0, n)
	buf := make([]byte, 64<<10)
	for len(got) < n {
		k, err := ls.Read(buf)
		if err != nil {
			t.Fatalf("tee read after %d bytes: %v", len(got), err)
		}
		got = append(got, buf[:k]...)
	}
	return got
}

func TestLocalAttachTeesThenShowsAndEchoes(t *testing.T) {
	r := localAttachRig(t)
	id, ref := r.run(t)
	cli := dialLocal(t, r.server)
	nonce := cli.hello(t, id)
	ls, err := r.sub.LocalAttach(ctx5(t), ref, runner.LocalAttachRequest{Nonce: nonce, Tee: true, Options: runner.AttachOptions{Cols: 80, Rows: 24}})
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	var bound LocalBoundMsg
	_ = json.Unmarshal(cli.expect(t, LocalBound).b, &bound)
	if bound.ReadOnly || bound.Cols != 80 {
		t.Fatalf("bound = %+v", bound)
	}
	cli.send(t, LocalData, []byte("hello"))
	if got := readTee(t, ls, 5); string(got) != "hello" {
		t.Fatalf("tee = %q", got)
	}
	if err := ls.AckTee(ctx5(t), 5); err != nil {
		t.Fatal(err)
	}
	if got := cli.data(5, 5*time.Second); string(got) != "hello" {
		t.Fatalf("shown = %q", got)
	}
}

// With no window, nothing reaches the CLI until the org has the bytes and acknowledges them.
func TestLocalAttachTeeBeforeShow(t *testing.T) {
	r := localAttachRig(t, func(lt *localTimes) { lt.teeWindow = 0 })
	id, ref := r.run(t)
	cli := dialLocal(t, r.server)
	ls, err := r.sub.LocalAttach(ctx5(t), ref, runner.LocalAttachRequest{Nonce: cli.hello(t, id), Tee: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	cli.expect(t, LocalBound)
	cli.send(t, LocalData, []byte("abc"))
	if got := readTee(t, ls, 3); string(got) != "abc" {
		t.Fatalf("tee = %q", got)
	}
	if got := cli.data(1, 300*time.Millisecond); len(got) != 0 {
		t.Fatalf("the CLI was shown %q before the org acknowledged it", got)
	}
	if err := ls.AckTee(ctx5(t), 3); err != nil {
		t.Fatal(err)
	}
	if got := cli.data(3, 5*time.Second); string(got) != "abc" {
		t.Fatalf("shown after the ack = %q", got)
	}
}

// No acknowledgement within T of the oldest unacknowledged byte being sent ends the session; until
// then the CLI is shown at most the window past the last acknowledgement.
func TestLocalAttachStallsOnlyWhenOldestUnackedByteExceedsT(t *testing.T) {
	r := localAttachRig(t, func(lt *localTimes) { lt.teeWindow, lt.teeTimeout = 8, 400*time.Millisecond })
	id, ref := r.run(t)
	cli := dialLocal(t, r.server)
	ls, err := r.sub.LocalAttach(ctx5(t), ref, runner.LocalAttachRequest{Nonce: cli.hello(t, id), Tee: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	cli.expect(t, LocalBound)
	cli.send(t, LocalData, []byte("0123456789abcdef"))
	readTee(t, ls, 16)
	// A partial acknowledgement leaves the oldest unacknowledged byte in the same chunk.
	if err := ls.AckTee(ctx5(t), 2); err != nil {
		t.Fatal(err)
	}
	if got := cli.data(10, 200*time.Millisecond); string(got) != "0123456789" {
		t.Fatalf("shown with the window full = %q, want the 8-byte window past the ack of 2", got)
	}
	cli.expectEnd(t, 2*time.Second, LocalReasonTeeStalled)
	if _, ok := cli.next(time.Second); ok {
		t.Fatal("the connection stayed open after the stall")
	}
}

// An idle session never stalls: with nothing unacknowledged the T clock is not running.
func TestLocalAttachIdleSessionNeverStalls(t *testing.T) {
	r := localAttachRig(t, func(lt *localTimes) { lt.teeTimeout = 200 * time.Millisecond })
	id, ref := r.run(t)
	cli := dialLocal(t, r.server)
	ls, err := r.sub.LocalAttach(ctx5(t), ref, runner.LocalAttachRequest{Nonce: cli.hello(t, id), Tee: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	cli.expect(t, LocalBound)
	cli.send(t, LocalData, []byte("x"))
	readTee(t, ls, 1)
	if err := ls.AckTee(ctx5(t), 1); err != nil {
		t.Fatal(err)
	}
	if got := cli.data(1, 5*time.Second); string(got) != "x" {
		t.Fatalf("shown = %q", got)
	}
	if f, ok := cli.next(800 * time.Millisecond); ok {
		t.Fatalf("an idle session got %q %s", f.typ, f.b)
	}
	cli.send(t, LocalData, []byte("y"))
	readTee(t, ls, 1)
	if err := ls.AckTee(ctx5(t), 2); err != nil {
		t.Fatal(err)
	}
	if got := cli.data(1, 5*time.Second); string(got) != "y" {
		t.Fatalf("shown after idling = %q", got)
	}
}

// Acknowledgements are cumulative: a lower one is a no-op and never shrinks the window, and one past
// the bytes sent is refused.
func TestLocalAttachStaleAckDoesNotShrinkTheWindow(t *testing.T) {
	r := localAttachRig(t, func(lt *localTimes) { lt.teeWindow = 8 })
	id, ref := r.run(t)
	cli := dialLocal(t, r.server)
	ls, err := r.sub.LocalAttach(ctx5(t), ref, runner.LocalAttachRequest{Nonce: cli.hello(t, id), Tee: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	cli.expect(t, LocalBound)
	cli.send(t, LocalData, []byte("aaaaaaaa"))
	readTee(t, ls, 8)
	if err := ls.AckTee(ctx5(t), 8); err != nil {
		t.Fatal(err)
	}
	if got := cli.data(8, 5*time.Second); string(got) != "aaaaaaaa" {
		t.Fatalf("first shown = %q", got)
	}
	// Straight to the runner: the org's own end already drops a stale acknowledgement.
	r.server.mu.Lock()
	var stream uint32
	for k := range r.server.locals {
		stream = k.id
	}
	r.server.mu.Unlock()
	ack := func(offset int64) error {
		raw, _ := json.Marshal(runnerwire.LocalAttachTeeAckArgs{Stream: stream, Offset: offset})
		return r.server.localTeeAck(r.rp, raw)
	}
	if err := ack(4); err != nil {
		t.Fatalf("a stale acknowledgement was refused: %v", err)
	}
	cli.send(t, LocalData, []byte("bbbbbbbb"))
	readTee(t, ls, 8)
	if got := cli.data(8, 300*time.Millisecond); string(got) != "bbbbbbbb" {
		t.Fatalf("shown after a stale ack = %q, want the whole window", got)
	}
	if err := ack(17); err == nil {
		t.Fatal("an acknowledgement past the bytes sent was accepted")
	}
}

// Steady output above 64 KiB/s with a 300 ms acknowledgement round trip (the org acknowledges the
// latest offset, one acknowledgement in flight) does not end the session.
func TestLocalAttachSteadyOutputWithSlowAcksDoesNotStall(t *testing.T) {
	r := localAttachRig(t)
	id, ref := r.run(t)
	cli := dialLocal(t, r.server)
	ls, err := r.sub.LocalAttach(ctx5(t), ref, runner.LocalAttachRequest{Nonce: cli.hello(t, id), Tee: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	cli.expect(t, LocalBound)
	var read atomic.Int64
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := ls.Read(buf)
			read.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(300 * time.Millisecond):
			}
			if ls.AckTee(context.Background(), read.Load()) != nil {
				return
			}
		}
	}()
	const run, rate = 3 * time.Second, 128 << 10 // bytes per second
	chunk := bytes.Repeat([]byte("z"), 8<<10)
	go func() {
		tick := time.NewTicker(time.Second * time.Duration(len(chunk)) / rate)
		defer tick.Stop()
		for end := time.Now().Add(run); time.Now().Before(end); <-tick.C {
			if WriteLocalFrame(cli.conn, LocalData, chunk) != nil {
				return
			}
		}
	}()
	shown := 0
	for deadline := time.Now().Add(run + time.Second); time.Now().Before(deadline); {
		f, ok := cli.next(time.Until(deadline))
		if !ok {
			break
		}
		if f.typ != LocalData {
			t.Fatalf("the session ended under steady output: %q %s", f.typ, f.b)
		}
		shown += len(f.b)
	}
	if want := int(run.Seconds()) * (64 << 10); shown < want {
		t.Fatalf("shown %d bytes in %v, want more than 64 KiB/s (%d)", shown, run, want)
	}
}

func TestLocalAttachObserverInputIsDropped(t *testing.T) {
	r := localAttachRig(t)
	id, ref := r.run(t)
	cli := dialLocal(t, r.server)
	ls, err := r.sub.LocalAttach(ctx5(t), ref, runner.LocalAttachRequest{Nonce: cli.hello(t, id), Options: runner.AttachOptions{Observer: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	var bound LocalBoundMsg
	_ = json.Unmarshal(cli.expect(t, LocalBound).b, &bound)
	if !bound.ReadOnly {
		t.Fatal("an observer was bound as a writer")
	}
	cli.send(t, LocalData, []byte("typed by an observer"))
	cli.send(t, LocalResize, LocalSizeMsg{Cols: 10, Rows: 5})
	if got := cli.data(1, 300*time.Millisecond); len(got) != 0 {
		t.Fatalf("an observer's input reached the PTY: %q", got)
	}
	if len(r.fake.Resizes) != 0 {
		t.Fatalf("an observer resized the PTY: %v", r.fake.Resizes)
	}
	// Promotion opens a writer's exec in place of the observer's.
	if err := ls.SetObserver(ctx5(t), false, 80, 24); err != nil {
		t.Fatal(err)
	}
	var mode LocalModeMsg
	_ = json.Unmarshal(cli.expect(t, LocalMode).b, &mode)
	if mode.ReadOnly {
		t.Fatal("promotion told the CLI it is read-only")
	}
	cli.send(t, LocalData, []byte("writer"))
	if got := cli.data(6, 5*time.Second); string(got) != "writer" {
		t.Fatalf("promoted writer's echo = %q", got)
	}
}

func TestLocalAttachModeObserverDropsInputFromThenOn(t *testing.T) {
	r := localAttachRig(t)
	id, ref := r.run(t)
	cli := dialLocal(t, r.server)
	ls, err := r.sub.LocalAttach(ctx5(t), ref, runner.LocalAttachRequest{Nonce: cli.hello(t, id)})
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	cli.expect(t, LocalBound)
	cli.send(t, LocalData, []byte("a"))
	if got := cli.data(1, 5*time.Second); string(got) != "a" {
		t.Fatalf("writer echo = %q", got)
	}
	if err := ls.SetObserver(ctx5(t), true, 0, 0); err != nil {
		t.Fatal(err)
	}
	cli.expect(t, LocalMode)
	cli.send(t, LocalData, []byte("b"))
	if got := cli.data(1, 300*time.Millisecond); len(got) != 0 {
		t.Fatalf("input after the fence reached the PTY: %q", got)
	}
}

// The socket grants nothing by itself: a nonce the org never grants expires, an unknown nonce is
// refused, a nonce is spent once, reaches only its own connection, and only its own run.
func TestLocalAttachNonceRefusals(t *testing.T) {
	r := localAttachRig(t, func(lt *localTimes) { lt.nonceTTL = 300 * time.Millisecond })
	id, ref := r.run(t)
	other, otherRef := r.run(t)

	waiting := dialLocal(t, r.server) // no ticket: the org sends no grant
	waiting.hello(t, id)
	f := waiting.expect(t, LocalRefuse)
	if !bytes.Contains(f.b, []byte(LocalReasonExpired)) {
		t.Fatalf("ungranted connection ended with %s", f.b)
	}
	if local, _ := r.server.local(ref); r.fake.Called("attach", local) {
		t.Fatal("an ungranted connection reached the substrate")
	}

	if _, err := r.sub.LocalAttach(ctx5(t), ref, runner.LocalAttachRequest{Nonce: "not-a-nonce"}); err == nil {
		t.Fatal("an unknown nonce was granted")
	}

	a, b := dialLocal(t, r.server), dialLocal(t, r.server)
	na := a.hello(t, id)
	b.hello(t, id)
	ls, err := r.sub.LocalAttach(ctx5(t), ref, runner.LocalAttachRequest{Nonce: na})
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	a.expect(t, LocalBound)
	if f := b.expect(t, LocalRefuse); !bytes.Contains(f.b, []byte(LocalReasonExpired)) {
		t.Fatalf("the other connection got %q %s, not its own expiry", f.typ, f.b)
	}
	if _, err := r.sub.LocalAttach(ctx5(t), ref, runner.LocalAttachRequest{Nonce: na}); err == nil {
		t.Fatal("a spent nonce was granted again")
	}

	c := dialLocal(t, r.server)
	nc := c.hello(t, other)
	if _, err := r.sub.LocalAttach(ctx5(t), ref, runner.LocalAttachRequest{Nonce: nc}); err == nil {
		t.Fatalf("a nonce for %s was granted for %s", otherRef, ref)
	}
	c.expect(t, LocalRefuse)
}

func TestLocalAttachHelloRefusesARunNotHere(t *testing.T) {
	r := localAttachRig(t)
	cli := dialLocal(t, r.server)
	cli.send(t, LocalHello, LocalHelloMsg{V: LocalProtocol, RunID: uuid.New()})
	if f := cli.expect(t, LocalRefuse); !bytes.Contains(f.b, []byte(LocalReasonNotHere)) {
		t.Fatalf("refuse = %s", f.b)
	}
	old := dialLocal(t, r.server)
	id, _ := r.run(t)
	old.send(t, LocalHello, LocalHelloMsg{V: 2, RunID: id})
	if f := old.expect(t, LocalRefuse); !bytes.Contains(f.b, []byte(LocalReasonProtocol)) {
		t.Fatalf("refuse = %s", f.b)
	}
}

func TestLocalAttachLinkLossEndsTheSession(t *testing.T) {
	r := localAttachRig(t)
	id, ref := r.run(t)
	cli := dialLocal(t, r.server)
	ls, err := r.sub.LocalAttach(ctx5(t), ref, runner.LocalAttachRequest{Nonce: cli.hello(t, id), Tee: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	cli.expect(t, LocalBound)
	r.rp.Close()
	cli.expectEnd(t, 2*time.Second, "")
	if _, ok := cli.next(time.Second); ok {
		t.Fatal("the connection stayed open after the link dropped")
	}
}

func TestLocalAttachTeeClosedEndsThePTYWithinASecond(t *testing.T) {
	r := localAttachRig(t)
	id, ref := r.run(t)
	cli := dialLocal(t, r.server)
	ls, err := r.sub.LocalAttach(ctx5(t), ref, runner.LocalAttachRequest{Nonce: cli.hello(t, id)})
	if err != nil {
		t.Fatal(err)
	}
	cli.expect(t, LocalBound)
	_ = ls.Close()
	cli.expectEnd(t, time.Second, LocalReasonOrgEnded)
	r.server.mu.Lock()
	n := len(r.server.locals)
	r.server.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d local sessions left after the tee closed", n)
	}
}

func TestLocalAttachEndsBeforeItsSandboxIsKilled(t *testing.T) {
	r := localAttachRig(t)
	id, ref := r.run(t)
	cli := dialLocal(t, r.server)
	ls, err := r.sub.LocalAttach(ctx5(t), ref, runner.LocalAttachRequest{Nonce: cli.hello(t, id)})
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Close()
	cli.expect(t, LocalBound)
	local, _ := r.server.local(ref)
	r.server.EndLocalAttach(local)
	cli.expectEnd(t, time.Second, LocalReasonRunEnded)
}
