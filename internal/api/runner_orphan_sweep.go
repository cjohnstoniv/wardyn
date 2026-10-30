// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// readySweepRetries are the waits before each further pass of the READY sweep. A pass can fail for a
// reason that clears in minutes: the call or a run read erring, or a candidate the runner holds until
// its pod is proven stopped, which the runner reports as the pass's error. Without a retry the next
// chance was the cadence sweep, an undispatchedGrace later.
var readySweepRetries = []time.Duration{10 * time.Second, 30 * time.Second, 90 * time.Second}

// sweepRunnerOrphansOnReady sweeps one runner's orphans the moment its session is READY, so a run
// whose create died with runnerd (its proxy pod left Running, renewing against a run token the org
// already ended) is reclaimed on reconnect instead of on the next undispatchedGrace sweep.
//
// It takes no age gate, and so a narrower rule than the cadence sweep: only a run that is terminal,
// or has no row at all, is an orphan. A non-terminal run without a ref may be a create still in
// flight on this runner across a reconnect, which only the cadence sweep's age gate may judge. The
// runs are read only after the runner lists its candidates, so any run whose objects it listed was
// already inserted: "no row" is a leak, never a run created after the read.
//
// A pass that errs is tried again after each of waits, while ctx lives and sameSession holds. A retry
// only repeats the call: a held candidate meets the runner's own proof-of-stop hold on every pass and
// goes only once the runner has that proof. Every pass logs one line, swept=0 included.
func (s *Server) sweepRunnerOrphansOnReady(ctx context.Context, runnerID uuid.UUID, sub SandboxOrphanSweeper, sameSession func() bool, waits []time.Duration) {
	for attempt := 0; ; attempt++ {
		if !s.sweepRunnerOrphansOnce(ctx, runnerID, sub, attempt+1) || attempt == len(waits) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(waits[attempt]):
		}
		if ctx.Err() != nil || !sameSession() {
			return
		}
	}
}

// sweepRunnerOrphansOnce is one pass of the READY sweep. It reports whether the pass failed, the
// sweep call or a run read, and so is worth another.
func (s *Server) sweepRunnerOrphansOnce(ctx context.Context, runnerID uuid.UUID, sub SandboxOrphanSweeper, attempt int) bool {
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
	attrs := []any{slog.String("runner_id", runnerID.String()), slog.Int("attempt", attempt), slog.Int("swept", swept)}
	if err == nil && loadErr == nil {
		slog.InfoContext(ctx, "wardynd: runner orphan sweep on reconnect", attrs...)
		return false
	}
	if err != nil {
		attrs = append(attrs, slog.Any("err", err))
	}
	if loadErr != nil {
		attrs = append(attrs, slog.Any("read_run_err", loadErr))
	}
	slog.WarnContext(ctx, "wardynd: runner orphan sweep on reconnect", attrs...)
	return true
}
