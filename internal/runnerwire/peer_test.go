// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

type pair struct {
	org, run   *Peer
	orgC, runC *LoopConn
}

func newPair(t *testing.T, orgCfg, runCfg PeerConfig) *pair {
	t.Helper()
	oc, rc := Loopback()
	orgCfg.Org = true
	runCfg.Org = false
	p := &pair{org: NewPeer(oc, orgCfg), run: NewPeer(rc, runCfg), orgC: oc, runC: rc}
	go p.org.Run(context.Background())
	go p.run.Run(context.Background())
	t.Cleanup(func() { p.org.Close(); p.run.Close() })
	return p
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func echoHandler(ctx context.Context, id uint32, method string, args json.RawMessage) (any, error) {
	switch method {
	case "echo":
		var v map[string]string
		if err := json.Unmarshal(args, &v); err != nil {
			return nil, err
		}
		return v, nil
	case "offline":
		return nil, runner.ErrRunnerOffline
	case "refuse":
		return nil, Refuse("outside allowed roots")
	case "block":
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, errors.New("boom")
}

func TestPeerCallRoundTripAndTypedErrors(t *testing.T) {
	p := newPair(t, PeerConfig{}, PeerConfig{OnCall: echoHandler})
	var got map[string]string
	if err := p.org.Call(ctxT(t), "echo", map[string]string{"a": "b"}, &got); err != nil || got["a"] != "b" {
		t.Fatalf("echo = %v, %v", got, err)
	}
	if err := p.org.Call(ctxT(t), "offline", nil, nil); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("runner_offline code did not reach errors.Is: %v", err)
	}
	var we *Error
	if err := p.org.Call(ctxT(t), "refuse", nil, nil); !errors.As(err, &we) || we.Code != CodeRefused || we.Message != "outside allowed roots" {
		t.Fatalf("refusal = %v", err)
	}
	if err := p.org.Call(ctxT(t), "x", nil, nil); !errors.As(err, &we) || we.Code != CodeInternal {
		t.Fatalf("unclassified error = %v", err)
	}
}

func TestPeerCallIDsFollowTheStreamIDSpace(t *testing.T) {
	var mu sync.Mutex
	var seen []uint32
	h := func(ctx context.Context, id uint32, m string, a json.RawMessage) (any, error) {
		mu.Lock()
		seen = append(seen, id)
		mu.Unlock()
		return nil, nil
	}
	p := newPair(t, PeerConfig{}, PeerConfig{OnCall: h})
	for range 3 {
		if err := p.org.Call(ctxT(t), "echo", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range seen {
		if id == 0 || id%2 != 0 {
			t.Fatalf("org call id %d is not even", id)
		}
	}
}

func TestPeerWaitingEventsArriveInOrderBeforeReplyOnCallersGoroutine(t *testing.T) {
	var run *Peer
	h := func(ctx context.Context, id uint32, m string, a json.RawMessage) (any, error) {
		for _, d := range []string{"pulling", "creating", "starting"} {
			data, _ := json.Marshal(Waiting{Detail: d})
			if err := run.Event(ctx, Event{Kind: EventWaiting, Call: id, Data: data}); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	p := newPair(t, PeerConfig{}, PeerConfig{OnCall: h})
	run = p.run
	var details []string
	if err := p.org.Call(ctxT(t), MethodCreateSandbox, nil, nil, WithEvents(func(ev Event) {
		var w Waiting
		_ = json.Unmarshal(ev.Data, &w)
		details = append(details, w.Detail) // unsynchronised on purpose: -race proves it is the caller's goroutine
	})); err != nil {
		t.Fatal(err)
	}
	if len(details) != 3 || details[0] != "pulling" || details[2] != "starting" {
		t.Fatalf("waiting events before REPLY = %v", details)
	}
}

func TestPeerOtherEventsGoToOnEvent(t *testing.T) {
	got := make(chan Event, 1)
	p := newPair(t, PeerConfig{OnEvent: func(ev Event) { got <- ev }}, PeerConfig{})
	if err := p.run.Event(ctxT(t), Event{Kind: EventCaps, Data: json.RawMessage(`{"version":"1"}`)}); err != nil {
		t.Fatal(err)
	}
	if ev := <-got; ev.Kind != EventCaps {
		t.Fatalf("event = %+v", ev)
	}
	if err := p.run.Event(ctxT(t), Event{Kind: "bogus"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.org.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("an event of an unknown kind did not close the session")
	}
}

func TestPeerStreamEchoCreditAndHalfClose(t *testing.T) {
	p := newPair(t, PeerConfig{}, PeerConfig{OnOpen: func(s *Stream, o Open) error {
		go func() { // the runner side echoes
			io.Copy(s, s)
			s.CloseWrite()
		}()
		return nil
	}})
	s, err := p.org.Open(ctxT(t), Open{Kind: KindRelay, Target: "x"})
	if err != nil {
		t.Fatal(err)
	}
	// 2 MiB crosses the 256 KiB stream window several times: the writer must
	// block on credit and every byte must still arrive, in order.
	want := make([]byte, 2<<20)
	for i := range want {
		want[i] = byte(i * 7)
	}
	go func() { s.Write(want); s.CloseWrite() }()
	got, err := io.ReadAll(s)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("echoed %d bytes, want %d (err %v)", len(got), len(want), err)
	}
}

func TestPeerStreamReset(t *testing.T) {
	accepted := make(chan *Stream, 1)
	p := newPair(t, PeerConfig{}, PeerConfig{OnOpen: func(s *Stream, o Open) error { accepted <- s; return nil }})
	s, err := p.org.Open(ctxT(t), Open{Kind: KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	peer := <-accepted
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	var re *ResetError
	if _, err := peer.Read(make([]byte, 1)); !errors.As(err, &re) || re.Code != ResetCancelled {
		t.Fatalf("peer read after reset = %v", err)
	}
	if _, err := s.Write([]byte("x")); !errors.As(err, &re) {
		t.Fatalf("write after reset = %v", err)
	}
}

func TestPeerRefusedOpen(t *testing.T) {
	p := newPair(t, PeerConfig{}, PeerConfig{OnOpen: func(*Stream, Open) error { return errors.New("no") }})
	if _, err := p.org.Open(ctxT(t), Open{Kind: KindRelay}); err == nil {
		t.Fatal("a refused open succeeded")
	}
}

func TestPeerCallOpensStreamsClaimedFromTheReply(t *testing.T) {
	var run *Peer
	h := func(ctx context.Context, id uint32, m string, a json.RawMessage) (any, error) {
		s, err := run.Open(ctx, Open{Kind: KindPTY, Call: id})
		if err != nil {
			return nil, err
		}
		go func() { s.Write([]byte("hello")); s.CloseWrite() }()
		return StreamResult{Stream: s.ID()}, nil
	}
	p := newPair(t, PeerConfig{}, PeerConfig{OnCall: h})
	run = p.run
	var res StreamResult
	if err := p.org.Call(ctxT(t), MethodAttach, nil, &res); err != nil {
		t.Fatal(err)
	}
	s, err := p.org.TakeStream(res.Stream)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(s); string(got) != "hello" {
		t.Fatalf("pty stream = %q", got)
	}
	if _, err := p.org.TakeStream(res.Stream + 100); !errors.Is(err, ErrNoStream) {
		t.Fatalf("TakeStream of an unknown id = %v", err)
	}
}

func TestPeerCallerCancelStopsTheHandler(t *testing.T) {
	stopped := make(chan struct{})
	h := func(ctx context.Context, id uint32, m string, a json.RawMessage) (any, error) {
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}
	p := newPair(t, PeerConfig{}, PeerConfig{OnCall: h})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- p.org.Call(ctx, MethodWait, nil, nil) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call = %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("the runner-side handler outlived its caller")
	}
}

func TestLinkDropMidCallIsRunnerOffline(t *testing.T) {
	p := newPair(t, PeerConfig{}, PeerConfig{OnCall: echoHandler})
	errc := make(chan error, 1)
	go func() { errc <- p.org.Call(ctxT(t), "block", nil, nil) }()
	time.Sleep(50 * time.Millisecond)
	p.orgC.Drop()
	if err := <-errc; !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("call over a dropped link = %v, want ErrRunnerOffline", err)
	}
	if p.org.Online() {
		t.Fatal("the peer reports online after the link dropped")
	}
	if err := p.org.Call(ctxT(t), "echo", nil, nil); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("a new call on a dead link = %v", err)
	}
}

func TestLinkDropResetsByteStreams(t *testing.T) {
	p := newPair(t, PeerConfig{}, PeerConfig{OnOpen: func(*Stream, Open) error { return nil }})
	s, err := p.org.Open(ctxT(t), Open{Kind: KindExec})
	if err != nil {
		t.Fatal(err)
	}
	p.runC.Drop()
	var re *ResetError
	if _, err := s.Read(make([]byte, 1)); !errors.As(err, &re) || re.Code != ResetReconnect {
		t.Fatalf("stream read after the link dropped = %v, want a reconnect reset", err)
	}
}

func TestSeverMidFrameClosesTheSession(t *testing.T) {
	p := newPair(t, PeerConfig{}, PeerConfig{})
	p.runC.SeverMidFrame()
	_ = p.run.Event(ctxT(t), Event{Kind: EventCaps, Data: json.RawMessage(`{"version":"padded padded padded padded"}`)})
	select {
	case <-p.org.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a frame cut in half did not close the session")
	}
	if err := p.org.Err(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("session error = %v, want ErrUnexpectedEOF", err)
	}
}

func TestOversizeFrameClosesTheSession(t *testing.T) {
	p := newPair(t, PeerConfig{}, PeerConfig{})
	hdr, _ := (Frame{Type: TypeData, Stream: 1}).Encode(nil)
	hdr[16], hdr[17], hdr[18], hdr[19] = 0, 2, 0, 0 // 128 KiB: over the DATA limit
	p.runC.out <- hdr
	select {
	case <-p.org.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("an oversize frame did not close the session")
	}
	if err := p.org.Err(); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("session error = %v, want ErrFrameTooLarge", err)
	}
}

func TestFrameFromTheWrongSideClosesTheSession(t *testing.T) {
	p := newPair(t, PeerConfig{}, PeerConfig{})
	b, _ := (Frame{Type: TypeLease, Seq: 1}).Encode(nil) // LEASE is org -> runner
	p.runC.out <- b
	select {
	case <-p.org.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a runner-sent LEASE did not close the session")
	}
}

func TestLoopbackDelay(t *testing.T) {
	oc, rc := Loopback()
	oc.SetDelay(80 * time.Millisecond)
	start := time.Now()
	go oc.WriteFrame(context.Background(), Frame{Type: TypePing})
	if _, err := rc.ReadFrame(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 70*time.Millisecond {
		t.Fatal("the injected delay was not applied")
	}
}

func TestPeerAcksTrimTheReplayBuffer(t *testing.T) {
	p := newPair(t, PeerConfig{OnEvent: func(Event) {}}, PeerConfig{})
	for range 40 {
		if err := p.run.Event(ctxT(t), Event{Kind: EventCaps}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.run.replay.mu.Lock()
		held := len(p.run.replay.frames)
		p.run.replay.mu.Unlock()
		if held < 8 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d frames still buffered after 40 events: ACKs are not trimming", held)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
