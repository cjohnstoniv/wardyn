// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// linkStillWorks fails the test unless the shared connection is open, both
// peers are online and a call still round-trips over it.
func linkStillWorks(t *testing.T, org, run *Peer, sock *gatedConn) {
	t.Helper()
	select {
	case <-sock.closed:
		t.Fatal("a cancelled caller closed the shared connection")
	default:
	}
	if !org.Online() || !run.Online() {
		t.Fatalf("a cancelled caller took the session offline: org %v, runner %v", org.Err(), run.Err())
	}
	var got map[string]string
	if err := org.Call(ctxT(t), "echo", map[string]string{"k": "v"}, &got); err != nil || got["k"] != "v" {
		t.Fatalf("a call after the cancelled one = %v, %v", got, err)
	}
}

// A caller that gives up while its frame is still being written must not end
// the session: coder/websocket closes the whole socket when the ctx of an
// in-flight write ends, so the frame is written under the link's ctx and the
// caller's ctx bounds only the waits around it.
func TestCallerCancelDuringABlockedWriteKeepsTheConnection(t *testing.T) {
	for _, tc := range []struct {
		name string
		op   func(ctx context.Context, run *Peer) error
	}{
		{"Open", func(ctx context.Context, run *Peer) error {
			_, err := run.Open(ctx, Open{Kind: KindRelay})
			return err
		}},
		{"Call", func(ctx context.Context, run *Peer) error { return run.Call(ctx, "echo", nil, nil) }},
		{"Event", func(ctx context.Context, run *Peer) error { return run.Event(ctx, Event{Kind: EventWaiting}) }},
		{"Send", func(ctx context.Context, run *Peer) error { return run.Send(ctx, Frame{Type: TypeReady}) }},
		{"EventAck", func(ctx context.Context, run *Peer) error { return run.EventAck(ctx, Event{Kind: EventWaiting}) }},
		{"CurrentActivity", func(ctx context.Context, run *Peer) error { return run.CurrentActivity(ctx, uuid.New()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			org, run, sock := newWSPair(t, PeerConfig{OnCall: echoHandler})
			sock.hold()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { _ = tc.op(ctx, run); close(done) }()
			select {
			case <-sock.blocked:
			case <-time.After(3 * time.Second):
				t.Fatal("the frame never reached the socket")
			}
			cancel()
			select { // a write whose ctx ended closes the socket at once
			case <-sock.closed:
			case <-time.After(500 * time.Millisecond):
			}
			sock.release()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("the cancelled caller never returned")
			}
			linkStillWorks(t, org, run, sock)
		})
	}
}

// Characterization: a caller that gives up while WAITING (for OPEN_OK, a
// REPLY or an ACK) gets its ctx error and the session carries on; Open still
// resets the stream it gave up on.
func TestCallerCancelWhileWaitingKeepsTheConnection(t *testing.T) {
	t.Run("Open waiting for OPEN_OK", func(t *testing.T) {
		accepted := make(chan *Stream, 1)
		org, run, sock := newWSPair(t, PeerConfig{OnCall: echoHandler, OnOpen: func(s *Stream, o Open) error { accepted <- s; return nil }})
		sock.hold() // the runner's OPEN_OK cannot leave
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { _, err := org.Open(ctx, Open{Kind: KindPTY}); errc <- err }()
		runS := <-accepted
		<-sock.blocked
		cancel()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Open = %v", err)
		}
		sock.release()
		select {
		case <-runS.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("the abandoned stream was not reset")
		}
		linkStillWorks(t, org, run, sock)
	})
	t.Run("Call waiting for the REPLY", func(t *testing.T) {
		started := make(chan struct{}, 1)
		org, run, sock := newWSPair(t, PeerConfig{OnCall: func(ctx context.Context, id uint32, m string, a json.RawMessage) (any, error) {
			if m == "block" {
				started <- struct{}{}
			}
			return echoHandler(ctx, id, m, a)
		}})
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- org.Call(ctx, "block", nil, nil) }()
		<-started // the CALL was written and is being served
		cancel()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled Call = %v", err)
		}
		linkStillWorks(t, org, run, sock)
	})
	t.Run("EventAck waiting for the ACK", func(t *testing.T) {
		org, run, sock := newWSPair(t, PeerConfig{OnCall: echoHandler}) // one EVENT is not acked by itself
		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() { errc <- run.EventAck(ctx, Event{Kind: EventWaiting}) }()
		waitFor(t, 3*time.Second, "the EVENT to arrive", func() bool { return org.LastReceivedSeq() >= 1 })
		cancel()
		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled EventAck = %v", err)
		}
		linkStillWorks(t, org, run, sock)
	})
}
