// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// SweepOrphanedSandboxesHolding is SweepOrphanedSandboxes where an orphan held(runID) names is stopped and kept
// instead of removed (api.SandboxOrphanHolder); heldN counts them.
func (d *Driver) SweepOrphanedSandboxesHolding(ctx context.Context, minAge time.Duration, isOrphan, held func(runID uuid.UUID) bool) (swept, heldN int, err error) {
	return d.sweepOrphans(ctx, minAge, isOrphan, held)
}

// holdOrphan stops the run's agent and its proxy and keeps the agent container, with its files. It answers false,
// so the caller tears the sandbox down as before, whenever the stop is not confirmed: any error, and a
// credential-cache wipe that failed the first time.
//
// A re-assert on a sandbox this process already held cleanly is a wipe on a stopped container, which cannot run
// (aws_cache_wipe.go): that answer is accepted only for an id in heldOrphans.
func (d *Driver) holdOrphan(ctx context.Context, runID uuid.UUID) bool {
	err := d.EndSandbox(ctx, agentContainerName(runID))
	switch {
	case err == nil:
		d.heldOrphans.Store(runID, struct{}{})
		return true
	case errors.Is(err, runner.ErrAWSCacheWipeFailed):
		if _, held := d.heldOrphans.Load(runID); held {
			return true
		}
	}
	slog.WarnContext(ctx, "docker: holding an orphaned sandbox failed; it is torn down",
		slog.String("run_id", runID.String()), slog.Any("err", err))
	return false
}
