// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

func TestStreamDonePreservesBufferedBytes(t *testing.T) {
	incoming := make(chan *Stream, 1)
	p := newPair(t, PeerConfig{OnOpen: func(s *Stream, o Open) error { incoming <- s; return nil }}, PeerConfig{})
	out, err := p.run.Open(ctxT(t), Open{Kind: KindExec})
	if err != nil {
		t.Fatal(err)
	}
	in := <-incoming
	if _, err = out.Write([]byte("retained")); err != nil {
		t.Fatal(err)
	}
	if err = out.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-in.Done():
		t.Fatal("one half-close terminated the stream")
	default:
	}
	if err = in.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-in.Done():
	case <-ctxT(t).Done():
		t.Fatal("both close did not finish")
	}
	got, err := io.ReadAll(in)
	if err != nil || string(got) != "retained" || in.Err() != nil {
		t.Fatalf("read=%q,%v, terminal=%v", got, err, in.Err())
	}
}

func TestStreamDoneSignalsReset(t *testing.T) {
	incoming := make(chan *Stream, 1)
	p := newPair(t, PeerConfig{OnOpen: func(s *Stream, o Open) error { incoming <- s; return nil }}, PeerConfig{})
	out, err := p.run.Open(ctxT(t), Open{Kind: KindExec})
	if err != nil {
		t.Fatal(err)
	}
	in := <-incoming
	if err = out.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-in.Done():
	case <-ctxT(t).Done():
		t.Fatal("reset did not finish")
	}
	if in.Err() == nil {
		t.Fatal("reset was reported as graceful")
	}
}

// A read deadline ends a Read with nothing to return, and only that Read: cleared, the stream reads on.
func TestStreamReadDeadlineLeavesTheStreamUsable(t *testing.T) {
	incoming := make(chan *Stream, 1)
	p := newPair(t, PeerConfig{OnOpen: func(s *Stream, o Open) error { incoming <- s; return nil }}, PeerConfig{})
	out, err := p.run.Open(ctxT(t), Open{Kind: KindExec})
	if err != nil {
		t.Fatal(err)
	}
	in := <-incoming
	if err = in.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = in.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read with a deadline already past = %v", err)
	}
	res := make(chan error, 1)
	go func() { _, err := in.Read(make([]byte, 1)); res <- err }()
	if err = in.SetReadDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-res:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("blocked read after its deadline = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked read ignored its deadline")
	}
	_ = in.SetReadDeadline(time.Time{})
	if _, err = out.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if n, err := in.Read(make([]byte, 1)); n != 1 || err != nil {
		t.Fatalf("read after the deadline was cleared = %d, %v", n, err)
	}
}
