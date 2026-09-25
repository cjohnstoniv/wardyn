// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// AdvisoryLockKeyed's fail-open arms. Every caller proceeds UNLOCKED on an
// error, so what these pin is that giving up is bounded and leaves nothing
// behind: a refused call must return inside its budget, hand back any pool
// connection it borrowed and free the one in-process slot, or the NEXT sign-in
// waits out its whole budget on a slot nobody holds.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// keyedLockTestClass keeps these tests' keys off LoginSupersedeLockClass, which
// other packages' tests take against the same database at the same time.
const keyedLockTestClass int32 = 0x54535431 // "TST1"

// keyedLockSlack is the ε in "errors within wait+ε": generous for a loaded CI
// box, and far below the unbounded stall the budget exists to prevent.
const keyedLockSlack = 2 * time.Second

func tryKeyed(t *testing.T, pool *pgxpool.Pool, obj int32) bool {
	t.Helper()
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire probe conn: %v", err)
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(context.Background(), `SELECT pg_try_advisory_lock($1, $2)`, keyedLockTestClass, obj).Scan(&got); err != nil {
		t.Fatalf("probe try-lock: %v", err)
	}
	if got {
		conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1, $2)`, keyedLockTestClass, obj) //nolint:errcheck // probe cleanup
	}
	return got
}

// keyedLockPool is a pool whose cleanup gives up on Close after keyedLockSlack:
// pgxpool.Close waits for every acquired connection, so a leak the test has
// already reported would otherwise hang the package until its timeout.
func keyedLockPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pgPool(t) // skips when WARDYN_TEST_PG is unset; migrates
	pool, err := Connect(context.Background(), os.Getenv("WARDYN_TEST_PG"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() { pool.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(keyedLockSlack):
			t.Error("pool.Close did not return: a connection is still acquired")
		}
	})
	return pool
}

// acquiredSettles waits up to keyedLockSlack for the pool's acquired count to
// reach want, and returns the last count seen.
func acquiredSettles(pool *pgxpool.Pool, want int32) int32 {
	deadline := time.Now().Add(keyedLockSlack)
	for {
		got := pool.Stat().AcquiredConns()
		if got == want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAdvisoryLockKeyed_HeldElsewhereFailsOpenWithinBudgetAndLeaksNothing(t *testing.T) {
	pool := keyedLockPool(t)
	ctx := context.Background()
	const obj = 101

	holder, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire holder conn: %v", err)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_lock($1, $2)`, keyedLockTestClass, obj); err != nil {
		holder.Release()
		t.Fatalf("holder lock: %v", err)
	}
	baseline := pool.Stat().AcquiredConns()

	const wait = 300 * time.Millisecond
	start := time.Now()
	release, err := AdvisoryLockKeyed(ctx, pool, keyedLockTestClass, obj, wait)
	took := time.Since(start)
	if err == nil {
		release()
		holder.Release()
		t.Fatal("took a lock another session holds")
	}
	if !strings.Contains(err.Error(), "advisory lock (") {
		t.Errorf("err = %v; want the lock-wait arm, not the slot or capacity arm", err)
	}
	if took > wait+keyedLockSlack {
		t.Errorf("gave up after %v on a %v budget: the lock wait is not bounded", took, wait)
	}
	// Polled: a connection refused mid-transaction is destroyed rather than
	// pooled (it may hold the lock if the grant raced the cancel), and pgxpool
	// destroys asynchronously.
	if got := acquiredSettles(pool, baseline); got != baseline {
		t.Errorf("AcquiredConns = %d after the refusal, want the baseline %d: the refused call kept its connection", got, baseline)
	}

	holder.Exec(ctx, `SELECT pg_advisory_unlock($1, $2)`, keyedLockTestClass, obj) //nolint:errcheck // released with the conn either way
	holder.Release()

	// The slot was freed: the next call takes the lock at once instead of
	// waiting out its budget behind a slot nobody holds.
	release, err = AdvisoryLockKeyed(ctx, pool, keyedLockTestClass, obj, 5*time.Second)
	if err != nil {
		t.Fatalf("the call after a refusal failed: %v", err)
	}
	if tryKeyed(t, pool, obj) {
		release()
		t.Fatal("a successful AdvisoryLockKeyed does not hold the lock")
	}
	release()
	if !tryKeyed(t, pool, obj) {
		t.Fatal("release did not unlock")
	}
}

func TestAdvisoryLockKeyed_BusySlotFailsOpenWithinBudget(t *testing.T) {
	pool := keyedLockPool(t)
	ctx := context.Background()

	first, err := AdvisoryLockKeyed(ctx, pool, keyedLockTestClass, 201, 5*time.Second)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	baseline := pool.Stat().AcquiredConns()

	// A different key: nothing contends in the database, only the in-process
	// one-hold-at-a-time slot.
	const wait = 200 * time.Millisecond
	start := time.Now()
	second, err := AdvisoryLockKeyed(ctx, pool, keyedLockTestClass, 202, wait)
	took := time.Since(start)
	if err == nil {
		second()
		first()
		t.Fatal("a second hold was taken while the process slot was busy")
	}
	if !strings.Contains(err.Error(), "in-process advisory lock slot") {
		t.Errorf("err = %v; want the slot arm", err)
	}
	if took > wait+keyedLockSlack {
		t.Errorf("gave up after %v on a %v budget", took, wait)
	}
	if got := pool.Stat().AcquiredConns(); got != baseline {
		t.Errorf("AcquiredConns = %d, want %d: waiting on the slot must pin no connection", got, baseline)
	}
	first()

	second, err = AdvisoryLockKeyed(ctx, pool, keyedLockTestClass, 202, 5*time.Second)
	if err != nil {
		t.Fatalf("the call after the slot freed failed: %v", err)
	}
	second()
}

func TestAdvisoryLockKeyed_PoolWithoutSpareConnsFailsOpenAndFreesTheSlot(t *testing.T) {
	pool := keyedLockPool(t)
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(os.Getenv("WARDYN_TEST_PG"))
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = advisoryLockFreeConnsNeeded - 1
	small, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("small pool: %v", err)
	}
	t.Cleanup(small.Close)

	if release, err := AdvisoryLockKeyed(ctx, small, keyedLockTestClass, 301, 5*time.Second); err == nil {
		release()
		t.Fatal("a pool that cannot spare a connection for the guarded work took a hold anyway: the lock would self-deadlock")
	} else if !strings.Contains(err.Error(), "pool cannot spare") {
		t.Errorf("err = %v; want the capacity arm", err)
	}
	if got := small.Stat().AcquiredConns(); got != 0 {
		t.Errorf("AcquiredConns = %d, want 0", got)
	}

	release, err := AdvisoryLockKeyed(ctx, pool, keyedLockTestClass, 301, 5*time.Second)
	if err != nil {
		t.Fatalf("the call after a capacity refusal failed: %v — the refusal kept the process slot", err)
	}
	release()
}
