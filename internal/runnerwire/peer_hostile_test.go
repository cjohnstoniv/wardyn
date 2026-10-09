// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rawPeer is a Peer under test whose other end is a bare LoopConn the test
// drives frame by frame: a peer that does not follow the protocol.
type rawPeer struct {
	p   *Peer
	raw *LoopConn
}

func newRaw(t *testing.T, cfg PeerConfig) *rawPeer {
	t.Helper()
	pc, rc := Loopback()
	p := NewPeer(pc, cfg)
	go p.Run(context.Background())
	t.Cleanup(p.Close)
	return &rawPeer{p: p, raw: rc}
}

func (r *rawPeer) write(t *testing.T, f Frame) {
	t.Helper()
	if err := r.raw.WriteFrame(ctxT(t), f); err != nil {
		t.Fatalf("write %s: %v", f.Type, err)
	}
}

func (r *rawPeer) read(t *testing.T) Frame {
	t.Helper()
	f, err := r.raw.ReadFrame(ctxT(t))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return f
}

// readType reads until a frame of type ty, skipping others.
func (r *rawPeer) readType(t *testing.T, ty Type) Frame {
	t.Helper()
	for {
		if f := r.read(t); f.Type == ty {
			return f
		}
	}
}

func (r *rawPeer) closedWith(t *testing.T, want error) {
	t.Helper()
	select {
	case <-r.p.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the session did not close")
	}
	if err := r.p.Err(); !errors.Is(err, want) {
		t.Fatalf("session closed with %v, want %v", err, want)
	}
}

func replyFrame(id uint32, seq uint64) Frame {
	b, _ := json.Marshal(Reply{})
	return Frame{Type: TypeReply, Stream: id, Seq: seq, Payload: b}
}

func callFrame(id uint32, seq uint64) Frame {
	b, _ := json.Marshal(Call{Method: "m"})
	return Frame{Type: TypeCall, Stream: id, Seq: seq, Payload: b}
}

// F1: a second REPLY for one call used to block the read loop on the one-slot
// reply channel, with the session still "online".
func TestDuplicateReplyClosesTheSessionNotTheReadLoop(t *testing.T) {
	r := newRaw(t, PeerConfig{Org: true})
	errc := make(chan error, 1)
	go func() { errc <- r.p.Call(ctxT(t), "echo", nil, nil) }()
	call := r.readType(t, TypeCall)
	r.write(t, replyFrame(call.Stream, 1))
	r.write(t, replyFrame(call.Stream, 2))
	r.write(t, replyFrame(call.Stream, 3))
	if err := <-errc; err != nil {
		t.Fatalf("the first REPLY should complete the call: %v", err)
	}
	r.closedWith(t, ErrBadFrame)
}

func TestReplyForACallNotPendingClosesTheSession(t *testing.T) {
	r := newRaw(t, PeerConfig{Org: true})
	r.write(t, replyFrame(2, 1))
	r.closedWith(t, ErrBadFrame)
}

// A caller that gave up still gets the runner's late REPLY, which is not an error.
func TestLateReplyToACancelledCallIsBenign(t *testing.T) {
	r := newRaw(t, PeerConfig{Org: true})
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- r.p.Call(ctx, "wait", nil, nil) }()
	call := r.readType(t, TypeCall)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	r.write(t, replyFrame(call.Stream, 1))
	short, stop := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer stop()
	if err := r.p.Call(short, "again", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a following call = %v, want it to wait for its own reply", err)
	}
	if !r.p.Online() {
		t.Fatalf("a late REPLY to a cancelled call closed the session: %v", r.p.Err())
	}
}

