// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// StateFromRunner runs on H2's checked STATE path. An open action left after
// this session's PENDING page refuses READY; returned containment requests are
// queued before reconciliation-required, so the next fresh PENDING applies
// them. This method performs no runner RPC, renewal or transition.
func (s *Server) StateFromRunner(ctx context.Context, runnerID uuid.UUID, rows []runnerwire.RunState) ([]store.RunnerContainment, error) {
	st, ok := s.cfg.Store.(store.PG)
	if !ok {
		return nil, store.ErrRunnerExecutionRefused
	}
	open, _, err := st.ListPendingRunnerActions(ctx, runnerID)
	if err != nil {
		return nil, err
	}
	if len(open) > 0 {
		return nil, store.ErrRunnerReconciliationRequired
	}
	requests, err := st.ReconcileRunnerState(ctx, runnerID, rows)
	if errors.Is(err, store.ErrRunnerReconciliationRequired) {
		if qerr := queueRunnerContainment(ctx, st, runnerID, requests); qerr != nil {
			return requests, errors.Join(err, qerr)
		}
	}
	return requests, err
}

type runnerLossClaim struct {
	store.PG
	runnerID uuid.UUID
	ref      string
}

func (s runnerLossClaim) MarkRunLost(ctx context.Context, id uuid.UUID, _ types.LostReason, at time.Time, _ time.Duration) (bool, error) {
	return s.MarkRunnerRunLost(ctx, s.runnerID, id, s.ref, at)
}

// OfflineFromRunner is an off-read-loop callback. A fresh connection loss does
// nothing: only DB-token-lapsed candidates enter the existing kept-loss writer,
// approval cancellation, broker revocation and stop-proxy/queued containment.
// The committing loss claim rechecks the authenticated owner and current lapse.
func (s *Server) OfflineFromRunner(ctx context.Context, runnerID uuid.UUID) error {
	st, ok := s.cfg.Store.(store.PG)
	if !ok || s.cfg.Runner == nil {
		return store.ErrRunnerExecutionRefused
	}
	after := uuid.Nil
	for {
		rows, err := st.ListRunnerLapsedRuns(ctx, runnerID, after)
		if err != nil {
			return err
		}
		for _, run := range rows {
			bounded, cancel := context.WithTimeout(ctx, reconcileFinalizeTimeout)
			s.loseRun(bounded, runnerLossClaim{PG: st, runnerID: runnerID, ref: run.SandboxRef}, st, run, types.LostOutage, types.RunFailed, runTokenLapseAfter, s.cfg.Now())
			cancel()
			after = run.ID
		}
		if len(rows) < 100 || ctx.Err() != nil {
			return ctx.Err()
		}
	}
}
