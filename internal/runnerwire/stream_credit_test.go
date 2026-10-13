// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

func avail(c *Credit) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.avail
}

// readAllWithin reads s to EOF, failing the test if the bytes stop arriving: a
// writer blocked on connection credit never finishes on its own.
func readAllWithin(t *testing.T, s *Stream, what string) []byte {
	t.Helper()
	type res struct {
		b   []byte
		err error
	}
	ch := make(chan res, 1)
	go func() { b, err := io.ReadAll(s); ch <- res{b, err} }()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatalf("%s: read: %v", what, r.err)
		}
		return r.b
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: the writer stopped: connection credit was not returned", what)
		return nil
	}
}

// connWindowWithin sums the connection WINDOW increments r receives until they
// reach want or limit passes.
func connWindowWithin(t *testing.T, r *rawPeer, want int64, limit time.Duration) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	var got int64
	for got < want {
		f, err := r.raw.ReadFrame(ctx)
		if err != nil {
			return got
		}
		if f.Type == TypeWindow && f.Stream == 0 {
			n, _ := DecodeWindow(f.Payload)
			got += int64(n)
		}
	}
	return got
}

// Characterization: a live stream returns credit in StreamWindow/4 grants and
// the connection's in ConnWindow/4 batches of every byte read. What is not yet
// returned sits in the reader's unacked tally (stream) or the peer's connPend
// (connection); none of it is lost.
func TestStreamCreditLiveStreamAccounting(t *testing.T) {
	accepted := make(chan *Stream, 1)
	p := newPair(t, PeerConfig{}, PeerConfig{OnOpen: func(s *Stream, o Open) error { accepted <- s; return nil }})
	s, err := p.org.Open(ctxT(t), Open{Kind: KindPTY})
	if err != nil {
		t.Fatal(err)
	}
	run := <-accepted
	// Reads in 4 KiB blocks make every stream grant exactly StreamWindow/4: 18
	// grants and 5 blocks unacked. The connection counts all of it: one batch of
	// ConnWindow/4 sent, 2 grants and the 5 blocks pending.
	const block = 4 << 10
	const total = 18*StreamWindow/4 + 5*block
	go func() { _, _ = run.Write(make([]byte, total)) }()
	for read := 0; read < total; read += block {
		if _, err := io.ReadFull(s, make([]byte, block)); err != nil {
			t.Fatal(err)
		}
	}
	var unacked, pend int64
	waitFor(t, 3*time.Second, "the WINDOW frames to settle", func() bool {
		s.mu.Lock()
		unacked = s.unacked
		s.mu.Unlock()
		p.org.mu.Lock()
		pend = p.org.connPend
		p.org.mu.Unlock()
		return avail(run.send)+unacked == StreamWindow && avail(p.run.connSend)+pend == ConnWindow
	})
	if unacked != 5*block || pend != 2*StreamWindow/4+5*block {
		t.Fatalf("unacked = %d, connPend = %d; want %d and %d", unacked, pend, 5*block, 2*StreamWindow/4+5*block)
	}
	go func() { _, _ = run.Write([]byte("x")) }()
	if _, err := io.ReadFull(s, make([]byte, 1)); err != nil {
		t.Fatalf("the live stream stopped: %v", err)
	}
}

