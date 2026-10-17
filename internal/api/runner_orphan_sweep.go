// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// sweepRunnerOrphansOnReady sweeps one runner's orphans the moment its session is READY, so a run
// whose create died with runnerd (its proxy pod left Running, renewing against a run token the org
// already ended) is reclaimed on reconnect instead of on the next undispatchedGrace sweep.
//
// It takes no age gate, and so a narrower rule than the cadence sweep: only a run that is terminal,
// or has no row at all, is an orphan. A non-terminal run without a ref may be a create still in
// flight on this runner across a reconnect, which only the cadence sweep's age gate may judge. The
// runs are read only after the runner lists its candidates, so any run whose objects it listed was
// already inserted: "no row" is a leak, never a run created after the read.
func (s *Server) sweepRunnerOrphansOnReady(ctx context.Context, runnerID uuid.UUID, sub SandboxOrphanSweeper) {
	var loadErr error
	isOrphan := func(runID uuid.UUID) bool {
		run, err := s.cfg.Store.GetRun(ctx, runID)
		if errors.Is(err, store.ErrNotFound) {
			return true
		}
		if err != nil {
			loadErr = err
			return false
		}
		return isTerminalRunState(run.State)
	}
	swept, err := sub.SweepOrphanedSandboxes(ctx, 0, isOrphan)
	if loadErr != nil {
		slog.WarnContext(ctx, "wardynd: runner orphan sweep on reconnect: read run", slog.String("runner_id", runnerID.String()), slog.Any("err", loadErr))
	}
	if err != nil {
		slog.WarnContext(ctx, "wardynd: runner orphan sweep on reconnect", slog.String("runner_id", runnerID.String()), slog.Any("err", err))
	}
	if swept > 0 {
		slog.InfoContext(ctx, "wardynd: runner orphan sweep on reconnect", slog.String("runner_id", runnerID.String()), slog.Int("swept", swept))
	}
}
