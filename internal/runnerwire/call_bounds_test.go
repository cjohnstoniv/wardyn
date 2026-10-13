// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestIncomingCallsBoundHandlers(t *testing.T) {
	a, b := Loopback()
	defer b.Close()
	p := NewPeer(a, PeerConfig{Org: true, OnCall: func(ctx context.Context, _ uint32, _ string, _ json.RawMessage) (any, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	defer p.Close()
	for i := range MaxCalls {
		if err := p.onCall(Frame{Stream: uint32(i*2 + 1), Payload: []byte(`{"method":"blocked"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.onCall(Frame{Stream: uint32(MaxCalls*2 + 1), Payload: []byte(`{"method":"blocked"}`)}); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("unbounded handler admission: %v", err)
	}
}

func TestCallEventQueueBoundAndDrain(t *testing.T) {
	c := &pendingCall{onEvent: func(Event) {}, notify: make(chan struct{}, 1)}
	ev := Event{Kind: EventWaiting, Data: make([]byte, MaxControlFrame/2)}
	if queued, err := c.queue(ev); !queued || err != nil {
		t.Fatal(err)
	}
	if _, err := c.queue(ev); !errors.Is(err, ErrBadFrame) {
		t.Fatalf("unbounded callback queue: %v", err)
	}
	c.drain()
	if queued, err := c.queue(ev); !queued || err != nil {
		t.Fatalf("drain did not release capacity: %v", err)
	}
}
