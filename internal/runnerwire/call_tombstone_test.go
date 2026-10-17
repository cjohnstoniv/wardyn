// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// startedRaw is a resumed peer under test, run by Start as a session runs it, whose other end is a bare
// LoopConn the test drives frame by frame.
func startedRaw(t *testing.T, cfg PeerConfig, lastSeq uint64) *rawPeer {
	t.Helper()
	pc, rc := Loopback()
	p := NewPeer(pc, cfg)
	if err := p.Start(context.Background(), lastSeq); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return &rawPeer{p: p, raw: rc}
}

// carriedCall is the org side of a session resumed after a link drop with call 2 unanswered: its CALL
// went out as seq 1 and is still in the replay buffer, and the call was carried as a tombstone.
func carriedCall() PeerConfig {
	buf := NewReplayBuffer()
	call := callFrame(2, 0)
	_ = buf.Reserve(context.Background(), len(call.Payload))
	buf.Assign(call)
	return PeerConfig{Org: true, Replay: buf, NextID: 4, AbandonedCalls: []uint32{2}}
}

func pong() Frame { return Frame{Type: TypePong, Payload: EncodePing(time.Now())} }

func (r *rawPeer) pending(id uint32) bool {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()
	return r.p.calls[id] != nil
}

// handled waits until the peer has taken sequenced frame seq, or closed the session over it.
func (r *rawPeer) handled(t *testing.T, seq uint64) {
	t.Helper()
	waitFor(t, 3*time.Second, "the frame to be handled", func() bool { return r.p.LastReceivedSeq() >= seq || !r.p.Online() })
}

// R2-0100: a call the runner had received when the link dropped can only be answered by a REPLY in the
// runner's replay, since the drop ended the runner's handler. Once the runner's first PONG shows that
// replay has been read, the tombstone goes, so tombstones no longer pile up across resumes toward
// MaxCalls. A REPLY for it after that is still a protocol error.
func TestResumeRetiresCallsThePeerCanNoLongerAnswer(t *testing.T) {
	r := startedRaw(t, carriedCall(), 1) // the runner had received the CALL
	if !r.pending(2) {
		t.Fatal("the carried call was dropped before the runner's replay was read")
	}
	r.write(t, pong())
	waitFor(t, 3*time.Second, "the carried call to retire", func() bool { return !r.pending(2) })
	r.write(t, replyFrame(2, 1))
	r.closedWith(t, ErrBadFrame)
}

// FR-0075: what R2-0100's own fix (forget the call when its caller goes offline) would break. A REPLY the
// runner sequenced before the drop arrives in its replay and must find its tombstone; a carried call whose
// CALL this side replays is answered on the new link, after the runner's first PONG, and must find its
// tombstone too. Neither may close the resumed session.
func TestResumeKeepsAReplayedReplyAndAReplayedCall(t *testing.T) {
	t.Run("REPLY in the runner's replay", func(t *testing.T) {
		r := startedRaw(t, carriedCall(), 1)
		r.write(t, replyFrame(2, 1))
		r.write(t, pong())
		r.handled(t, 1)
		if !r.p.Online() || r.pending(2) {
			t.Fatalf("a replayed REPLY: online %v (%v), still pending %v", r.p.Online(), r.p.Err(), r.pending(2))
		}
	})
	t.Run("CALL replayed by this side", func(t *testing.T) {
		r := startedRaw(t, carriedCall(), 0) // the runner never got the CALL
		if f := r.readType(t, TypeCall); f.Stream != 2 {
			t.Fatalf("replayed CALL on stream %d, want 2", f.Stream)
		}
		r.write(t, pong())
		r.write(t, replyFrame(2, 1))
		r.handled(t, 1)
		if !r.p.Online() || r.pending(2) {
			t.Fatalf("the REPLY to a replayed CALL: online %v (%v), still pending %v", r.p.Online(), r.p.Err(), r.pending(2))
		}
	})
}

// FUG-L202: a CALL whose caller gave up while it waited for replay room was never sequenced, so nothing
// can answer it and it leaves no tombstone counting toward MaxCalls.
func TestCallNeverSequencedLeavesNoTombstone(t *testing.T) {
	buf := NewReplayBuffer()
	_ = buf.Reserve(context.Background(), ReplayBytes) // the runner has acknowledged nothing: no room left
	r := newRaw(t, PeerConfig{Org: true, Replay: buf})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := r.p.Call(ctx, "echo", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call = %v, want its deadline while it waited for replay room", err)
	}
	r.p.mu.Lock()
	n := len(r.p.calls)
	r.p.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d tombstones for a CALL that never left", n)
	}
}

// FR-0076: events for a call whose caller already returned are dropped, not queued for nobody until the
// queue's bound closes the session.
func TestCancelledCallDropsItsEvents(t *testing.T) {
	r := newRaw(t, PeerConfig{Org: true})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- r.p.Call(ctx, "wait", nil, nil, WithEvents(func(Event) {})) }()
	call := r.readType(t, TypeCall)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	data, _ := json.Marshal(strings.Repeat("w", 600<<10))
	for seq := uint64(1); seq <= 2; seq++ {
		b, _ := json.Marshal(Event{Kind: EventWaiting, Call: call.Stream, Data: data})
		r.write(t, Frame{Type: TypeEvent, Seq: seq, Payload: b})
	}
	r.handled(t, 2)
	if !r.p.Online() {
		t.Fatalf("events for a cancelled call closed the session: %v", r.p.Err())
	}
}

// FR-0077: a result too large for one frame still answers the call, with an internal error, instead of
// leaving the caller to wait for its own deadline.
func TestUnsendableReplyStillAnswersTheCall(t *testing.T) {
	big := strings.Repeat("r", MaxControlFrame)
	r := newRaw(t, PeerConfig{OnCall: func(context.Context, uint32, string, json.RawMessage) (any, error) { return big, nil }})
	r.write(t, callFrame(2, 1))
	rep := r.readType(t, TypeReply)
	var got Reply
	if err := json.Unmarshal(rep.Payload, &got); err != nil || rep.Stream != 2 || got.Error == nil || got.Error.Code != CodeInternal ||
		!strings.Contains(got.Error.Message, "could not be sent") {
		t.Fatalf("REPLY = stream %d %+v %v; want an internal error answering call 2", rep.Stream, got, err)
	}
}
