// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"io"
	"testing"
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
