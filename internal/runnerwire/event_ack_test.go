// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestEventAckSmallTailWaitsForDurableAcceptance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	a, b := Loopback()
	org := NewPeer(a, PeerConfig{Org: true, OnEventChecked: func(Event) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	run := NewPeer(b, PeerConfig{})
	defer org.Close()
	defer run.Close()
	go org.Run(ctx) //nolint:errcheck // the test owns shutdown
	go run.Run(ctx) //nolint:errcheck // the test owns shutdown
	done := make(chan error, 1)
	go func() { done <- run.EventAck(ctx, Event{Kind: EventSpool}) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-done:
		t.Fatalf("acknowledged before durable acceptance: %v", err)
	default:
	}
	if org.LastReceivedSeq() != 0 {
		t.Fatal("advanced cursor before durable acceptance")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("single-event tail never acknowledged")
	}
}

func TestEventAckCancellationAndRefusal(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "refuse"}[refuse], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			accepted := make(chan struct{})
			cfg := PeerConfig{Org: true, OnEvent: func(Event) { close(accepted) }}
			if refuse {
				cfg.OnEventChecked = func(Event) error { close(accepted); return errors.New("store refused") }
			}
			a, b := Loopback()
			org, run := NewPeer(a, cfg), NewPeer(b, PeerConfig{})
			defer org.Close()
			defer run.Close()
			go org.Run(ctx) //nolint:errcheck // the test owns shutdown
			go run.Run(ctx) //nolint:errcheck // the test owns shutdown
			waitCtx, stop := context.WithCancel(ctx)
			defer stop()
			done := make(chan error, 1)
			go func() { done <- run.EventAck(waitCtx, Event{Kind: EventSpool}) }()
			select {
			case <-accepted:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if !refuse {
				stop()
			}
			select {
			case err := <-done:
				if err == nil || (!refuse && !errors.Is(err, context.Canceled)) {
					t.Fatalf("ack wait: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("ack waiter survived cancellation/link failure")
			}
			if refuse && org.LastReceivedSeq() != 0 {
				t.Fatal("refused event was acknowledged")
			}
		})
	}
}

type loseAck struct {
	Conn
	lost atomic.Bool
}

func (c *loseAck) WriteFrame(ctx context.Context, f Frame) error {
	if f.Type == TypeAck && c.lost.CompareAndSwap(false, true) {
		_ = c.Conn.Close()
		return ErrLinkDown
	}
	return c.Conn.WriteFrame(ctx, f)
}

func TestEventAckLostAckResumesWithoutLosingDurableTail(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var accepted atomic.Int32
	a, b := Loopback()
	org := NewPeer(&loseAck{Conn: a}, PeerConfig{Org: true, OnEventChecked: func(Event) error { accepted.Add(1); return nil }})
	run := NewPeer(b, PeerConfig{})
	go org.Run(ctx) //nolint:errcheck // injected transport failure
	go run.Run(ctx) //nolint:errcheck // injected transport failure
	if err := run.EventAck(ctx, Event{Kind: EventSpool}); err == nil {
		t.Fatal("lost ACK reported success")
	}
	orgCfg, err := org.ResumeConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	runCfg, err := run.ResumeConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	a, b = Loopback()
	org, run = NewPeer(a, orgCfg), NewPeer(b, runCfg)
	defer org.Close()
	defer run.Close()
	if err := org.Start(ctx, runCfg.LastReceived); err != nil {
		t.Fatal(err)
	}
	if err := run.Start(ctx, orgCfg.LastReceived); err != nil {
		t.Fatal(err)
	}
	if err := run.replay.waitAck(ctx, run.Done(), 1); err != nil {
		t.Fatal("authenticated resume cursor did not acknowledge durable tail:", err)
	}
	if err := run.EventAck(ctx, Event{Kind: EventSpool}); err != nil {
		t.Fatal(err)
	}
	if accepted.Load() != 2 {
		t.Fatalf("durable tail replayed despite accepted cursor: %d ingests", accepted.Load())
	}
}

// EventsAck writes the whole batch before it waits, and one cumulative ACK for
// the last event settles it: one round trip per batch.
func TestEventsAckPipelinesUntilCumulativeAck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	org, b := Loopback()
	run := NewPeer(b, PeerConfig{})
	defer run.Close()
	go run.Run(ctx) //nolint:errcheck // the test owns shutdown
	evs := make([]Event, 8)
	for i := range evs {
		evs[i] = Event{Kind: EventSpool}
	}
	done := make(chan error, 1)
	go func() { done <- run.EventsAck(ctx, evs) }()
	for seq := uint64(1); seq <= uint64(len(evs)); seq++ {
		f, err := org.ReadFrame(ctx)
		if err != nil || f.Type != TypeEvent || f.Seq != seq {
			t.Fatalf("event %d not sent before any ACK: %+v %v", seq, f, err)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("returned before the ACK: %v", err)
	default:
	}
	if err := org.WriteFrame(ctx, Frame{Type: TypeAck, Payload: EncodeAck(uint64(len(evs)))}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
