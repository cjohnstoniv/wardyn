// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Postgres-backed test proving the ground-truth rotator's leader-election lock
// (db.GroundTruthRotatorLockKey) gives mutual exclusion, and that a standby
// takes over automatically when the leader's session dies — the whole point
// of the acquire-once shape (S2). Unlike the reaper's per-tick TickLock (see
// internal/db/advisory_lock_pg_test.go), nothing here calls release() on the
// winning side; takeover is instead proven by ending the leader's SESSION.
//
// That requires Hijack()ing the leader's connection out of the pool and
// closing the raw *pgx.Conn directly, NOT pool.Close(): pgxpool.Pool.Close()
// only closes connections currently sitting IDLE in the pool (see
// puddle.Pool.Close in jackc/puddle) — a connection still marked ACQUIRED,
// like the one holding this lock, is left untouched and simply leaked. That
// leak was caught live while writing this test: a naive poolA.Close() left
// the session (and its lock) alive indefinitely, wedging the second
// TryAdvisoryLock forever with no error. Hijack + a direct raw Close is what
// actually terminates the TCP connection and, with it, the server-side
// session — the same thing a process crash does.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
// Run: WARDYN_TEST_PG="postgres://wardyn:wardyn@localhost:55432/wardyn?sslmode=disable" \
//        go test ./cmd/wardynd/... -run TestGroundtruthRotatorLock

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// pgPool connects to WARDYN_TEST_PG and migrates it, mirroring the pgPool
// convention shared by every other *_pg_test.go in this repo (e.g.
// internal/db/migrate_pg_test.go, internal/broker/concurrency_pg_test.go).
// Skips (the only sanctioned skip) when the env var is absent.
func pgPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping Postgres-backed groundtruth rotator lock test")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestGroundtruthRotatorLock_MutualExclusionAndTakeover: two independent pools
// stand in for two wardynd replicas racing to lead the rotator. Exactly one
// acquires; while it holds the lock, the other must not; ending the leader's
// session (not a graceful release()) frees the lock for the standby.
//
// Every step runs under a bounded context: a regression that reintroduces the
// leaked-connection bug this test caught (see package doc) must fail loudly
// on a timeout instead of wedging `go test` indefinitely.
func TestGroundtruthRotatorLock_MutualExclusionAndTakeover(t *testing.T) {
	poolB := pgPool(t) // migrates the DB once; also plays the eventual standby-turned-leader

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	poolA, err := db.Connect(ctx, os.Getenv("WARDYN_TEST_PG"))
	if err != nil {
		t.Fatalf("connect poolA: %v", err)
	}
	defer poolA.Close() // harmless no-op on the lock connection: it is Hijack()ed out below

	// Acquire A's connection directly (rather than via db.TryAdvisoryLock) so
	// the test can identify and force-close THIS SPECIFIC session later.
	connA, err := poolA.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connA: %v", err)
	}
	var gotA bool
	if err := connA.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, db.GroundTruthRotatorLockKey).Scan(&gotA); err != nil {
		t.Fatalf("A try-lock: %v", err)
	}
	if !gotA {
		t.Fatal("A did not get an uncontended lock")
	}

	if _, ok, err := db.TryAdvisoryLock(ctx, poolB, db.GroundTruthRotatorLockKey); err != nil {
		t.Fatalf("B try-lock while A holds: %v", err)
	} else if ok {
		t.Fatal("B acquired a lock A already holds — two replicas would both rotate the ground-truth token")
	}

	// Simulate the leader crashing: hijack A's connection out of the pool and
	// close the RAW connection directly (see package doc for why plain
	// poolA.Close() does not work).
	rawA := connA.Hijack()
	if err := rawA.Close(ctx); err != nil {
		t.Fatalf("close hijacked connA: %v", err)
	}

	releaseB, ok, err := db.TryAdvisoryLock(ctx, poolB, db.GroundTruthRotatorLockKey)
	if err != nil {
		t.Fatalf("B try-lock after A's session died: %v", err)
	}
	if !ok {
		t.Fatal("standby never took over after the leader's session ended; the rotator would stay dead until manual intervention")
	}
	releaseB()
}
