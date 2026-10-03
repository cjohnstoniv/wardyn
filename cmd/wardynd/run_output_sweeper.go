// Copyright 2025 The Wardyn Authors
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

// runOutputSweepInterval is how often the run-output sweep ticks: it deletes
// persisted output past WARDYN_RUN_OUTPUT_RETENTION_DAYS and resolves the
// pending rows of terminal runs whose capture was abandoned. Not configurable,
// like the other fixed housekeeping cadences.
const runOutputSweepInterval = time.Hour

// runOutputSweeper is the *api.Server surface the sweep needs, narrowed so a
// test can drive a tick without a store.
type runOutputSweeper interface {
	SweepRunOutputs(context.Context) error
}

// runRunOutputSweeper ticks the sweep every interval until ctx ends; the first
// tick is one interval in, as for the other sweepers. ticks records each tick
// as the run_output sweep (nil records nothing); an error is an attempt
// without a success.
func runRunOutputSweeper(ctx context.Context, srv runOutputSweeper, interval time.Duration, ticks *sweephealth.Tracker) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = ticks.Tick(ctx, sweephealth.RunOutput, func(ctx context.Context) error {
				err := srv.SweepRunOutputs(ctx)
				if err != nil {
					slog.WarnContext(ctx, "wardynd: run output sweep error", slog.Any("err", err))
				}
				return err
			})
		}
	}
}

// startRunOutputSweeper runs the sweep on the elected sweeper leader
// (db.SweeperLeaderLockKey), each term started and stopped with it; a nil
// leader (a test, or a deployment that never built one) runs it unconditionally.
// Its deletes are idempotent, so a second leader for a moment is harmless.
func startRunOutputSweeper(ctx context.Context, leader *db.SweeperLeader, srv runOutputSweeper, interval time.Duration, ticks *sweephealth.Tracker) {
	leaderGo(ctx, leader, "run_output.sweeper", func(ctx context.Context) {
		runRunOutputSweeper(ctx, srv, interval, ticks)
	})
	slog.Info("wardynd: run output sweeper started", slog.Duration("interval", interval))
}

var _ runOutputSweeper = (*api.Server)(nil)
