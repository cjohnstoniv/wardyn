// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"
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
	start := time.Now()
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
	n := int(reads.Load() - 1)
	beats := int(time.Since(start) / fenceTestBeat)
	if n < 1 || n > beats+2 { // one per beat, at most; one per consumer would be 8 per beat
		t.Fatalf("%d fence reads in %d beats with 8 consumers, want at most one per beat", n, beats)
	}
	if ends.Load() != 0 {
		t.Fatalf("%d consumers ended although the run is covered", ends.Load())
	}
	t.Logf("fence reads with 8 consumers: 1 while the first was in flight, %d over %d beats after", n, beats)
}

// endedFirstCtx is a consumer context that reports itself ended once ended is set, but whose Done never closes,
// so the consumer's leave never runs and its sub is still on the run's loop when the miss is handled.
type endedFirstCtx struct {
	context.Context
	ended atomic.Bool
}

func (c *endedFirstCtx) Done() <-chan struct{} { return nil }

func (c *endedFirstCtx) Err() error {
	if c.ended.Load() {
		return context.Canceled
	}
	return nil
}

func TestMaskFenceMissEndsEveryLiveConsumer(t *testing.T) {
	s, _, _ := maskDoorServer(t)
	s.maskBeat = fenceTestBeat
	inRead, gate := make(chan struct{}, 1), make(chan struct{})
	s.maskFences.read = func(context.Context, uuid.UUID) bool {
		select {
		case inRead <- struct{}{}:
		default:
		}
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
	goneCtx := &endedFirstCtx{Context: context.Background()}
	goneFenced := s.holdMaskFence(goneCtx, run, func() { gone.Add(1) })
	select {
	case <-inRead:
	case <-time.After(5 * time.Second):
		t.Fatal("no fence read arrived")
	}
	goneCtx.ended.Store(true) // ends while the loop is inside the read that misses
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

// The loop does not stop with the server's context: at shutdown a consumer may stream until its own context
// ends, and it must stay fenced until then.
func TestMaskFenceOutlivesTheServerContext(t *testing.T) {
	s, _, _ := maskDoorServer(t)
	s.maskBeat = fenceTestBeat
	base, cancelBase := context.WithCancel(context.Background())
	s.cfg.BaseCtx = base
	var covered atomic.Bool
	covered.Store(true)
	var reads atomic.Int32
	s.maskFences.read = func(context.Context, uuid.UUID) bool {
		reads.Add(1)
		return covered.Load()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ended := make(chan struct{})
	fenced := s.holdMaskFence(ctx, uuid.New(), func() { close(ended) })
	waitFor(t, "a fence read", func() bool { return reads.Load() > 0 })

	cancelBase()
	time.Sleep(3 * fenceTestBeat)
	covered.Store(false)

	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("a consumer was not ended on a miss after the server's context ended")
	}
	if !fenced() {
		t.Fatal("the consumer was ended but not reported fenced")
	}
}

func TestMaskFenceStopsAfterTheLastConsumer(t *testing.T) {
	s, _, _ := maskDoorServer(t)
	s.maskBeat = fenceTestBeat
	// A read is counted only while its loop is live, checked and counted under one lock: a count taken under
	// that lock after the loop stopped is final, whatever beat was due.
	var mu sync.Mutex
	reads := 0
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return reads
	}
	s.maskFences.read = func(ctx context.Context, _ uuid.UUID) bool {
		mu.Lock()
		defer mu.Unlock()
		if ctx.Err() == nil {
			reads++
		}
		return true
	}
	run := uuid.New()
	cancels := make([]context.CancelFunc, 8)
	for i := range cancels {
		var ctx context.Context
		ctx, cancels[i] = context.WithCancel(context.Background())
		s.holdMaskFence(ctx, run, func() { t.Error("end called although the run is covered") })
	}
	waitFor(t, "a fence read", func() bool { return count() > 0 })
	for _, cancel := range cancels {
		cancel()
	}
	waitFor(t, "the run's loop to be dropped", func() bool { return fenceRuns(s) == 0 })
	n := count()
	time.Sleep(5 * fenceTestBeat)
	if got := count(); got != n {
		t.Fatalf("%d fence reads after the last consumer left, want 0", got-n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.holdMaskFence(ctx, run, func() { t.Error("end called although the run is covered") })
	if fenceRuns(s) != 1 {
		t.Fatal("a new consumer did not start a loop for its run")
	}
	waitFor(t, "a read from the fresh loop", func() bool { return count() > n })
}