// F2a: an OPEN for a call that is not in flight is refused, never parked.
func TestOpenForACallNotInFlightIsRefused(t *testing.T) {
	r := newRaw(t, PeerConfig{Org: true})
	open, _ := json.Marshal(Open{Kind: KindPTY, Call: 999999})
	r.write(t, Frame{Type: TypeOpen, Stream: 1, Payload: open})
	rst := r.readType(t, TypeReset)
	if code, _ := DecodeReset(rst.Payload); rst.Stream != 1 || code != ResetRefused {
		t.Fatalf("RESET = stream %d code %d, want stream 1 refused", rst.Stream, code)
	}
	r.p.mu.Lock()
	parked := len(r.p.parked) + len(r.p.byCall)
	r.p.mu.Unlock()
	if parked != 0 {
		t.Fatalf("%d streams parked for a call that is not in flight", parked)
	}
}

// F2b: a peer that ignores credit and sends past the connection window ends the session.
func TestSendingPastTheConnectionWindowClosesTheSession(t *testing.T) {
	r := newRaw(t, PeerConfig{Org: true, OnOpen: func(*Stream, Open) error { return nil }})
	open, _ := json.Marshal(Open{Kind: KindRelay})
	chunk := bytes.Repeat([]byte{1}, MaxDataFrame)
	streams := ConnWindow/StreamWindow + 2 // every stream within its own window; the sum is not
	for i := range streams {
		id := uint32(2*i + 1)
		r.write(t, Frame{Type: TypeOpen, Stream: id, Payload: open})
		for range StreamWindow / MaxDataFrame {
			if err := r.raw.WriteFrame(ctxT(t), Frame{Type: TypeData, Stream: id, Payload: chunk}); err != nil {
				break // the org already closed the session
			}
		}
	}
	r.closedWith(t, ErrBadFrame)
}

func TestSendingPastOneStreamWindowResetsThatStreamOnly(t *testing.T) {
	r := newRaw(t, PeerConfig{Org: true, OnOpen: func(*Stream, Open) error { return nil }})
	open, _ := json.Marshal(Open{Kind: KindRelay})
	r.write(t, Frame{Type: TypeOpen, Stream: 1, Payload: open})
	chunk := bytes.Repeat([]byte{1}, MaxDataFrame)
	for range StreamWindow/MaxDataFrame + 1 {
		r.write(t, Frame{Type: TypeData, Stream: 1, Payload: chunk})
	}
	rst := r.readType(t, TypeReset)
	if code, _ := DecodeReset(rst.Payload); rst.Stream != 1 || code != ResetInternal {
		t.Fatalf("RESET = stream %d code %d", rst.Stream, code)
	}
	if !r.p.Online() {
		t.Fatal("one stream's overflow closed the connection")
	}
}

// F2c: a connection holds MaxStreams streams; the next OPEN is refused.
func TestMaxStreamsRefusesTheNextOpen(t *testing.T) {
	r := newRaw(t, PeerConfig{Org: true, OnOpen: func(*Stream, Open) error { return nil }})
	open, _ := json.Marshal(Open{Kind: KindRelay})
	for i := range MaxStreams + 1 {
		r.write(t, Frame{Type: TypeOpen, Stream: uint32(2*i + 1), Payload: open})
	}
	rst := r.readType(t, TypeReset)
	if code, _ := DecodeReset(rst.Payload); rst.Stream != uint32(2*MaxStreams+1) || code != ResetRefused {
		t.Fatalf("RESET = stream %d code %d, want stream %d refused", rst.Stream, code, 2*MaxStreams+1)
	}
	r.p.mu.Lock()
	n := len(r.p.streams) + len(r.p.parked)
	r.p.mu.Unlock()
	if n != MaxStreams {
		t.Fatalf("%d streams held, want %d", n, MaxStreams)
	}
}

// F3: a replayed sequenced frame runs once; a gap ends the session.
func TestReplayedSequencedFrameIsDroppedAndReACKed(t *testing.T) {
	var runs atomic.Int32
	r := newRaw(t, PeerConfig{Org: false, OnCall: func(context.Context, uint32, string, json.RawMessage) (any, error) {
		runs.Add(1)
		return nil, nil
	}})
	r.write(t, callFrame(2, 1))
	r.readType(t, TypeReply)
	r.write(t, callFrame(2, 1)) // the same frame again, as a lost ACK would replay it
	ack := r.readType(t, TypeAck)
	if seq, _ := DecodeAck(ack.Payload); seq != 1 {
		t.Fatalf("re-ACK = %d, want 1", seq)
	}
	time.Sleep(50 * time.Millisecond)
	if n := runs.Load(); n != 1 {
		t.Fatalf("the replayed CALL ran %d times, want once", n)
	}
	if !r.p.Online() {
		t.Fatal("a replay closed the session")
	}
}

