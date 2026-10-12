// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
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

// runRunQueueSweeper ticks every interval until ctx ends. ticks records each tick as the run_queue
// sweep (nil records nothing); an error is an attempt without a success.
func runRunQueueSweeper(ctx context.Context, srv runQueueSweeper, interval time.Duration, ticks *sweephealth.Tracker) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		_ = ticks.Tick(ctx, sweephealth.RunQueue, func(ctx context.Context) error {
			err := srv.SweepRunQueue(ctx)
			if err != nil {
				slog.WarnContext(ctx, "wardynd: run queue sweep error", slog.Any("err", err))
			}
			return err
		})
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
func startRunQueueSweeper(ctx context.Context, leader *db.SweeperLeader, srv runQueueSweeper, interval time.Duration, ticks *sweephealth.Tracker) {
	leaderGo(ctx, leader, "run_queue.sweeper", func(ctx context.Context) {
		runRunQueueSweeper(ctx, srv, interval, ticks)
	})
	slog.Info("wardynd: run queue sweeper started", slog.Duration("interval", interval))
}

var _ runQueueSweeper = (*api.Server)(nil)
