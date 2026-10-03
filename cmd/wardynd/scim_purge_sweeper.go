// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/db"
)

// scimPurgeSweepInterval is how often the purge sweeper ticks. The purge delay is measured in days, so
// the interval only bounds how late a purge or a resumed suspension runs; not configurable.
const scimPurgeSweepInterval = 15 * time.Minute

// scimPurgeSweeper is the *api.Server surface the sweep needs, narrowed so a test can drive a tick
// without a store.
type scimPurgeSweeper interface {
	SweepSCIMPurge(context.Context) error
}

// runSCIMPurgeSweeper ticks the sweep every interval until ctx ends; the first tick is one interval in,
// as for the other sweepers. It records no sweep-health tick (ponytail: a name in the closed sweephealth
// set, its metric and its status row, if an operator needs to see this sweep stall).
func runSCIMPurgeSweeper(ctx context.Context, srv scimPurgeSweeper, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := srv.SweepSCIMPurge(ctx); err != nil {
				slog.WarnContext(ctx, "wardynd: scim purge sweep error", slog.Any("err", err))
			}
		}
	}
}

// startSCIMPurgeSweeper runs the sweep on the elected sweeper leader (db.SweeperLeaderLockKey), each
// term started and stopped with it; a nil leader runs it unconditionally. Only when SCIM is on. Each
// purge re-checks under the identity row lock, so a second leader for a moment purges nobody twice.
func startSCIMPurgeSweeper(ctx context.Context, f *bootFlags, leader *db.SweeperLeader, srv scimPurgeSweeper, interval time.Duration) {
	if f.scimToken == nil || strings.TrimSpace(*f.scimToken) == "" {
		return
	}
	leaderGo(ctx, leader, "scim.purge.sweeper", func(ctx context.Context) {
		runSCIMPurgeSweeper(ctx, srv, interval)
	})
	slog.Info("wardynd: scim purge sweeper started", slog.Duration("interval", interval))
}

var _ scimPurgeSweeper = (*api.Server)(nil)
