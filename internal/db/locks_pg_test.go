// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// The PG-backed Locker against a real server: nothing in Go models a
// session-level lock, a lost connection or a full pool. Two pools are two
// unrelated sessions, i.e. two replicas.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func twoReplicas(t *testing.T) (*PGLocker, *PGLocker, *pgxpool.Pool) {
	t.Helper()
	a, b := pgPool(t), pgPool(t)
	return NewPGLocker(a, 4), NewPGLocker(b, 4), b
}

func TestPGLocker_SerializesAcrossReplicas(t *testing.T) {
	a, b, _ := twoReplicas(t)
	requireSerializes(t, a, b)
}

func TestPGLocker_EveryDocumentedNestingIsOrdered(t *testing.T) {
	a, _, _ := twoReplicas(t)
	requireOrderEnforced(t, a)
}

func TestPGLocker_ReleaseEndsTheContext(t *testing.T) {
	a, _, _ := twoReplicas(t)
	requireReleaseEndsTheContext(t, a)
}

func TestPGLocker_NestingSemantics(t *testing.T) {
	a, _, _ := twoReplicas(t)
	requireNestingSemantics(t, a)
}

// A nested lock reuses its caller's connection: with ONE slot, a hold that
// nests three more locks still fits, and a second outer hold does not.
func TestPGLocker_NestedLocksShareTheCallersConnection(t *testing.T) {
	pool := pgPool(t)
	l := NewPGLocker(pool, 1)
	ctx := context.Background()
	old := LockPoolAcquireWait
	LockPoolAcquireWait = 100 * time.Millisecond
	t.Cleanup(func() { LockPoolAcquireWait = old })

	octx, release, err := l.Lock(ctx, testKey(t, RunOpLockClass), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cur := octx
	for _, class := range []int32{AWSSSOLockClass, ADOSignInLockClass, SiteConfigLockClass} {
		nctx, nrelease, err := l.Lock(cur, testKey(t, class), time.Second)
		if err != nil {
			t.Fatalf("nested lock %#x on a one-connection pool: %v", uint32(class), err)
		}
		defer nrelease()
		cur = nctx
	}
	if _, _, err := l.Lock(ctx, testKey(t, CapEnforcementLockClass), time.Second); !errors.Is(err, ErrLockNoCapacity) {
		t.Errorf("a second outer hold on a full pool = %v, want ErrLockNoCapacity", err)
	}
}

// Pool exhaustion refuses rather than proceeding unlocked: the refused caller
// holds nothing, and the key it asked for is not taken.
func TestPGLocker_ExhaustedPoolRefusesAndTakesNothing(t *testing.T) {
	pool := pgPool(t)
	l := NewPGLocker(pool, 1)
	old := LockPoolAcquireWait
	LockPoolAcquireWait = 100 * time.Millisecond
	t.Cleanup(func() { LockPoolAcquireWait = old })
	ctx := context.Background()

	_, release, err := l.Lock(ctx, testKey(t, RunOpLockClass), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	want := NewLockKey(AWSSSOLockClass, t.Name(), "wanted")
	if _, rel, err := l.Lock(ctx, want, time.Second); !errors.Is(err, ErrLockNoCapacity) {
		if err == nil {
			rel()
		}
		t.Fatalf("Lock on an exhausted pool = %v, want ErrLockNoCapacity", err)
	}
	if _, rel, ok, err := l.TryLock(ctx, want); !errors.Is(err, ErrLockNoCapacity) || ok {
		if ok {
			rel()
		}
		t.Fatalf("TryLock on an exhausted pool = ok %v, err %v, want ErrLockNoCapacity", ok, err)
	}
	release()
	// The slot is free again, and the key was never held.
	other := NewPGLocker(pool, 1)
	_, rel, ok, err := other.TryLock(ctx, want)
	if err != nil || !ok {
		t.Fatalf("the refused key was left held: ok %v, err %v", ok, err)
	}
	rel()
}

// A lock lost with its connection (a failover, an operator's
// pg_terminate_backend) cancels the context the guarded work holds, with
// ErrLockLost as the cause.
func TestPGLocker_LostConnectionCancelsTheGuardedContext(t *testing.T) {
	pool := pgPool(t)
	l := NewPGLocker(pool, 2)
	old := LockWatchInterval
	LockWatchInterval = 20 * time.Millisecond
	t.Cleanup(func() { LockWatchInterval = old })
	ctx := context.Background()
	k := testKey(t, AWSSSOLockClass)

	hctx, release, err := l.Lock(ctx, k, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// A nested lock is verified too.
	nctx, nrelease, err := l.Lock(hctx, testKey(t, SiteConfigLockClass), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer nrelease()

	var killed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM (
		SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND objsubid = 2 AND classid::bigint = $1 AND objid::bigint = $2) t`,
		int64(uint32(k.Class)), int64(uint32(k.Obj))).Scan(&killed); err != nil {
		t.Fatal(err)
	}
	if killed != 1 {
		t.Fatalf("terminated %d backends holding the lock, want 1", killed)
	}
	for _, c := range []context.Context{hctx, nctx} {
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the guarded context was not cancelled after the lock's connection was lost")
		}
		if cause := context.Cause(c); !errors.Is(cause, ErrLockLost) {
			t.Errorf("cause = %v, want ErrLockLost", cause)
		}
	}
	// The lock really is gone: another replica takes it.
	_, rel, ok, err := NewPGLocker(pool, 1).TryLock(ctx, k)
	if err != nil || !ok {
		t.Fatalf("a lost lock could not be taken by another session: ok %v, err %v", ok, err)
	}
	rel()
}

// Releasing frees the lock at once, and a hold that is never lost never has its
// context cancelled by the watcher.
func TestPGLocker_ReleaseFreesTheLockAtOnce(t *testing.T) {
	a, b, _ := twoReplicas(t)
	ctx := context.Background()
	k := testKey(t, SiteConfigLockClass)
	hctx, release, err := a.Lock(ctx, k, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * LockWatchInterval / 2)
	if hctx.Err() != nil {
		t.Fatalf("a healthy hold's context ended: %v", context.Cause(hctx))
	}
	release()
	_, rel, ok, err := b.TryLock(ctx, k)
	if err != nil || !ok {
		t.Fatalf("the lock was not free the moment release returned: ok %v, err %v", ok, err)
	}
	rel()
}
