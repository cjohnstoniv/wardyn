// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
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
	var byID map[uuid.UUID]types.AgentRun
	var loadErr error
	isOrphan := func(runID uuid.UUID) bool {
		if byID == nil && loadErr == nil {
			var runs []types.AgentRun
			if runs, loadErr = s.cfg.Store.ListRuns(ctx); loadErr == nil {
				byID = make(map[uuid.UUID]types.AgentRun, len(runs))
				for _, run := range runs {
					byID[run.ID] = run
				}
			}
		}
		if loadErr != nil {
			return false
		}
		run, known := byID[runID]
		return !known || isTerminalRunState(run.State)
	}
	swept, err := sub.SweepOrphanedSandboxes(ctx, 0, isOrphan)
	if loadErr != nil {
		slog.WarnContext(ctx, "wardynd: runner orphan sweep on reconnect: list runs", slog.String("runner_id", runnerID.String()), slog.Any("err", loadErr))
	}
	if err != nil {
		slog.WarnContext(ctx, "wardynd: runner orphan sweep on reconnect", slog.String("runner_id", runnerID.String()), slog.Any("err", err))
	}
	if swept > 0 {
		slog.InfoContext(ctx, "wardynd: runner orphan sweep on reconnect", slog.String("runner_id", runnerID.String()), slog.Int("swept", swept))
	}
}
