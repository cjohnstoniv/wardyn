// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// TestDefaultPool_ReaperTickBesideLifetimeLocks is the compose stack's first
// reaper tick on a 4-CPU host: the DSN leaves pool_max_conns unset, and the
// single-instance lock, the sweeper leader and the ground-truth rotator each
// hold a connection for the process lifetime. On pgx's default pool (4 there)
// the tick took the last connection for its lock and waited forever for
// another to prune, and every request after it hung. Run it under
// `taskset -c 0-3` to see the old default fail.
func TestDefaultPool_ReaperTickBesideLifetimeLocks(t *testing.T) {
	pgPool(t) // skips without WARDYN_TEST_PG; migrates
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := db.Connect(ctx, os.Getenv("WARDYN_TEST_PG"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	for range 3 {
		c, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("hold a lifetime connection: %v", err)
		}
		defer c.Release()
	}

	var release func()
	for release == nil && ctx.Err() == nil {
		if r, ok := reapTickLock(pool)(ctx); ok {
			release = r
		} else {
			time.Sleep(50 * time.Millisecond) // another test holds the reaper key for a moment
		}
	}
	if release != nil {
		defer release()
	}
	if release == nil || ctx.Err() != nil {
		t.Fatalf("the reaper's tick waited out its deadline for a connection (pool_max_conns=%d)", pool.Config().MaxConns)
	}
	var one int
	if err := pool.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil {
		t.Fatalf("a request got no connection during the reaper's tick (pool_max_conns=%d): %v", pool.Config().MaxConns, err)
	}
}
