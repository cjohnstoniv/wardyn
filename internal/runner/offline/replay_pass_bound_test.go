// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
)

// A replay send the organisation never answers holds its pass only until replayPassBound: the pass
// ends, the hung run is backed off, and the runs behind it are sent in the next pass.
func TestReplayPassEndsAtTheBoundOnAHungSend(t *testing.T) {
	const bound = 100 * time.Millisecond
	prev := replayPassBound
	replayPassBound = bound
	t.Cleanup(func() { replayPassBound = prev })
	m, _, _, org := managerFixture(t)
	hung := createLocalAs(t, m, org, uuid.MustParse("00000000-0000-4000-8000-000000000001"))
	later := createLocalAs(t, m, org, uuid.MustParse("ffffffff-0000-4000-8000-000000000002"))
	for _, id := range []uuid.UUID{hung, later} {
		spool, err := runnerspool.Open(filepath.Join(m.dir, "spools", id.String()), id)
		if err != nil {
			t.Fatal(err)
		}
		appendDecision(t, spool, id)
		if err := spool.Close(); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	calls := map[uuid.UUID]int{}
	send := func(ctx context.Context, evs []runnerwire.Event) error {
		id := *evs[0].RunID
		mu.Lock()
		calls[id]++
		mu.Unlock()
		if id == hung {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	sent := func(id uuid.UUID) int {
		mu.Lock()
		defer mu.Unlock()
		return calls[id]
	}
	fixed := time.Now() // the fake clock stands still, so the hung run's back-off never lapses
	ticks := make(chan time.Time)
	m.mu.Lock()
	m.now, m.replayTicks, m.replaySend = func() time.Time { return fixed }, ticks, send
	m.peer, m.online, m.replayReady = &runnerwire.Peer{}, true, true
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { m.replayLoop(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	start := time.Now()
	select {
	case ticks <- time.Time{}: // taken once the first pass has ended
	case <-time.After(5 * time.Second):
		t.Fatal("the first pass is still held by a send that never answers")
	}
	if took := time.Since(start); took < bound {
		t.Errorf("the first pass ended after %v, before the %v bound", took, bound)
	}
	if n := sent(later); n != 0 {
		t.Errorf("the later run was sent %d times in the pass that timed out, want 0", n)
	}
	select {
	case ticks <- time.Time{}: // taken once the second pass has ended
	case <-time.After(5 * time.Second):
		t.Fatal("the second pass did not end")
	}
	if n := sent(later); n != 1 {
		t.Errorf("the later run was sent %d times by the second pass, want 1", n)
	}
	if n := sent(hung); n != 1 {
		t.Errorf("the hung run was sent %d times, want 1 (backed off in the second pass)", n)
	}
}
