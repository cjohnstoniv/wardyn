// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Postgres-backed tests for TryAdvisoryLockDedicated: a lock held for the
// process lifetime takes nothing from the request pool, so a pool of ONE keeps
// answering under it, and the sweeper leader still elects and hands over there.
//
// Guarded by WARDYN_TEST_PG, like every other *_pg_test.go here.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// dedicatedTestLockKey is a key nothing else takes, so these tests cannot meet
// another package's holder of a real one.
const dedicatedTestLockKey int64 = 0x5741524459_544544 // ASCII "WARDYTED"

// poolOfOne is a pool on the test database with pool_max_conns=1: any lock
// that borrowed from it would leave no connection at all.
func poolOfOne(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg := pgPool(t).Config().Copy()
	cfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// answers fails the test unless pool serves a query within two seconds; on an
// exhausted pool pgxpool.Acquire would block until the deadline instead.
func answers(t *testing.T, pool *pgxpool.Pool, when string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var one int
	if err := pool.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("the pool did not answer %s: %v", when, err)
	}
}

func TestTryAdvisoryLockDedicated_HoldsNoPoolConnection(t *testing.T) {
	poolA, poolB := poolOfOne(t), poolOfOne(t)
	ctx := context.Background()

	conn, releaseA, ok, err := TryAdvisoryLockDedicated(ctx, poolA, dedicatedTestLockKey)
	if err != nil || !ok {
		t.Fatalf("first taker = %v, %v; want the lock", ok, err)
	}
	releaseA = sync.OnceFunc(releaseA)
	defer releaseA()

	if n := poolA.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("the lock holds %d pool connection(s), want 0", n)
	}
	answers(t, poolA, "while its process holds a dedicated lock")
	// The returned connection is the session that holds the lock.
	var held bool
	if err := conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND pid = pg_backend_pid())`).Scan(&held); err != nil || !held {
		t.Fatalf("the returned connection holds the lock = %v, %v; want true", held, err)
	}

	// A second process is refused, and its failed try leaves nothing behind.
	if _, _, ok, err := TryAdvisoryLockDedicated(ctx, poolB, dedicatedTestLockKey); err != nil || ok {
		t.Fatalf("second taker while the first holds = %v, %v; want refused", ok, err)
	}
	if n := poolB.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("a refused try left %d pool connection(s) acquired", n)
	}

	// Release unlocks before it closes: the lock is free the moment it returns,
	// and it returns inside its bound.
	start := time.Now()
	releaseA()
	if took := time.Since(start); took > dedicatedLockReleaseWait {
		t.Fatalf("release took %s, over its %s bound", took, dedicatedLockReleaseWait)
	}
	_, releaseB, ok, err := TryAdvisoryLockDedicated(ctx, poolB, dedicatedTestLockKey)
	if err != nil || !ok {
		t.Fatalf("taker straight after release = %v, %v; want the lock", ok, err)
	}
	releaseB()
}

// TestTryAdvisoryLockDedicated_ReleaseIsBoundedOnADeadSession: a session that
// is already gone must not hold shutdown past the bound.
func TestTryAdvisoryLockDedicated_ReleaseIsBoundedOnADeadSession(t *testing.T) {
	pool, other := poolOfOne(t), poolOfOne(t)
	ctx := context.Background()
	conn, release, ok, err := TryAdvisoryLockDedicated(ctx, pool, dedicatedTestLockKey)
	if err != nil || !ok {
		t.Fatalf("taker = %v, %v; want the lock", ok, err)
	}
	if _, err := other.Exec(ctx, `SELECT pg_terminate_backend($1)`, conn.PgConn().PID()); err != nil {
		t.Fatalf("ending the holder's session: %v", err)
	}
	start := time.Now()
	release()
	if took := time.Since(start); took > dedicatedLockReleaseWait+time.Second {
		t.Fatalf("release on a dead session took %s, over its %s bound", took, dedicatedLockReleaseWait)
	}
	waitFor(t, "the dead session's lock to be free", 5*time.Second, func() bool {
		_, r, ok, err := TryAdvisoryLockDedicated(ctx, other, dedicatedTestLockKey)
		if err != nil || !ok {
			return false
		}
		r()
		return true
	})
}

// TestSweeperLeader_ElectsAndHandsOverOnAPoolOfOne: two replicas, each on a
// pool of one. One leads at a durable epoch with no pool connection held, the
// pool keeps answering, and stopping it hands the lease on at a newer epoch.
func TestSweeperLeader_ElectsAndHandsOverOnAPoolOfOne(t *testing.T) {
	newLeader := func(name string) *SweeperLeader {
		l := NewSweeperLeader(poolOfOne(t), name)
		l.retry, l.monitor, l.poll = 100*time.Millisecond, 50*time.Millisecond, 10*time.Millisecond
		return l
	}
	a, b := newLeader("replica-a"), newLeader("replica-b")
	leads := func(l *SweeperLeader) bool {
		_, _, end, ok := l.Join()
		if ok {
			end()
		}
		return ok
	}

	ctxA, stopA := context.WithCancel(context.Background())
	ctxB, stopB := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	defer wg.Wait()
	defer stopB()
	defer stopA()
	wg.Add(2)
	go func() { defer wg.Done(); a.Run(ctxA) }()
	go func() { defer wg.Done(); b.Run(ctxB) }()

	waitFor(t, "a leader", 5*time.Second, func() bool { return leads(a) || leads(b) })
	time.Sleep(300 * time.Millisecond) // long enough for a second leader to show itself
	if leads(a) && leads(b) {
		t.Fatal("both replicas lead")
	}
	leader, follower, stopLeader := a, b, stopA
	if !leads(a) {
		leader, follower, stopLeader = b, a, stopB
	}
	info, err := leader.Info(context.Background())
	if err != nil || !info.Self || info.Epoch == 0 {
		t.Fatalf("leader Info = %+v, %v; want Self at a durable epoch", info, err)
	}
	if n := leader.pool.Stat().AcquiredConns(); n != 0 {
		t.Fatalf("the leader holds %d pool connection(s), want 0", n)
	}
	answers(t, leader.pool, "while its process leads")
	if cur, err := leader.Current(context.Background(), info.Epoch); err != nil || !cur {
		t.Fatalf("Current(epoch %d) = %v, %v; want true", info.Epoch, cur, err)
	}

	stopLeader()
	waitFor(t, "the other replica to take over", 5*time.Second, func() bool { return leads(follower) })
	next, err := follower.Info(context.Background())
	if err != nil || !next.Self || next.Epoch <= info.Epoch {
		t.Fatalf("after handover Info = %+v, %v; want Self at an epoch above %d", next, err, info.Epoch)
	}
	if cur, err := follower.Current(context.Background(), info.Epoch); err != nil || cur {
		t.Fatalf("Current(old epoch %d) = %v, %v; want false", info.Epoch, cur, err)
	}
}
