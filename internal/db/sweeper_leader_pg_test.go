// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Postgres-backed tests for SweeperLeader: one elected leader runs each gated
// sweep, a stopped leader hands over within a retry, and a lock lost under a
// still-running leader (a failover) cancels and joins its sweeps while the
// epoch moves on. Two pools stand in for two replicas: advisory locks are
// re-entrant within a session, so one pool would prove the opposite.
//
// Guarded by WARDYN_TEST_PG, like every other *_pg_test.go here.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fastLeader is a leader on its own pool with every interval shrunk, so a
// handover is observable in well under a second.
func fastLeader(t *testing.T, name string) *SweeperLeader {
	t.Helper()
	l := NewSweeperLeader(pgPool(t), name)
	l.retry, l.monitor, l.poll = 100*time.Millisecond, 50*time.Millisecond, 10*time.Millisecond
	return l
}

func waitFor(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// tickSweep is a gated sweep that ticks every few milliseconds, counting its
// ticks in n and the sweeps running at once in running (max in peak).
func tickSweep(n, running, peak *atomic.Int64) func(context.Context) {
	return func(ctx context.Context) {
		cur := running.Add(1)
		defer running.Add(-1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				n.Add(1)
			}
		}
	}
}

// TestSweeperLeader_OneLeaderRunsTheSweepAndHandsOver: with two replicas on one
// database exactly one runs the gated sweep, every tick comes from it, and
// stopping it hands the sweep to the other within one retry interval, at a
// newer epoch.
func TestSweeperLeader_OneLeaderRunsTheSweepAndHandsOver(t *testing.T) {
	a, b := fastLeader(t, "replica-a"), fastLeader(t, "replica-b")
	var nA, nB, running, peak atomic.Int64

	ctxA, stopA := context.WithCancel(context.Background())
	ctxB, stopB := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer wg.Wait()
	defer stopB()
	defer stopA()
	for _, r := range []struct {
		l   *SweeperLeader
		ctx context.Context
		n   *atomic.Int64
	}{{a, ctxA, &nA}, {b, ctxB, &nB}} {
		wg.Add(2)
		go func() { defer wg.Done(); r.l.Run(r.ctx) }()
		go func() { defer wg.Done(); r.l.Go(r.ctx, tickSweep(r.n, &running, &peak)) }()
	}

	waitFor(t, "a leader to tick", 5*time.Second, func() bool { return nA.Load()+nB.Load() > 10 })
	time.Sleep(300 * time.Millisecond) // long enough for a second leader to show itself
	if nA.Load() > 0 && nB.Load() > 0 {
		t.Fatalf("both replicas ran the sweep (a=%d b=%d ticks)", nA.Load(), nB.Load())
	}
	if p := peak.Load(); p != 1 {
		t.Fatalf("%d sweeps ran at once, want exactly 1", p)
	}

	// The leader is whichever ticked; stop it and the other must take over.
	leader, follower, stopLeader, followerTicks := a, b, stopA, &nB
	if nA.Load() == 0 {
		leader, follower, stopLeader, followerTicks = b, a, stopB, &nA
	}
	info, err := leader.Info(context.Background())
	if err != nil || !info.Self {
		t.Fatalf("leader Info = %+v, %v; want Self", info, err)
	}
	firstEpoch := info.Epoch
	stopLeader()
	waitFor(t, "the other replica to take over within a retry", 5*time.Second, func() bool { return followerTicks.Load() > 5 })

	info, err = follower.Info(context.Background())
	if err != nil || !info.Self || info.Epoch <= firstEpoch || info.Holder != "replica-a" && info.Holder != "replica-b" {
		t.Fatalf("after handover Info = %+v, %v; want Self at an epoch above %d", info, err, firstEpoch)
	}
	if cur, err := follower.Current(context.Background(), firstEpoch); err != nil || cur {
		t.Fatalf("Current(old epoch %d) = %v, %v; want false", firstEpoch, cur, err)
	}
	if cur, err := follower.Current(context.Background(), info.Epoch); err != nil || !cur {
		t.Fatalf("Current(new epoch %d) = %v, %v; want true", info.Epoch, cur, err)
	}
}

// TestSweeperLeader_LostLockCancelsAndJoinsTheSweeps: the leader's session is
// ended behind its back, which is what a failover does. Its sweep context is
// cancelled, the leader waits for the sweep to finish before it lets go, and
// the other replica takes over at a newer epoch.
func TestSweeperLeader_LostLockCancelsAndJoinsTheSweeps(t *testing.T) {
	a, b := fastLeader(t, "replica-a"), fastLeader(t, "replica-b")
	killer := pgPool(t)
	ctx, stop := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer wg.Wait()
	defer stop()

	var started, finished atomic.Bool
	slow := func(c context.Context) {
		started.Store(true)
		<-c.Done()
		time.Sleep(150 * time.Millisecond) // a sweep that takes a moment to wind down
		finished.Store(true)
	}
	wg.Add(1)
	go func() { defer wg.Done(); a.Run(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); a.Go(ctx, slow) }()
	waitFor(t, "a to lead and start its sweep", 5*time.Second, func() bool { return started.Load() })
	info, err := a.Info(ctx)
	if err != nil || !info.Self {
		t.Fatalf("a.Info = %+v, %v; want Self", info, err)
	}
	oldEpoch := info.Epoch

	// b joins as a follower now, so it is waiting to take over.
	var bTicks, running, peak atomic.Int64
	wg.Add(1)
	go func() { defer wg.Done(); b.Run(ctx) }()
	wg.Add(1)
	go func() { defer wg.Done(); b.Go(ctx, tickSweep(&bTicks, &running, &peak)) }()
	time.Sleep(200 * time.Millisecond)
	if bTicks.Load() != 0 {
		t.Fatal("b swept while a led")
	}

	if _, err := killer.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND classid::bigint = $1 AND objid::bigint = $2 AND objsubid = 1`,
		(SweeperLeaderLockKey>>32)&0xffffffff, SweeperLeaderLockKey&0xffffffff); err != nil {
		t.Fatalf("ending the leader's session: %v", err)
	}

	waitFor(t, "a's sweep to be cancelled and joined", 5*time.Second, func() bool { return finished.Load() })
	waitFor(t, "b to take over", 5*time.Second, func() bool { return bTicks.Load() > 5 })
	if _, _, end, ok := a.Join(); ok {
		end()
		t.Fatal("a still reports a term after losing its lock")
	}
	if cur, err := b.Current(ctx, oldEpoch); err != nil || cur {
		t.Fatalf("Current(a's epoch %d) = %v, %v; want false", oldEpoch, cur, err)
	}
}