// Many live streams, each read below its StreamWindow/4 grant threshold, still
// return connection credit: 128 streams x 32 KiB fill the connection window,
// the far side reads them all, and a further write must not wait for any
// stream to end.
func TestStreamCreditReturnedUnderManyLiveStreams(t *testing.T) {
	const streams, size = 128, 32 << 10
	if streams*size < ConnWindow || size >= StreamWindow/4 || streams > MaxStreams {
		t.Fatalf("%d streams x %d bytes no longer fills the connection window below the stream grant", streams, size)
	}
	accepted := make(chan *Stream, streams)
	p := newPair(t, PeerConfig{}, PeerConfig{OnOpen: func(s *Stream, o Open) error { accepted <- s; return nil }})
	org := make([]*Stream, streams)
	run := make([]*Stream, streams)
	for i := range streams {
		s, err := p.org.Open(ctxT(t), Open{Kind: KindPTY})
		if err != nil {
			t.Fatalf("stream %d: open: %v", i, err)
		}
		org[i], run[i] = s, <-accepted
	}
	for i, s := range run {
		if _, err := s.Write(make([]byte, size)); err != nil {
			t.Fatalf("stream %d: write: %v", i, err)
		}
	}
	for i, s := range org {
		if _, err := io.ReadFull(s, make([]byte, size)); err != nil {
			t.Fatalf("stream %d: read: %v", i, err)
		}
	}
	werr := make(chan error, 1)
	go func() { _, err := run[0].Write([]byte("x")); werr <- err }()
	select {
	case err := <-werr:
		if err != nil {
			t.Fatalf("further write: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a further write waited on connection credit the reads never returned")
	}
	if _, err := io.ReadFull(org[0], make([]byte, 1)); err != nil {
		t.Fatalf("the further byte did not arrive: %v", err)
	}
	if !p.org.Online() || !p.run.Online() {
		t.Fatal("the connection closed")
	}
}

// Characterization: the close and reset paths the credit fix must not alter.
func TestStreamCreditCloseAndResetPaths(t *testing.T) {
	t.Run("a reset wakes a writer blocked on credit", func(t *testing.T) {
		accepted := make(chan *Stream, 1)
		p := newPair(t, PeerConfig{}, PeerConfig{OnOpen: func(s *Stream, o Open) error { accepted <- s; return nil }})
		s, err := p.org.Open(ctxT(t), Open{Kind: KindPTY})
		if err != nil {
			t.Fatal(err)
		}
		run := <-accepted
		werr := make(chan error, 1)
		go func() { _, err := run.Write(make([]byte, StreamWindow+1)); werr <- err }()
		waitFor(t, 3*time.Second, "the writer to exhaust its stream window", func() bool { return avail(run.send) == 0 })
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		var re *ResetError
		select {
		case err := <-werr:
			if !errors.As(err, &re) || re.Code != ResetCancelled {
				t.Fatalf("blocked write = %v, want the reset", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("the reset did not wake the blocked writer")
		}
		waitFor(t, 3*time.Second, "both tables to drop the stream", func() bool {
			return p.org.StreamCount() == 0 && p.run.StreamCount() == 0
		})
		if !p.org.Online() || !p.run.Online() {
			t.Fatal("a stream reset closed the connection")
		}
	})
	t.Run("graceful half-closes deliver every byte then EOF", func(t *testing.T) {
		p := newPair(t, PeerConfig{}, PeerConfig{OnOpen: func(s *Stream, o Open) error {
			go func() { _, _ = s.Write([]byte("hello")); _ = s.CloseWrite() }()
			return nil
		}})
		s, err := p.org.Open(ctxT(t), Open{Kind: KindExec})
		if err != nil {
			t.Fatal(err)
		}
		if got := readAllWithin(t, s, "exec output"); string(got) != "hello" {
			t.Fatalf("read %q", got)
		}
		if err := s.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-s.Done():
		case <-time.After(3 * time.Second):
			t.Fatal("both half-closes did not finish the stream")
		}
		if err := s.Err(); err != nil {
			t.Fatalf("graceful close Err = %v", err)
		}
		waitFor(t, 3*time.Second, "both tables to drop the stream", func() bool {
			return p.org.StreamCount() == 0 && p.run.StreamCount() == 0
		})
	})
}

// R2-0014: every delivered byte is returned to the sender's connection window
// however the stream ends. Each stream here stays under the StreamWindow/4 grant
// threshold, as an exec's output does, and together they carry more than
// ConnWindow; a leak of the tails blocks a later stream's writer for good.
func TestStreamCreditReturnedWhenShortStreamsEnd(t *testing.T) {
	const size = 60 << 10
	rounds := ConnWindow/size + 8
	for _, tc := range []struct {
		name string
		end  func(t *testing.T, s *Stream, what string)
	}{
		{"read to EOF then reset", func(t *testing.T, s *Stream, what string) {
			readAllWithin(t, s, what)
			_ = s.Close()
		}},
		{"read to EOF then both half-closed", func(t *testing.T, s *Stream, what string) {
			readAllWithin(t, s, what)
			_ = s.CloseWrite()
		}},
		{"reset with the bytes unread", func(t *testing.T, s *Stream, what string) {
			waitFor(t, 3*time.Second, what+": the writer stopped: connection credit was not returned", func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()
				return s.remoteClosed
			})
			_ = s.Close()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newPair(t, PeerConfig{}, PeerConfig{OnOpen: func(s *Stream, o Open) error {
				go func() { _, _ = s.Write(make([]byte, size)); _ = s.CloseWrite() }()
				return nil
			}})
			for i := range rounds {
				s, err := p.org.Open(ctxT(t), Open{Kind: KindExec})
				if err != nil {
					t.Fatalf("stream %d: open: %v", i, err)
				}
				tc.end(t, s, fmt.Sprintf("stream %d", i))
			}
			waitFor(t, 3*time.Second, "the runner's connection window to refill", func() bool {
				return avail(p.run.connSend) == ConnWindow
			})
		})
	}
}

// R2-0014: DATA the read loop delivers to a stream already marked reset (Reset
// has aborted it and not yet dropped it from the table) is discarded, and its
// bytes go back to the sender's connection window.
func TestStreamCreditReturnedForDataOnAResetStream(t *testing.T) {
	opened := make(chan *Stream, 1)
	r := newRaw(t, PeerConfig{Org: true, OnOpen: func(s *Stream, o Open) error { opened <- s; return nil }})
	open, _ := json.Marshal(Open{Kind: KindRelay})
	r.write(t, Frame{Type: TypeOpen, Stream: 1, Payload: open})
	r.readType(t, TypeOpenOK)
	s := <-opened
	s.abort(ResetCancelled)
	r.write(t, Frame{Type: TypeData, Stream: 1, Payload: bytes.Repeat([]byte{1}, 1000)})
	if got := connWindowWithin(t, r, 1000, 2*time.Second); got != 1000 {
		t.Fatalf("connection WINDOW returned %d bytes, want 1000", got)
	}
}

// R2-0014: DATA the peer sent before it saw this side's RESET reaches a stream
// already gone from the table; it is dropped and its bytes go back to the
// sender's connection window.
func TestStreamCreditReturnedForDataInFlightPastALocalReset(t *testing.T) {
	opened := make(chan *Stream, 1)
	r := newRaw(t, PeerConfig{Org: true, OnOpen: func(s *Stream, o Open) error { opened <- s; return nil }})
	open, _ := json.Marshal(Open{Kind: KindRelay})
	r.write(t, Frame{Type: TypeOpen, Stream: 1, Payload: open})
	r.readType(t, TypeOpenOK)
	s := <-opened
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r.readType(t, TypeReset)
	r.write(t, Frame{Type: TypeData, Stream: 1, Payload: bytes.Repeat([]byte{1}, 1000)})
	if got := connWindowWithin(t, r, 1000, 2*time.Second); got != 1000 {
		t.Fatalf("connection WINDOW returned %d bytes, want 1000", got)
	}
}

// A DATA frame refused for overflowing its stream's window is not held, so its
// bytes go back to the sender's connection window along with the buffer the
// reset discards.
func TestStreamCreditReturnedForDataRefusedPastTheStreamWindow(t *testing.T) {
	r := newRaw(t, PeerConfig{Org: true, OnOpen: func(*Stream, Open) error { return nil }})
	open, _ := json.Marshal(Open{Kind: KindRelay})
	r.write(t, Frame{Type: TypeOpen, Stream: 1, Payload: open})
	r.readType(t, TypeOpenOK)
	chunk := bytes.Repeat([]byte{1}, MaxDataFrame)
	for range StreamWindow / MaxDataFrame {
		r.write(t, Frame{Type: TypeData, Stream: 1, Payload: chunk})
	}
	r.write(t, Frame{Type: TypeData, Stream: 1, Payload: bytes.Repeat([]byte{1}, 1000)})
	want := int64(StreamWindow + 1000)
	if got := connWindowWithin(t, r, want, 2*time.Second); got != want {
		t.Fatalf("connection WINDOW returned %d bytes, want %d", got, want)
	}
	if !r.p.Online() {
		t.Fatal("one stream's overflow closed the connection")
	}
}

// TestStreamCreditDrainAfterBothClosedCreditsOnce: the output relay half-closes
// its side first and drains the buffer only after both ends have closed, by
// which point the unread bytes were already returned to the sender when the
// stream ended. Reading them must not return that credit a second time.
func TestStreamCreditDrainAfterBothClosedCreditsOnce(t *testing.T) {
	const size = 100 << 10
	accepted := make(chan *Stream, 1)
	p := newPair(t, PeerConfig{}, PeerConfig{OnOpen: func(s *Stream, o Open) error { accepted <- s; return nil }})
	s, err := p.org.Open(ctxT(t), Open{Kind: KindExec})
	if err != nil {
		t.Fatal(err)
	}
	run := <-accepted
	_ = s.CloseWrite()
	if _, err := run.Write(make([]byte, size)); err != nil {
		t.Fatal(err)
	}
	_ = run.CloseWrite()
	select {
	case <-s.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the stream did not end after both sides closed")
	}
	if got := readAllWithin(t, s, "the drain after both closed"); len(got) != size {
		t.Fatalf("drained %d bytes, want %d", len(got), size)
	}
	owed := func() int64 {
		p.org.mu.Lock()
		defer p.org.mu.Unlock()
		return avail(p.run.connSend) + p.org.connPend
	}
	waitFor(t, 3*time.Second, "the sender's credit to come back", func() bool { return owed() >= ConnWindow })
	time.Sleep(200 * time.Millisecond) // let any second return land
	if got := owed(); got != ConnWindow {
		t.Fatalf("sender window + withheld credit = %d, want %d: the drained bytes were credited twice", got, ConnWindow)
	}
}