func TestSequenceGapClosesTheSession(t *testing.T) {
	r := newRaw(t, PeerConfig{Org: false, OnCall: func(context.Context, uint32, string, json.RawMessage) (any, error) { return nil, nil }})
	r.write(t, callFrame(2, 1))
	r.write(t, callFrame(4, 3)) // 2 never came
	r.closedWith(t, ErrBadFrame)
}

func TestPeerConfigResumesSequencing(t *testing.T) {
	var runs atomic.Int32
	r := newRaw(t, PeerConfig{Org: false, LastReceived: 5, OnCall: func(context.Context, uint32, string, json.RawMessage) (any, error) {
		runs.Add(1)
		return nil, nil
	}})
	r.write(t, callFrame(2, 5)) // already received on the previous connection
	r.write(t, callFrame(4, 6))
	r.readType(t, TypeReply)
	time.Sleep(50 * time.Millisecond)
	if n := runs.Load(); n != 1 {
		t.Fatalf("%d calls ran, want only seq 6", n)
	}
	if got := r.p.LastReceivedSeq(); got != 6 {
		t.Fatalf("LastReceivedSeq = %d", got)
	}

	// The org side of a resumed session numbers on from the carried buffer.
	buf := NewReplayBuffer()
	for range 3 {
		_ = buf.Reserve(context.Background(), 0)
		buf.Assign(Frame{Type: TypeLease})
	}
	o := newRaw(t, PeerConfig{Org: true, Replay: buf})
	if err := o.p.Send(ctxT(t), Frame{Type: TypeLease, Payload: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if lease := o.readType(t, TypeLease); lease.Seq != 4 {
		t.Fatalf("first frame after the resume has seq %d, want 4", lease.Seq)
	}
}

// F4: an ACK for a frame never sent ends the session instead of poisoning Since.
func TestAckAheadClosesTheSession(t *testing.T) {
	r := newRaw(t, PeerConfig{Org: true})
	r.write(t, Frame{Type: TypeAck, Payload: EncodeAck(99)})
	r.closedWith(t, ErrBadFrame)
}

func TestReplayBufferAckAheadIsRefusedAndKeepsResumeWorking(t *testing.T) {
	b := NewReplayBuffer()
	_ = b.Reserve(context.Background(), 1)
	b.Assign(Frame{Type: TypeEvent, Payload: []byte("x")})
	if err := b.Ack(5); !errors.Is(err, ErrAckAhead) {
		t.Fatalf("Ack(5) = %v, want ErrAckAhead", err)
	}
	if got, err := b.Since(0); err != nil || len(got) != 1 {
		t.Fatalf("Since after an ahead ACK = %v, %v", got, err)
	}
}

// F4: REPLY goroutines waiting on a full replay buffer end when the link does.
func TestReplyGoroutinesEndWhenTheLinkDrops(t *testing.T) {
	big := strings.Repeat("a", 600<<10)
	r := newRaw(t, PeerConfig{Org: false, OnCall: func(context.Context, uint32, string, json.RawMessage) (any, error) {
		return big, nil // two of these cannot both fit the 1 MiB replay buffer, and nobody ACKs
	}})
	for i := range 3 {
		r.write(t, callFrame(uint32(2*(i+1)), uint64(i+1)))
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		r.p.mu.Lock()
		n := len(r.p.inCalls)
		r.p.mu.Unlock()
		if n == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d calls in flight", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	r.raw.Drop()
	deadline = time.Now().Add(3 * time.Second)
	for {
		r.p.mu.Lock()
		n := len(r.p.inCalls)
		r.p.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d reply goroutines still blocked after the link dropped", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
