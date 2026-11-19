// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// strandedClaimReleaser is the store's release of claims whose run was never written
// (store.PG.ReleaseStrandedRunClaims), asked by type assertion like runCapCreator.
type strandedClaimReleaser interface {
	ReleaseStrandedRunClaims(ctx context.Context, olderThan time.Duration) ([]store.StrandedRunClaim, error)
}

// releaseStrandedRunClaims frees each source and workspace that a launch claimed and never wrote a run
// for, once undispatchedGrace has passed, the age past which finalizeUndispatchedRuns already reads a
// launch as abandoned. A record step's entry for that run, still recording, is marked failed with the
// record card's fixed launch-failure sentence.
func (s *Server) releaseStrandedRunClaims(ctx context.Context) error {
	rs, ok := s.cfg.Store.(strandedClaimReleaser)
	if !ok {
		return nil
	}
	released, err := rs.ReleaseStrandedRunClaims(ctx, undispatchedGrace)
	if err != nil {
		return err
	}
	for _, c := range released {
		slog.WarnContext(ctx, "wardynd: released a claim whose run was never written",
			slog.String("kind", c.Kind), slog.String("id", c.ID.String()), slog.String("run_id", c.RunID.String()))
		if c.Kind == "workspace" {
			s.failStrandedRecording(ctx, c.ID, c.RunID)
		}
	}
	return nil
}

// failStrandedRecording marks runID's recording entry on the workspace failed, fenced on it still recording.
func (s *Server) failStrandedRecording(ctx context.Context, wsID, runID uuid.UUID) {
	ws, err := s.cfg.Store.GetWorkspace(ctx, wsID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: could not read the workspace to fail a stranded recording",
			slog.String("workspace_id", wsID.String()), slog.String("run_id", runID.String()), slog.Any("err", err))
		return
	}
	for key, res := range recordResultsMap(ws) {
		if res.RunID != runID || res.Status != recordStatusRecording {
			continue
		}
		now := s.cfg.Now().UTC()
		res.Status, res.FinishedAt, res.FailureHint = recordStatusFailed, &now, recordLaunchFailureHint(nil)
		if _, _, err := s.putRecordResult(ctx, wsID, key, res, recordStatusRecording); err != nil {
			slog.WarnContext(ctx, "wardynd: could not fail a stranded recording",
				slog.String("workspace_id", wsID.String()), slog.String("run_id", runID.String()), slog.Any("err", err))
		}
	}
}
