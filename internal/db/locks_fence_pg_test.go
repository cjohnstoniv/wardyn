// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Postgres-backed tests for the fence path of a run's start locks: the fence excludes a writer and an older
// replica's start on the same key, a lost or refused session ends every hold on it, a start never answers
// ErrLockNoCapacity, one cancelled caller never drops another's hold, and a writer that ends a run queues ahead of a
// stream of starts. Two PGLockers over one pool stand in for two replicas: each has its own fence connections.
//
// Guarded by WARDYN_TEST_PG, like every other *_pg_test.go here.

import (
	"cmp"
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// fenceNonce keeps this run's start keys apart from any other run's on the same database.
var fenceNonce = time.Now().String()

// fenceKey is a start key no other test takes.
func fenceKey(t *testing.T, i int) LockKey {
	return NewLockKey(RunStartLockClass, t.Name(), strconv.Itoa(i), fenceNonce)
}

// fenceConnOf is the fence connection hctx's hold is on.
func fenceConnOf(t *testing.T, hctx context.Context) *lockConn {
	t.Helper()
	h := heldFrom(hctx)
	if h == nil || h.conn == nil || !h.conn.fence {
		t.Fatal("the hold is not on a fence connection")
	}
	return h.conn
}

// rawLock takes k in a session of pool with a blocking pg_advisory_lock, as an older replica's start does, and
// returns its release.
func rawLock(t *testing.T, pool *pgxpool.Pool, k LockKey) func() {
	t.Helper()
	ctx := context.Background()
	c, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := c.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, k.Class, k.Obj); err != nil {
		c.Release()
		t.Fatalf("raw lock: %v", err)
	}
	return func() {
		c.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, k.Class, k.Obj) //nolint:errcheck // the session ends with the test anyway
		c.Release()
	}
}

// rawTry reports whether a session of pool can take k now; it gives k back at once.
func rawTry(t *testing.T, pool *pgxpool.Pool, k LockKey) bool {
	t.Helper()
	ctx := context.Background()
	c, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer c.Release()
	var got bool
	if err := c.QueryRow(ctx, `SELECT pg_try_advisory_lock($1, $2)`, k.Class, k.Obj).Scan(&got); err != nil {
		t.Fatalf("raw try: %v", err)
	}
	if got {
		if _, err := c.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, k.Class, k.Obj); err != nil {
			t.Fatalf("raw unlock: %v", err)
		}
	}
	return got
}

// TestFenceStartExcludesTerminalWrite_PG: across two lockers on one database, a start's fence hold makes the other
// locker's start and its queued writer wait and be refused, and a writer's queued hold makes a start wait.
func TestFenceStartExcludesTerminalWrite_PG(t *testing.T) {
	pool := pgPool(t)
	a, b := NewPGLocker(pool, 4), NewPGLocker(pool, 4)
	ctx := context.Background()
	k := fenceKey(t, 0)
	_, releaseA, err := a.Lock(ctx, k, time.Second)
	if err != nil {
		t.Fatalf("a's start: %v", err)
	}
	if _, _, err := b.Lock(ctx, k, 300*time.Millisecond); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("b's start beside a's = %v; want ErrLockBusy", err)
	}
	if _, _, err := LockQueued(ctx, b, k, 300*time.Millisecond); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("b's writer beside a's start = %v; want ErrLockBusy", err)
	}
	releaseA()
	_, releaseB, err := LockQueued(ctx, b, k, time.Second)
	if err != nil {
		t.Fatalf("b's writer once a let go: %v", err)
	}
	defer releaseB()
	if _, _, err := a.Lock(ctx, k, 300*time.Millisecond); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("a's start beside b's writer = %v; want ErrLockBusy", err)
	}
}

