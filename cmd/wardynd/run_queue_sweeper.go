// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/db"
)

// runQueueSweepInterval is how often the pool queue's leader sweep ticks: it ends waiting runs past their
// deadline, fails promoted runs whose start died, and promotes for every pool with waiting runs, so a
// slot that freed with no commit this replica saw still starts the next run within ten seconds. Not
// configurable, like the other fixed housekeeping cadences.
const runQueueSweepInterval = 10 * time.Second

// runQueueSweeper is the *api.Server surface the sweep needs, narrowed so a test can drive a tick
// without a store.
type runQueueSweeper interface {
	SweepRunQueue(context.Context) error
}

// runRunQueueSweeper ticks every interval until ctx ends.
func runRunQueueSweeper(ctx context.Context, srv runQueueSweeper, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := srv.SweepRunQueue(ctx); err != nil {
			slog.WarnContext(ctx, "wardynd: run queue sweep error", slog.Any("err", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// startRunQueueSweeper runs the sweep on the elected sweeper leader (db.SweeperLeaderLockKey), each term
// started and stopped with it; a nil leader (a test, or a deployment that never built one) runs it
// unconditionally. Every write is a CAS under the pool's row lock, so a second leader for a moment is
// harmless.
func startRunQueueSweeper(ctx context.Context, leader *db.SweeperLeader, srv runQueueSweeper, interval time.Duration) {
	leaderGo(ctx, leader, "run_queue.sweeper", func(ctx context.Context) {
		runRunQueueSweeper(ctx, srv, interval)
	})
	slog.Info("wardynd: run queue sweeper started", slog.Duration("interval", interval))
}

var _ runQueueSweeper = (*api.Server)(nil)
