// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Postgres-backed test that the real sweeper loops, started through leaderGo,
// run on exactly one of two replicas sharing a database. Two pools stand in for
// two wardynd processes. Guarded by WARDYN_TEST_PG, like the other *_pg_test.go
// files in this package.

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// countingSweeper is a recordingSweepable that counts the sweeps it is asked
// for and reports one deletion each, so the sweeper loop also audits it.
type countingSweeper struct{ calls atomic.Int64 }

func (c *countingSweeper) Sweep(time.Duration) (int, error) {
	c.calls.Add(1)
	return 1, nil
}

type countingRecorder struct{ events atomic.Int64 }

func (c *countingRecorder) Record(context.Context, types.AuditEvent) error {
	c.events.Add(1)
	return nil
}

var _ audit.Recorder = (*countingRecorder)(nil)

// TestLeaderGo_RecordingSweepRunsOnOneReplica: two replicas both start the
// recording sweeper; only the elected leader's loop runs, so each tick deletes
// once across both servers and the retention audit row is written once.
func TestLeaderGo_RecordingSweepRunsOnOneReplica(t *testing.T) {
	poolA := pgPool(t)
	poolB, err := pgxpool.New(context.Background(), os.Getenv("WARDYN_TEST_PG"))
	if err != nil {
		t.Fatalf("connect poolB: %v", err)
	}
	defer poolB.Close()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	var sweepA, sweepB countingSweeper
	var recA, recB countingRecorder
	for _, r := range []struct {
		pool *pgxpool.Pool
		sw   *countingSweeper
		rec  *countingRecorder
		name string
	}{{poolA, &sweepA, &recA, "replica-a"}, {poolB, &sweepB, &recB, "replica-b"}} {
		leader := db.NewSweeperLeader(r.pool, r.name)
		go leader.Run(ctx)
		leaderGo(ctx, leader, "recording.sweeper", func(c context.Context) {
			runRecordingSweeper(c, r.sw, r.rec, 20*time.Millisecond, time.Hour)
		})
	}

	deadline := time.Now().Add(10 * time.Second)
	for sweepA.calls.Load()+sweepB.calls.Load() < 10 {
		if time.Now().After(deadline) {
			t.Fatalf("no replica swept: a=%d b=%d", sweepA.calls.Load(), sweepB.calls.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sweepA.calls.Load() > 0 && sweepB.calls.Load() > 0 {
		t.Fatalf("both replicas swept (a=%d b=%d): each tick would delete twice", sweepA.calls.Load(), sweepB.calls.Load())
	}
	if recA.events.Load() > 0 && recB.events.Load() > 0 {
		t.Fatalf("both replicas wrote a retention audit row (a=%d b=%d)", recA.events.Load(), recB.events.Load())
	}
}

// TestLeaderGo_NilLeaderRunsEverywhere: with no leader the sweeper runs on the
// context it was given, as every sweeper did before there was an election.
func TestLeaderGo_NilLeaderRunsEverywhere(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	ran := make(chan struct{})
	leaderGo(ctx, nil, "x", func(c context.Context) {
		if c != ctx {
			t.Error("a nil leader must pass the caller's context through")
		}
		close(ran)
	})
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweeper never started")
	}
	stop()
}