// TestFenceLostSessionEndsEveryHold_PG: ending a fence connection's session ends every hold on it with ErrLockLost,
// leaves the holds on the other fence connection alone, and the next take dials a new connection.
func TestFenceLostSessionEndsEveryHold_PG(t *testing.T) {
	pool := pgPool(t)
	l := NewPGLocker(pool, 4)
	ctx := context.Background()
	var hctxs []context.Context
	for i := range 4 {
		hctx, release, err := l.Lock(ctx, fenceKey(t, i), time.Second)
		if err != nil {
			t.Fatalf("Lock %d: %v", i, err)
		}
		defer release()
		hctxs = append(hctxs, hctx)
	}
	lost := fenceConnOf(t, hctxs[0])
	terminate(t, pool, lost.conn.PgConn().PID())
	for _, hctx := range hctxs {
		if fenceConnOf(t, hctx) != lost {
			continue
		}
		waitFor(t, "every hold on the lost connection to end", 5*time.Second, func() bool { return hctx.Err() != nil })
		if cause := context.Cause(hctx); !errors.Is(cause, ErrLockLost) {
			t.Fatalf("a hold on the lost connection ended with %v; want ErrLockLost", cause)
		}
	}
	for _, hctx := range hctxs {
		if fenceConnOf(t, hctx) != lost && hctx.Err() != nil {
			t.Fatalf("a hold on the other connection ended: %v", context.Cause(hctx))
		}
	}
	for i := range LockFenceConns {
		hctx, release, err := l.Lock(ctx, fenceKey(t, 10+i), time.Second)
		if err != nil {
			t.Fatalf("a take after the loss: %v", err)
		}
		if fenceConnOf(t, hctx) == lost {
			t.Fatal("a take after the loss used the lost connection")
		}
		release()
	}
}

// TestFenceUnlockFalseClosesConn_PG: a release whose unlock the database answers false (the key was released out
// from under it) closes the connection, and every other hold on it ends with ErrLockLost.
func TestFenceUnlockFalseClosesConn_PG(t *testing.T) {
	l := NewPGLocker(pgPool(t), 4)
	ctx := context.Background()
	hctx, release, err := l.Lock(ctx, fenceKey(t, 0), time.Second)
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	c := fenceConnOf(t, hctx)
	var other context.Context
	for i := 1; other == nil; i++ {
		octx, orelease, err := l.Lock(ctx, fenceKey(t, i), time.Second)
		if err != nil {
			t.Fatalf("Lock %d: %v", i, err)
		}
		defer orelease()
		if fenceConnOf(t, octx) == c {
			other = octx
		}
	}
	k := HeldLocks(hctx)[0]
	c.mu.Lock()
	_, err = c.conn.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, k.Class, k.Obj)
	c.mu.Unlock()
	if err != nil {
		t.Fatalf("unlock out from under the hold: %v", err)
	}
	release()
	waitFor(t, "the connection to close", 5*time.Second, c.isClosed)
	waitFor(t, "the other hold to end", 5*time.Second, func() bool { return other.Err() != nil })
	if cause := context.Cause(other); !errors.Is(cause, ErrLockLost) {
		t.Fatalf("the other hold ended with %v; want ErrLockLost", cause)
	}
}

// TestFenceNeverNoCapacity_PG: with a locker of one slot, held by another class, 60 concurrent start holds all
// succeed.
func TestFenceNeverNoCapacity_PG(t *testing.T) {
	l := NewPGLocker(pgPool(t), 1)
	ctx := context.Background()
	_, releaseOp, err := l.Lock(ctx, NewLockKey(RunOpLockClass, t.Name()), time.Second)
	if err != nil {
		t.Fatalf("the slot's holder: %v", err)
	}
	defer releaseOp()
	var wg sync.WaitGroup
	var refused atomic.Int64
	for i := range 60 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, release, err := l.Lock(ctx, fenceKey(t, i), 5*time.Second)
			if err != nil {
				refused.Add(1)
				t.Errorf("start %d: %v", i, err)
				return
			}
			time.Sleep(200 * time.Millisecond)
			release()
		}()
	}
	wg.Wait()
	if n := refused.Load(); n != 0 {
		t.Fatalf("%d of 60 starts refused; want 0", n)
	}
}

