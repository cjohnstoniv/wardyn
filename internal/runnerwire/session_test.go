// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"
)

var testTime = time.Unix(1700000000, 0)

func TestAuthSignatureIsBoundToConnectionRunnerAndOrg(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	nonce := make([]byte, 32)
	rand.Read(nonce)
	sig := SignAuth(key, nonce, "r1", "orgsha")
	if !VerifyAuth(pub, nonce, "r1", "orgsha", sig) {
		t.Fatal("a good signature was refused")
	}
	other := make([]byte, 32)
	rand.Read(other)
	for name, ok := range map[string]bool{
		"other nonce":  VerifyAuth(pub, other, "r1", "orgsha", sig),
		"other runner": VerifyAuth(pub, nonce, "r2", "orgsha", sig),
		"other org":    VerifyAuth(pub, nonce, "r1", "other", sig),
		"short nonce":  VerifyAuth(pub, nonce[:16], "r1", "orgsha", sig),
		"no key":       VerifyAuth(nil, nonce, "r1", "orgsha", sig),
	} {
		if ok {
			t.Errorf("%s verified", name)
		}
	}
}

func TestPostAuthSequence(t *testing.T) {
	fresh := PostAuthSequence(false)
	if len(fresh) != 3 || fresh[0] != TypePending || fresh[1] != TypeState || fresh[2] != TypeReady {
		t.Fatalf("a fresh session runs PENDING, STATE, READY; got %v", fresh)
	}
	if got := PostAuthSequence(true); len(got) != 0 {
		t.Fatalf("a resumed session skips PENDING, STATE and READY; got %v", got)
	}
}

func TestStreamIDSpace(t *testing.T) {
	org, run := NewIDAllocator(true), NewIDAllocator(false)
	for range 5 {
		if id := org.Next(); id == 0 || id%2 != 0 {
			t.Fatalf("org id %d is not even", id)
		}
		if id := run.Next(); id%2 != 1 {
			t.Fatalf("runner id %d is not odd", id)
		}
	}
	if _, ok := InitiatedByOrg(0); ok {
		t.Fatal("the control stream has no owner")
	}
}

func TestCreditBlocksAndDropsNothing(t *testing.T) {
	conn := NewConnCredit()
	s := NewStreamCredit(conn)
	ctx := context.Background()
	n, err := s.Take(ctx, 1<<30)
	if err != nil || n != StreamWindow {
		t.Fatalf("first take = %d, %v; want the stream window", n, err)
	}
	got := make(chan int, 1)
	go func() { n, _ := s.Take(ctx, 100); got <- n }()
	select {
	case <-got:
		t.Fatal("a writer with no credit did not block")
	case <-time.After(50 * time.Millisecond):
	}
	s.Add(40)
	if n := <-got; n != 40 {
		t.Fatalf("a writer got %d of 40 credit granted", n)
	}
}

func TestCreditConnectionWindowBoundsEveryStream(t *testing.T) {
	conn := NewConnCredit()
	a, b := NewStreamCredit(conn), NewStreamCredit(conn)
	ctx := context.Background()
	taken := 0
	for taken < ConnWindow {
		n, err := a.Take(ctx, StreamWindow)
		if err != nil {
			t.Fatal(err)
		}
		taken += n
		a.Add(int64(n)) // the stream is replenished; only the connection is exhausted
	}
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := b.Take(short, 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("another stream sent past the connection window: %v", err)
	}
	conn.Add(10)
	if n, err := b.Take(ctx, 100); err != nil || n != 10 {
		t.Fatalf("take after a connection WINDOW = %d, %v", n, err)
	}
}

func TestCreditCloseFailsWriters(t *testing.T) {
	s := NewStreamCredit(NewConnCredit())
	if _, err := s.Take(context.Background(), StreamWindow); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { _, err := s.Take(context.Background(), 1); errc <- err }()
	time.Sleep(20 * time.Millisecond)
	s.Close()
	if err := <-errc; !errors.Is(err, ErrCreditClosed) {
		t.Fatalf("blocked writer = %v", err)
	}
}

func TestReplayBufferAckAndResume(t *testing.T) {
	b := NewReplayBuffer()
	ctx := context.Background()
	var seqs []uint64
	for range 5 {
		if err := b.Reserve(ctx, 10); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, b.Assign(Frame{Type: TypeEvent, Payload: make([]byte, 10)}).Seq)
	}
	if seqs[0] != 1 || seqs[4] != 5 {
		t.Fatalf("sequence numbers %v do not start at 1", seqs)
	}
	got, err := b.Since(2)
	if err != nil || len(got) != 3 || got[0].Seq != 3 {
		t.Fatalf("Since(2) = %v, %v", got, err)
	}
	b.Ack(3)
	if _, err := b.Since(1); !errors.Is(err, ErrResumeGap) {
		t.Fatalf("resume before the ack point = %v, want ErrResumeGap", err)
	}
	if got, err := b.Since(3); err != nil || len(got) != 2 {
		t.Fatalf("Since(3) after ack(3) = %v, %v", got, err)
	}
	if _, err := b.Since(99); !errors.Is(err, ErrResumeGap) {
		t.Fatalf("resume past what was sent = %v", err)
	}
}

func TestReplayBufferWaitsForAckAtOneMiB(t *testing.T) {
	b := NewReplayBuffer()
	ctx := context.Background()
	if err := b.Reserve(ctx, ReplayBytes); err != nil {
		t.Fatal(err)
	}
	b.Assign(Frame{Type: TypeEvent, Payload: make([]byte, ReplayBytes)})
	done := make(chan error, 1)
	go func() { done <- b.Reserve(ctx, 1) }()
	select {
	case <-done:
		t.Fatal("reserved past the 1 MiB replay buffer")
	case <-time.After(50 * time.Millisecond):
	}
	b.Ack(1)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := b.Reserve(ctx, ReplayBytes+1); !errors.Is(err, ErrReplayFull) {
		t.Fatalf("a frame larger than the buffer = %v", err)
	}
}
