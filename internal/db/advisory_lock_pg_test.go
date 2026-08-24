// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Postgres-backed tests for TryAdvisoryLock — the skip-if-held semantics the
// lifecycle reaper's tick gate rests on. This is NEW to the repo (Migrate's lock
// is the BLOCKING pg_advisory_lock), and it is only meaningful against a real
// server: nothing in Go models a session-level lock.
//
// The two-control-plane case is two acquisitions on two SEPARATE connections
// (advisory locks are re-entrant WITHIN a session, so a single-connection test
// would prove the opposite of what it claims).
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"testing"
)

// TestTryAdvisoryLock_SkipsWhenHeld: process A holds the reap lock, so process
// B's try returns false instead of queueing — the reaper skips that tick. Once A
// releases, B can take it.
func TestTryAdvisoryLock_SkipsWhenHeld(t *testing.T) {
	// Two pools = two unrelated sessions, i.e. two control planes.
	poolA, poolB := pgPool(t), pgPool(t)
	ctx := context.Background()

	releaseA, ok, err := TryAdvisoryLock(ctx, poolA, ReaperAdvisoryLockKey)
	if err != nil {
		t.Fatalf("A try-lock: %v", err)
	}
	if !ok {
		t.Fatal("A did not get an uncontended lock")
	}

	releaseB, ok, err := TryAdvisoryLock(ctx, poolB, ReaperAdvisoryLockKey)
	if err != nil {
		t.Fatalf("B try-lock: %v", err)
	}
	if ok {
		releaseB()
		releaseA()
		t.Fatal("B took a lock A already holds — the reap tick would run twice")
	}

	// A releases: the lock is now free, so B's next tick wins it.
	releaseA()
	releaseB, ok, err = TryAdvisoryLock(ctx, poolB, ReaperAdvisoryLockKey)
	if err != nil {
		t.Fatalf("B try-lock after release: %v", err)
	}
	if !ok {
		t.Fatal("lock was not released; the reaper would be dead until restart")
	}
	releaseB()
}

// TestAdvisoryLockKeysAreDistinct: no two lock keys may collide. Each pair is a
// different silent failure — a reap key equal to the migration lock would let a
// boot-time Migrate disable reaping and a long reap tick stall a concurrent
// boot; a rekey key equal to any of them would make `wardynd -rotate-age-key`
// refuse to run whenever an unrelated subsystem held its lock, or (worse) let
// the two proceed believing they had exclusivity. Needs no server — it is the
// one property of these constants that can go wrong.
func TestAdvisoryLockKeysAreDistinct(t *testing.T) {
	keys := map[string]int64{
		"migrate":            migrateAdvisoryLockKey,
		"reaper":             ReaperAdvisoryLockKey,
		"groundtruthRotator": GroundTruthRotatorLockKey,
		"secretRekey":        SecretRekeyLockKey,
	}
	seen := map[int64]string{}
	for name, key := range keys {
		if prev, dup := seen[key]; dup {
			t.Errorf("advisory lock keys %q and %q are both %#x", prev, name, key)
			continue
		}
		seen[key] = name
	}
}