// TestOtherClassesStillCapped_PG: at the slot cap another class, and a writer's queued start lock, are still
// refused with ErrLockNoCapacity.
func TestOtherClassesStillCapped_PG(t *testing.T) {
	l := NewPGLocker(pgPool(t), 1)
	ctx := context.Background()
	_, release, err := l.Lock(ctx, NewLockKey(RunOpLockClass, t.Name(), "a"), time.Second)
	if err != nil {
		t.Fatalf("the slot's holder: %v", err)
	}
	defer release()
	if _, _, err := l.Lock(ctx, NewLockKey(RunOpLockClass, t.Name(), "b"), time.Second); !errors.Is(err, ErrLockNoCapacity) {
		t.Fatalf("another class at the cap = %v; want ErrLockNoCapacity", err)
	}
	if _, _, err := LockQueued(ctx, l, fenceKey(t, 0), time.Second); !errors.Is(err, ErrLockNoCapacity) {
		t.Fatalf("a writer's queued start lock at the cap = %v; want ErrLockNoCapacity", err)
	}
}

// TestFenceBulkAscendingNoDeadlock_PG: two lockers each take the same start keys in ascending order on one fence
// hold, ten times over; every take ends within its wait.
func TestFenceBulkAscendingNoDeadlock_PG(t *testing.T) {
	pool := pgPool(t)
	var keys []LockKey
	for i := range 5 {
		keys = append(keys, fenceKey(t, i))
	}
	slices.SortFunc(keys, func(a, b LockKey) int { return cmp.Compare(a.Obj, b.Obj) })
	var wg sync.WaitGroup
	for _, l := range []*PGLocker{NewPGLocker(pool, 4), NewPGLocker(pool, 4)} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				ctx := context.Background()
				var releases []func()
				for _, k := range keys {
					hctx, release, err := l.Lock(ctx, k, 2*time.Second)
					if err != nil {
						t.Errorf("take %v: %v", k, err)
						break
					}
					ctx, releases = hctx, append(releases, release)
				}
				time.Sleep(20 * time.Millisecond)
				for _, release := range slices.Backward(releases) {
					release()
				}
			}
		}()
	}
	wg.Wait()
}

// TestFenceCallerCancelDoesNotDropOtherHolds_PG: 200 takes on a's fence connection, nested on a's hold, each with a
// context cancelled at a random point within 5 ms, half of them on a key a raw session holds so that they poll.
// Afterwards a's hold is live, its key is still held, and the connection's session is the same.
func TestFenceCallerCancelDoesNotDropOtherHolds_PG(t *testing.T) {
	pool := pgPool(t)
	l := NewPGLocker(pool, 4)
	ctx := context.Background()
	base := LockKey{Class: RunStartLockClass, Obj: -2_000_000_000}
	actx, release, err := l.Lock(ctx, base, time.Second)
	if err != nil {
		t.Fatalf("a's hold: %v", err)
	}
	defer release()
	c := fenceConnOf(t, actx)
	pid := c.conn.PgConn().PID()
	busy := LockKey{Class: RunStartLockClass, Obj: 2_000_000_000}
	defer rawLock(t, pool, busy)()
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k := busy
			if i%2 == 0 {
				k = LockKey{Class: RunStartLockClass, Obj: base.Obj + 1 + int32(i)}
			}
			cctx, cancel := context.WithCancel(actx)
			defer cancel()
			time.AfterFunc(time.Duration(rand.Int64N(int64(5*time.Millisecond))), cancel)
			if _, r, err := l.Lock(cctx, k, time.Second); err == nil {
				r()
			}
		}()
	}
	wg.Wait()
	if actx.Err() != nil {
		t.Fatalf("a's hold ended: %v", context.Cause(actx))
	}
	if err := LockHeld(actx); err != nil {
		t.Fatalf("a's key is no longer held: %v", err)
	}
	if got := c.conn.PgConn().PID(); got != pid || c.isClosed() {
		t.Fatalf("the fence connection's session changed (pid %d -> %d, closed %v)", pid, got, c.isClosed())
	}
}

