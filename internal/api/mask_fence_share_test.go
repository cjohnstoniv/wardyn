// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

const fenceTestBeat = 5 * time.Millisecond

func fenceRuns(s *Server) int {
	s.maskFences.mu.Lock()
	defer s.maskFences.mu.Unlock()
	return len(s.maskFences.runs)
}

func TestOneMaskFenceReadPerRunWhateverItsConsumers(t *testing.T) {
	s, _, _ := maskDoorServer(t)
	s.maskBeat = fenceTestBeat
	var reads, ends atomic.Int32
	arrived := make(chan struct{}, 1)
	release := make(chan bool)
	s.maskFences.read = func(ctx context.Context, _ uuid.UUID) bool {
		reads.Add(1)
		select {
		case arrived <- struct{}{}:
		default:
		}
		select {
		case ok := <-release:
			return ok
		case <-ctx.Done():
			return true
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := uuid.New()
	for range 8 {
		s.holdMaskFence(ctx, run, func() { ends.Add(1) })
	}

	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("no fence read arrived")
	}
	time.Sleep(10 * fenceTestBeat)
	if n := reads.Load(); n != 1 {
		t.Fatalf("8 consumers of one run made %d fence reads while one was in flight, want 1", n)
	}

	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case release <- true:
			case <-done:
				return
			}
		}
	}()
	time.Sleep(20 * fenceTestBeat)
	n := reads.Load() - 1
	if n < 1 || n > 30 { // one per beat, at most; 8 per beat would be about 160
		t.Fatalf("%d fence reads in 20 beats with 8 consumers, want about one per beat", n)
	}
	if ends.Load() != 0 {
		t.Fatalf("%d consumers ended although the run is covered", ends.Load())
	}
	t.Logf("fence reads with 8 consumers: 1 while the first was in flight, %d over 20 beats after", n)
}

func TestMaskFenceMissEndsEveryLiveConsumer(t *testing.T) {
	s, _, _ := maskDoorServer(t)
	s.maskBeat = fenceTestBeat
	gate := make(chan struct{})
	s.maskFences.read = func(context.Context, uuid.UUID) bool {
		<-gate
		return false
	}
	run := uuid.New()
	var ends [8]atomic.Int32
	fenced := make([]func() bool, 8)
	for i := range 8 {
		fenced[i] = s.holdMaskFence(context.Background(), run, func() { ends[i].Add(1) })
	}
	var gone atomic.Int32
	goneCtx, goneCancel := context.WithCancel(context.Background())
	goneFenced := s.holdMaskFence(goneCtx, run, func() { gone.Add(1) })
	goneCancel()
	close(gate)

	waitFor(t, "every live consumer to be ended", func() bool {
		for i := range ends {
			if ends[i].Load() == 0 {
				return false
			}
		}
		return true
	})
	time.Sleep(5 * fenceTestBeat)
	for i := range ends {
		if n := ends[i].Load(); n != 1 {
			t.Errorf("consumer %d ended %d times, want once", i, n)
		}
		if !fenced[i]() {
			t.Errorf("consumer %d was ended but not reported fenced", i)
		}
	}
	if gone.Load() != 0 || goneFenced() {
		t.Error("a consumer that ended on its own was ended or reported fenced")
	}
	waitFor(t, "the run's loop to retire", func() bool { return fenceRuns(s) == 0 })
}

func TestMaskFenceStopsAfterTheLastConsumer(t *testing.T) {
	s, _, _ := maskDoorServer(t)
	s.maskBeat = fenceTestBeat
	var reads atomic.Int32
	s.maskFences.read = func(context.Context, uuid.UUID) bool {
		reads.Add(1)
		return true
	}
	run := uuid.New()
	cancels := make([]context.CancelFunc, 8)
	for i := range cancels {
		var ctx context.Context
		ctx, cancels[i] = context.WithCancel(context.Background())
		s.holdMaskFence(ctx, run, func() { t.Error("end called although the run is covered") })
	}
	waitFor(t, "a fence read", func() bool { return reads.Load() > 0 })
	for _, cancel := range cancels {
		cancel()
	}
	waitFor(t, "the run's loop to be dropped", func() bool { return fenceRuns(s) == 0 })
	time.Sleep(fenceTestBeat) // a beat already due when the loop stopped may still read once
	n := reads.Load()
	time.Sleep(5 * fenceTestBeat)
	if got := reads.Load(); got != n {
		t.Fatalf("%d fence reads after the last consumer left, want 0", got-n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.holdMaskFence(ctx, run, func() { t.Error("end called although the run is covered") })
	if fenceRuns(s) != 1 {
		t.Fatal("a new consumer did not start a loop for its run")
	}
	waitFor(t, "a read from the fresh loop", func() bool { return reads.Load() > n })
}
