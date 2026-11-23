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

// runScheduleSweepInterval is how often the repeating schedules' leader sweep ticks: it books each slot due
// within the next two minutes as a one-time scheduled run, so a 30-second tick books every slot ahead of its
// time. Not configurable, like the other fixed housekeeping cadences.
const runScheduleSweepInterval = 30 * time.Second

// runScheduleSweeper is the *api.Server surface the sweep needs, narrowed so a test can drive a tick
// without a store.
type runScheduleSweeper interface {
	RunScheduleSweep(context.Context) error
}

// runRunScheduleSweeper ticks every interval until ctx ends. ticks records each tick as the run_schedule
// sweep (nil records nothing); an error is an attempt without a success.
func runRunScheduleSweeper(ctx context.Context, srv runScheduleSweeper, interval time.Duration, ticks *sweephealth.Tracker) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		_ = ticks.Tick(ctx, sweephealth.RunSchedule, func(ctx context.Context) error {
			err := srv.RunScheduleSweep(ctx)
			if err != nil {
				slog.WarnContext(ctx, "wardynd: run schedule sweep error", slog.Any("err", err))
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

// startRunScheduleSweeper runs the sweep on the elected sweeper leader (db.SweeperLeaderLockKey), each term
// started and stopped with it; a nil leader (a test, or a deployment that never built one) runs it
// unconditionally. Each booking carries an idempotency key for its slot and each advance is a compare-and-set,
// so a second leader for a moment books nothing twice.
func startRunScheduleSweeper(ctx context.Context, leader *db.SweeperLeader, srv runScheduleSweeper, interval time.Duration, ticks *sweephealth.Tracker) {
	leaderGo(ctx, leader, "run_schedule.sweeper", func(ctx context.Context) {
		runRunScheduleSweeper(ctx, srv, interval, ticks)
	})
	slog.Info("wardynd: run schedule sweeper started", slog.Duration("interval", interval))
}

var _ runScheduleSweeper = (*api.Server)(nil)