// TestFenceTakeBusyAgainstRawSessionLock_PG: an older replica's start, a blocking pg_advisory_lock on the same pair
// in another session, makes a fence take wait and be refused; and while the fence holds the key another session's
// try fails.
func TestFenceTakeBusyAgainstRawSessionLock_PG(t *testing.T) {
	pool := pgPool(t)
	l := NewPGLocker(pool, 4)
	ctx := context.Background()
	k := fenceKey(t, 0)
	unlock := rawLock(t, pool, k)
	if _, _, err := l.Lock(ctx, k, 300*time.Millisecond); !errors.Is(err, ErrLockBusy) {
		unlock()
		t.Fatalf("a fence take beside a raw session's lock = %v; want ErrLockBusy", err)
	}
	unlock()
	_, release, err := l.Lock(ctx, k, time.Second)
	if err != nil {
		t.Fatalf("a fence take once the raw session let go: %v", err)
	}
	defer release()
	if rawTry(t, pool, k) {
		t.Fatal("another session took the key while the fence held it")
	}
}

// TestFenceWriterNotStarvedByStartStream_PG: locker b takes k on the fence back to back for 5 s, each hold 50 ms.
// A second in, locker a's queued writer gets k within its 2 s wait, and while a holds k, b's next take waits.
func TestFenceWriterNotStarvedByStartStream_PG(t *testing.T) {
	pool := pgPool(t)
	a, b := NewPGLocker(pool, 4), NewPGLocker(pool, 4)
	ctx := context.Background()
	k := fenceKey(t, 0)
	var bHolds atomic.Bool
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for end := time.Now().Add(5 * time.Second); time.Now().Before(end); {
			_, release, err := b.Lock(ctx, k, 5*time.Second)
			if err != nil {
				continue
			}
			bHolds.Store(true)
			time.Sleep(50 * time.Millisecond)
			bHolds.Store(false)
			release()
		}
	}()
	defer wg.Wait()
	time.Sleep(time.Second)
	start := time.Now()
	_, release, err := LockQueued(ctx, a, k, 2*time.Second)
	if err != nil {
		t.Fatalf("a's writer behind a stream of starts: %v", err)
	}
	t.Logf("a's writer waited %s", time.Since(start))
	for end := time.Now().Add(300 * time.Millisecond); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if bHolds.Load() {
			release()
			t.Fatal("b held k while a's writer held it")
		}
	}
	release()
}

// TestFenceHoldBlocksNestedWriter_PG: a writer nested under its run's operation lock queues for k behind a start's
// fence hold; while it waits a third locker's start cannot take k; and when the start lets go the writer gets k.
func TestFenceHoldBlocksNestedWriter_PG(t *testing.T) {
	pool := pgPool(t)
	a, b, c := NewPGLocker(pool, 4), NewPGLocker(pool, 4), NewPGLocker(pool, 4)
	ctx := context.Background()
	k := fenceKey(t, 0)
	_, releaseA, err := a.Lock(ctx, k, time.Second)
	if err != nil {
		t.Fatalf("a's start: %v", err)
	}
	opctx, releaseOp, err := b.Lock(ctx, NewLockKey(RunOpLockClass, t.Name()), time.Second)
	if err != nil {
		t.Fatalf("b's run lock: %v", err)
	}
	defer releaseOp()
	got := make(chan error, 1)
	go func() {
		_, release, err := b.Lock(opctx, k, 2*time.Second)
		if err == nil {
			release()
		}
		got <- err
	}()
	time.Sleep(300 * time.Millisecond) // b is queued
	if _, _, ok, err := c.TryLock(ctx, k); ok || err != nil {
		releaseA()
		t.Fatalf("c's start while b's writer waits = %v, %v; want false, nil", ok, err)
	}
	releaseA()
	if err := <-got; err != nil {
		t.Fatalf("b's nested writer once a let go: %v", err)
	}
}
