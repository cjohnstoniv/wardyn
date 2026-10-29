// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// refuseFirst is an org OnEventChecked that refuses the first event for good and accepts the rest.
func refuseFirst(calls *atomic.Int32) func(Event) error {
	return func(Event) error {
		if calls.Add(1) == 1 {
			return &EventRefusal{Code: RefusalShape, Err: errors.New("unknown field")}
		}
		return nil
	}
}

// A refusal the organisation recorded is acknowledged like an acceptance: the runner's wait returns, the org's
// cursor moves past the event, and the session carries on (DR-0436).
func TestEventRefusalAcknowledgedKeepsSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var calls atomic.Int32
	a, b := Loopback()
	org := NewPeer(a, PeerConfig{Org: true, OnEventChecked: refuseFirst(&calls)})
	run := NewPeer(b, PeerConfig{})
	defer org.Close()
	defer run.Close()
	go org.Run(ctx) //nolint:errcheck // the test owns shutdown
	go run.Run(ctx) //nolint:errcheck // the test owns shutdown
	if err := run.EventAck(ctx, Event{Kind: EventAgentExit}); err != nil {
		t.Fatalf("refused event was not acknowledged: %v", err)
	}
	if org.LastReceivedSeq() != 1 || !org.Online() || !run.Online() {
		t.Fatalf("cursor %d, org online %v, runner online %v: want 1, true, true", org.LastReceivedSeq(), org.Online(), run.Online())
	}
	if err := run.EventAck(ctx, Event{Kind: EventSpool}); err != nil {
		t.Fatalf("event after a refusal: %v", err)
	}
	if org.LastReceivedSeq() != 2 {
		t.Fatalf("cursor %d after two events, want 2", org.LastReceivedSeq())
	}
}

// The livelock witness: a refused event is handled once, however often the session drops and resumes, because
// the runner's replay buffer no longer holds it.
func TestEventRefusalNotReplayedOnResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var refused atomic.Int32
	orgCfg := PeerConfig{Org: true, OnEventChecked: func(ev Event) error {
		if ev.Kind == EventAgentExit {
			refused.Add(1)
			return &EventRefusal{Code: RefusalStoreRefused, Err: errors.New("generation moved on")}
		}
		return nil
	}}
	a, b := Loopback()
	org, run := NewPeer(a, orgCfg), NewPeer(b, PeerConfig{})
	go org.Run(ctx) //nolint:errcheck // the test drops the link
	go run.Run(ctx) //nolint:errcheck // the test drops the link
	acked := 0
	if run.EventAck(ctx, Event{Kind: EventAgentExit}) == nil {
		acked++
	}
	for cycle := 0; cycle < 3; cycle++ {
		a.Drop()
		orgNext, err := org.ResumeConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		runNext, err := run.ResumeConfig(ctx)
		if err != nil {
			t.Fatal(err)
		}
		frames, err := runNext.Replay.Since(orgNext.LastReceived)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range frames {
			if f.Type == TypeEvent {
				t.Errorf("cycle %d: the runner would replay EVENT seq %d after the org's cursor %d", cycle, f.Seq, orgNext.LastReceived)
			}
		}
		a, b = Loopback()
		org, run = NewPeer(a, orgNext), NewPeer(b, runNext)
		if err := org.Start(ctx, runNext.LastReceived); err != nil {
			t.Fatal(err)
		}
		if err := run.Start(ctx, orgNext.LastReceived); err != nil {
			t.Fatal(err)
		}
		if run.EventAck(ctx, Event{Kind: EventSpool}) == nil {
			acked++
		}
	}
	defer org.Close()
	defer run.Close()
	if n := refused.Load(); n != 1 || acked != 4 {
		t.Fatalf("refused event handled %d times, %d of 4 waits acknowledged: want 1 and 4", n, acked)
	}
}

// A 0.9.x runner knows nothing new: it writes raw EVENT frames and reads an ACK, which is all the fix sends it.
func TestEventRefusalOldRunnerRawFrames(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var calls atomic.Int32
	a, b := Loopback()
	org := NewPeer(a, PeerConfig{Org: true, OnEventChecked: refuseFirst(&calls)})
	defer org.Close()
	go org.Run(ctx) //nolint:errcheck // the test owns shutdown
	payload, err := Marshal(Event{Kind: EventSpool})
	if err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= 2; seq++ {
		if err := b.WriteFrame(ctx, Frame{Type: TypeEvent, Seq: seq, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		for {
			f, err := b.ReadFrame(ctx)
			if err != nil {
				t.Fatalf("EVENT %d: the org ended the link instead of acknowledging: %v", seq, err)
			}
			if f.Type == TypeGoaway || f.Type == TypeRevoked {
				t.Fatalf("EVENT %d: the org sent %s", seq, f.Type)
			}
			if f.Type != TypeAck {
				continue
			}
			got, err := DecodeAck(f.Payload)
			if err != nil || got != seq {
				t.Fatalf("EVENT %d: ACK %d, %v", seq, got, err)
			}
			break
		}
	}
	if !org.Online() {
		t.Fatal("org closed the link after acknowledging")
	}
}

// The protocol version and frame types are those of 0.9.8; the `workload` event (rc.8) is the one kind added.
func TestWireUnchangedSinceV098(t *testing.T) {
	if ProtocolVersion != 1 || len(EventKinds) != 8 || typeEnd != TypeCurrentActivity+1 {
		t.Fatalf("protocol %d, %d event kinds, typeEnd %d: want 1, 8, %d", ProtocolVersion, len(EventKinds), typeEnd, TypeCurrentActivity+1)
	}
}
