// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Postgres-backed tests for AdvisoryLockHeld: a follower retrying a lifetime
// lock must not dial a session of its own every tick to be told the leader has
// it.
//
// Guarded by WARDYN_TEST_PG, like every other *_pg_test.go here.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAdvisoryLockHeld_BigintKeyAcrossSessions(t *testing.T) {
	holder, observer := poolOfOne(t), poolOfOne(t)
	ctx := context.Background()
	for _, key := range []int64{dedicatedTestLockKey, -1, -4294967295} {
		t.Run(fmt.Sprintf("%d", key), func(t *testing.T) {
			_, release, ok, err := TryAdvisoryLockDedicated(ctx, holder, key)
			if err != nil || !ok {
				t.Fatalf("hold bigint key: %v, %v", ok, err)
			}
			defer release()
			if held, err := AdvisoryLockHeld(ctx, observer, key); err != nil || !held {
				t.Fatalf("cross-session probe = %v, %v; want held", held, err)
			}
			if held, err := AdvisoryLockHeld(ctx, observer, key^1); err != nil || held {
				t.Fatalf("neighbor key probe = %v, %v; want free", held, err)
			}
		})
	}
}

func TestAdvisoryLockHeld_ObservesOwnSessionWithoutTakingLock(t *testing.T) {
	pool := poolOfOne(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `SELECT pg_advisory_lock($1)`, dedicatedTestLockKey); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, `SELECT pg_advisory_unlock_all()`) })
	if held, err := AdvisoryLockHeld(ctx, pool, dedicatedTestLockKey); err != nil || !held {
		t.Fatalf("own-session probe = %v, %v; want held", held, err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_advisory_unlock($1)`, dedicatedTestLockKey); err != nil {
		t.Fatal(err)
	}
	if held, err := AdvisoryLockHeld(ctx, pool, dedicatedTestLockKey); err != nil || held {
		t.Fatalf("probe after one unlock = %v, %v; want free", held, err)
	}
}

// serverSessions is the cumulative count of sessions this database has
// established, so a follower's connection CHURN is measurable. The PG suite runs
// -p 1 against one shared database (make test-report-pg), so nothing else is
// dialling while this runs.
func serverSessions(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int64
	if err := pool.QueryRow(ctx, `SELECT sessions FROM pg_stat_database WHERE datname = current_database()`).Scan(&n); err != nil {
		t.Fatalf("read pg_stat_database.sessions: %v", err)
	}
	return n
}

// TestSweeperLeader_FollowerDoesNotDialASessionEveryTick: replica A leads;
// replica B retries on a short backoff. Every one of B's attempts must report
// standby, and NONE of them may open a database session of its own — the probe
// answers from B's pool, which keeps one connection and reuses it.
//
// Counting server SESSIONS rather than the live backends is the point: the
// dedicated connection a follower used to dial is closed again immediately, so
// it is invisible to pg_stat_activity by the time anyone could look, while the
// cumulative counter still carries it.
func TestSweeperLeader_FollowerDoesNotDialASessionEveryTick(t *testing.T) {
	const attempts = 6

	leaderPool, followerPool := poolOfOne(t), poolOfOne(t)
	ctx := context.Background()

	// A leads for the duration: the dedicated session its term holds.
	_, releaseA, ok, err := TryAdvisoryLockDedicated(ctx, leaderPool, SweeperLeaderLockKey)
	if err != nil || !ok {
		t.Fatalf("the leader took the lease = %v, %v", ok, err)
	}
	defer releaseA()

	follower := NewSweeperLeader(followerPool, "replica-b")
	follower.retry, follower.monitor, follower.poll = 10*time.Millisecond, 10*time.Millisecond, time.Millisecond

	// Warm the follower's pool so its one probe connection is already counted.
	if held, err := AdvisoryLockHeld(ctx, followerPool, SweeperLeaderLockKey); err != nil || !held {
		t.Fatalf("the follower's probe = held %v, err %v; want held by the leader", held, err)
	}
	before := serverSessions(t, leaderPool)

	for i := range attempts {
		if got := follower.lead(ctx); got != "another replica holds the lock" {
			t.Fatalf("attempt %d = %q, want a standby that never dialled", i+1, got)
		}
	}

	dialed := serverSessions(t, leaderPool) - before
	// One pooled connection serves every probe; the dedicated path would have
	// dialled and closed one per attempt.
	if dialed > 1 {
		t.Fatalf("%d attempts opened %d database sessions, want at most 1: a follower that loses the lease must not dial a session per tick", attempts, dialed)
	}
	if dialed < 0 {
		t.Fatalf("session count went backwards by %d", -dialed)
	}
}

// The probe is not a fence and must not be mistaken for one: it reports the
// lock free when nobody holds it, and the winner still wins the dedicated try.
func TestAdvisoryLockHeld_FreeWhenNobodyHoldsIt(t *testing.T) {
	pool := poolOfOne(t)
	ctx := context.Background()

	held, err := AdvisoryLockHeld(ctx, pool, dedicatedTestLockKey)
	if err != nil || held {
		t.Fatalf("probe on an unheld key = held %v, err %v; want free", held, err)
	}
	// The probe leaves nothing behind: the key is still takeable.
	conn, release, ok, err := TryAdvisoryLockDedicated(ctx, pool, dedicatedTestLockKey)
	if err != nil || !ok {
		t.Fatalf("take after the probe = %v, %v; the probe stranded the lock", ok, err)
	}
	release()
	_ = conn
}

// While one session holds the key, another sees it held; and a probe error is
// reported rather than read as "held", so a caller that falls through to its
// dedicated try still gets the real fault.
func TestAdvisoryLockHeld_ReportsHeldAndSurfacesErrors(t *testing.T) {
	holder, other := poolOfOne(t), poolOfOne(t)
	ctx := context.Background()

	_, release, ok, err := TryAdvisoryLockDedicated(ctx, holder, dedicatedTestLockKey)
	if err != nil || !ok {
		t.Fatalf("holder took the key = %v, %v", ok, err)
	}
	held, err := AdvisoryLockHeld(ctx, other, dedicatedTestLockKey)
	if err != nil || !held {
		t.Fatalf("probe against a held key = held %v, err %v; want held", held, err)
	}
	release()

	// A pool that cannot reach the database errors rather than answering false,
	// so a follower never mistakes an unreachable database for a quiet one.
	closed := poolOfOne(t)
	closed.Close()
	if _, err := AdvisoryLockHeld(ctx, closed, dedicatedTestLockKey); err == nil {
		t.Fatal("probing a closed pool returned no error; an unreachable database must not read as an unheld lock")
	}
}
