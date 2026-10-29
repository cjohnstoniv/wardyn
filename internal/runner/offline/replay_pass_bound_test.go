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
	// Each pass is numbered by the replayTick hook, which the loop calls as a pass begins, so a send is
	// counted against the pass that made it, whatever the test's own goroutine has reached by then.
	type key struct {
		pass int
		id   uuid.UUID
	}
	var mu sync.Mutex
	pass := 0
	began := map[int]time.Time{}
	calls := map[key]int{}
	tick := func() {
		mu.Lock()
		defer mu.Unlock()
		pass++
		began[pass] = time.Now()
	}
	send := func(ctx context.Context, evs []runnerwire.Event) error {
		id := *evs[0].RunID
		mu.Lock()
		calls[key{pass, id}]++
		mu.Unlock()
		if id == hung {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	sent := func(p int, id uuid.UUID) int {
		mu.Lock()
		defer mu.Unlock()
		return calls[key{p, id}]
	}
	fixed := time.Now() // the fake clock stands still, so the hung run's back-off never lapses
	ticks := make(chan time.Time)
	m.mu.Lock()
	m.now, m.replayTicks, m.replaySend, m.replayTick = func() time.Time { return fixed }, ticks, send, tick
	m.peer, m.online, m.replayReady = &runnerwire.Peer{}, true, true
	m.mu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { m.replayLoop(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	select {
	case ticks <- time.Time{}: // taken once the first pass has ended
	case <-time.After(5 * time.Second):
		t.Fatal("the first pass is still held by a send that never answers")
	}
	mu.Lock()
	took := time.Since(began[1])
	mu.Unlock()
	if took < bound {
		t.Errorf("the first pass ended after %v, before the %v bound", took, bound)
	}
	if n := sent(1, later); n != 0 {
		t.Errorf("the later run was sent %d times in the pass that timed out, want 0", n)
	}
	select {
	case ticks <- time.Time{}: // taken once the second pass has ended
	case <-time.After(5 * time.Second):
		t.Fatal("the second pass did not end")
	}
	if n := sent(2, later); n != 1 {
		t.Errorf("the later run was sent %d times by the second pass, want 1", n)
	}
	if n := sent(1, hung) + sent(2, hung); n != 1 {
		t.Errorf("the hung run was sent %d times, want 1 (backed off in the second pass)", n)
	}
}
