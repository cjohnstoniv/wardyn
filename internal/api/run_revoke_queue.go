// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/google/uuid"
)

// startRevokeQueue is replaced by the revoke queue.
func (s *Server) startRevokeQueue() {}

// markRevokeOwed stamps the run's revoke as owed (store.RevokeOwed) and reports whether the mark needs no
// second try: true when it landed, and true when the store keeps no mark. A failed write is logged at warn and
// answers false; the revoke runs regardless.
func (s *Server) markRevokeOwed(ctx context.Context, runID uuid.UUID) bool {
	st, ok := s.cfg.Store.(store.RevokeOwed)
	if !ok {
		return true
	}
	if err := st.MarkRevokeOwed(ctx, runID); err != nil {
		slog.WarnContext(ctx, "wardynd: marking a run's revoke as owed failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return false
	}
	return true
}

// clearRevokeOwed removes the mark after a revoke that returned nil. A failed clear is logged and left: the
// worker retries the run once more, which finds nothing owed and clears it.
func (s *Server) clearRevokeOwed(ctx context.Context, runID uuid.UUID) {
	st, ok := s.cfg.Store.(store.RevokeOwed)
	if !ok {
		return
	}
	if err := st.ClearRevokeOwed(ctx, runID); err != nil {
		slog.WarnContext(ctx, "wardynd: clearing a run's revoke owed mark failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

// settleRevokeOwed ends a revoke attempt. Nothing owed clears the mark. Something owed with the first mark
// not landed writes it again, on a fresh teardownCtx so a cancelled caller still writes it; if that fails too
// (Identity.RevokeRun is itself a database insert, so the database may be down) the run is retried only
// while it holds a sandbox.
func (s *Server) settleRevokeOwed(ctx context.Context, runID uuid.UUID, marked, owed bool) {
	switch {
	case !owed:
		s.clearRevokeOwed(ctx, runID)
	case !marked:
		mctx, cancel := teardownCtx(ctx)
		defer cancel()
		if !s.markRevokeOwed(mctx, runID) {
			slog.ErrorContext(ctx, "wardynd: a run's revoke is owed and could not be recorded; it is retried only while the run holds a sandbox",
				slog.String("run_id", runID.String()))
		}
	}
}
