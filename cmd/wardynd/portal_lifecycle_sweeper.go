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

// portalLifecycleSweepInterval is how often the portal lifecycle sweep ticks. A purge deadline is an admin's
// choice and a resumed suspension is a repair, so the interval only bounds how late they run; not configurable.
const portalLifecycleSweepInterval = 15 * time.Minute

// portalLifecycleSweeper is the *api.Server surface the sweep needs, narrowed so a test can drive a tick
// without a store.
type portalLifecycleSweeper interface {
	SweepPortalLifecycle(context.Context) error
}

// runPortalLifecycleSweeper ticks the sweep every interval until ctx ends; the first tick is one interval in, as
// for the other sweepers.
func runPortalLifecycleSweeper(ctx context.Context, srv portalLifecycleSweeper, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := srv.SweepPortalLifecycle(ctx); err != nil {
				slog.WarnContext(ctx, "wardynd: portal lifecycle sweep error", slog.Any("err", err))
			}
		}
	}
}

// startPortalLifecycleSweeper runs the portal sweep on the elected sweeper leader, each term started and
// stopped with it; a nil leader runs it unconditionally. Unlike the SCIM sweep it does not wait for a SCIM
// token: it runs whatever the SCIM setting is, and does its work only once issuer portals are activated (the
// sweep reads that itself). Every purge re-checks under the identity row lock, so a second leader for a moment
// purges nobody twice.
func startPortalLifecycleSweeper(ctx context.Context, leader *db.SweeperLeader, srv portalLifecycleSweeper, interval time.Duration) {
	leaderGo(ctx, leader, "portal.lifecycle.sweeper", func(ctx context.Context) {
		runPortalLifecycleSweeper(ctx, srv, interval)
	})
	slog.Info("wardynd: portal lifecycle sweeper started", slog.Duration("interval", interval))
}

var _ portalLifecycleSweeper = (*api.Server)(nil)
