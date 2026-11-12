// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/google/uuid"
)

const (
	// revokeOwedTick is how often each replica looks for runs whose revoke is owed.
	revokeOwedTick = 60 * time.Second
	// revokeOwedAge is how long a revoke has been owed before the worker takes it, so one still running on
	// the replica that started it is left alone.
	revokeOwedAge = time.Minute
	// revokeOwedPage is how many runs one tick takes.
	revokeOwedPage = 50
	// revokeOwedBound bounds one run's retry.
	revokeOwedBound = 20 * time.Second
)

// startRevokeQueue starts the worker that retries owed revokes, on every replica.
func (s *Server) startRevokeQueue() {
	s.goBackground(func() { s.revokeOwedLoop(s.watcherBaseCtx(), revokeOwedTick) })
}

// revokeOwedLoop runs revokeOwedPass every `every` until ctx ends. The cursor carries between ticks, so rows
// that keep failing cannot starve newer ones. Panic-safe per tick like the watcher sweeper; the interval is a
// parameter only so tests can drive it fast.
func (s *Server) revokeOwedLoop(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	var cursor *store.RevokeOwedCursor
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						slog.ErrorContext(ctx, "wardynd: PANIC in the revoke owed worker (contained; it continues next tick)",
							slog.Any("panic", r))
					}
				}()
				cursor = s.revokeOwedPass(ctx, cursor)
			}()
		}
	}
}

// revokeOwedPass retries one page of owed revokes from after, and returns where the next page starts: nil
// when the page was short, so the next pass starts again from the oldest. A failed read keeps the position.
func (s *Server) revokeOwedPass(ctx context.Context, after *store.RevokeOwedCursor) *store.RevokeOwedCursor {
	st, ok := s.cfg.Store.(store.RevokeOwed)
	if !ok {
		return nil
	}
	ids, next, err := st.ListRevokeOwedAfter(ctx, revokeOwedAge, after, revokeOwedPage)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: listing the runs whose revoke is owed failed; the next tick tries again", slog.Any("err", err))
		return after
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return after
		}
		s.retryOwedRevoke(ctx, id)
	}
	return next
}

// retryOwedRevoke runs one owed revoke. revokeRunCascade clears the mark itself when it returns nil; under
// forcedCtx a failure writes its run.revoke row once per process per run, not once a tick.
func (s *Server) retryOwedRevoke(ctx context.Context, id uuid.UUID) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "wardynd: PANIC retrying an owed revoke (contained)",
				slog.String("run_id", id.String()), slog.Any("panic", r))
		}
	}()
	rctx, cancel := context.WithTimeout(ctx, revokeOwedBound)
	defer cancel()
	if err := s.revokeRunCascade(forcedCtx(rctx), id); err == nil {
		s.forgetTeardownRow(id, "run.revoke")
	}
}

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
