// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestCurrentActivityIsNeverBufferedForResume(t *testing.T) {
	ctx := ctxT(t)
	received := make(chan Frame, 1)
	p := newPair(t, PeerConfig{OnControlChecked: func(f Frame) error { received <- f; return nil }}, PeerConfig{})
	run := uuid.New()
	if err := p.run.CurrentActivity(ctx, run); err != nil {
		t.Fatal(err)
	}
	select {
	case frame := <-received:
		var activity CurrentActivity
		if frame.Type != TypeCurrentActivity || frame.Seq != 0 || frame.Stream != 0 || Unmarshal(frame.Payload, &activity) != nil || activity.RunID != run {
			t.Fatalf("activity frame: %+v", frame)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	p.run.Close()
	cfg, err := p.run.ResumeConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := cfg.Replay.Since(0)
	if err != nil || len(frames) != 0 || cfg.LastReceived != 0 {
		t.Fatalf("live activity entered resume state: frames=%v received=%d err=%v", frames, cfg.LastReceived, err)
	}
	org, local := Loopback()
	defer org.Close()
	resumed := NewPeer(local, cfg)
	defer resumed.Close()
	if err := resumed.Start(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := resumed.Event(ctx, Event{Kind: EventPosture}); err != nil {
		t.Fatal(err)
	}
	first, err := org.ReadFrame(ctx)
	if err != nil || first.Type != TypeEvent || first.Seq != 1 {
		t.Fatalf("resume replayed activity before new sequenced event: %+v %v", first, err)
	}
}

func TestCurrentActivityRejectsWrongDirectionAndMissingRun(t *testing.T) {
	p := newPair(t, PeerConfig{}, PeerConfig{})
	if err := p.org.CurrentActivity(ctxT(t), uuid.New()); !errors.Is(err, ErrBadFrame) {
		t.Fatal("org sent runner activity", err)
	}
	if err := p.run.CurrentActivity(ctxT(t), uuid.Nil); !errors.Is(err, ErrBadFrame) {
		t.Fatal("activity omitted run", err)
	}
	frame := Frame{Type: TypeCurrentActivity, Seq: 1}
	if err := frame.Validate(); !errors.Is(err, ErrBadFrame) {
		t.Fatal("sequenced activity accepted", err)
	}
	if err := p.run.handle(Frame{Type: TypeCurrentActivity}); !errors.Is(err, ErrBadFrame) {
		t.Fatal("runner accepted activity from org", err)
	}
}
