// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// StatePageFromRunner is Admission.StatePage, on the peer read loop: one
// short intake transaction, no runner call. It answers nil only after the page
// committed. A Final page needing reconciliation refuses READY; any stop_proxy
// it requires committed with its page. An open action left after this
// session's PENDING page refuses READY too, so the next session carries it.
func (s *Server) StatePageFromRunner(ctx context.Context, runnerID uuid.UUID, sessionID string, page runnerwire.StatePage) error {
	st, ok := s.cfg.Store.(store.PG)
	if !ok {
		return store.ErrRunnerExecutionRefused
	}
	result, err := st.IntakeRunnerStatePage(ctx, runnerID, sessionID, page)
	if err == nil && result.AssertedCapHeld {
		s.RecordRunnerInventoryHold(ctx, runnerID)
	}
	if err != nil || !result.Complete {
		return err
	}
	if result.NeedsReconciliation {
		return store.ErrRunnerReconciliationRequired
	}
	open, _, err := st.ListPendingRunnerActions(ctx, runnerID)
	if err != nil {
		return err
	}
	if len(open) > 0 {
		return store.ErrRunnerReconciliationRequired
	}
	return nil
}

// RecordRunnerInventoryHold writes runner.inventory.hold: a page reported a
// run the org has no row for, past the runner's cap of asserted subjects, so
// nothing was queued for it and the runner stays not-ready until an operator
// acts. Its owner and admins see the runner's audit; other runners continue.
func (s *Server) RecordRunnerInventoryHold(ctx context.Context, id uuid.UUID) {
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "runner:"+id.String(),
		placement.ActionRunnerInventoryHold, id.String(), "denied", mustJSON(map[string]any{"reason": "asserted_cap", "cap": types.RunnerAssertedSubjectCap})))
}

// purgeAssertedRunnerSubjects deletes the org rows of asserted subjects whose
// quarantine succeeded more than types.RunnerAssertedEvidenceTTL ago, on the
// orphan sweep's cadence.
func (s *Server) purgeAssertedRunnerSubjects(ctx context.Context) error {
	st, ok := s.cfg.Store.(store.PG)
	if !ok {
		return nil
	}
	n, err := st.PurgeAssertedRunnerSubjects(ctx)
	if n > 0 {
		slog.InfoContext(ctx, "wardynd: purged asserted runner subjects", slog.Int64("rows", n))
	}
	return err
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

// runnerOfflineSweeps runs OfflineFromRunner off the Hub's session slot: at
// most one sweep per runner at a time, and every signal that arrives during a
// sweep coalesces into exactly one more. The periodic lapse sweeps remain the
// backstop for a sweep that fails.
type runnerOfflineSweeps struct {
	mu    sync.Mutex
	again map[uuid.UUID]bool
}

func (q *runnerOfflineSweeps) schedule(ctx context.Context, id uuid.UUID, sweep func(context.Context, uuid.UUID) error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, running := q.again[id]; running {
		q.again[id] = true
		return
	}
	if q.again == nil {
		q.again = map[uuid.UUID]bool{}
	}
	q.again[id] = false
	go func() {
		for {
			if err := sweep(ctx, id); err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "wardynd: runner offline sweep failed", slog.String("runner_id", id.String()), slog.Any("err", err))
			}
			q.mu.Lock()
			if !q.again[id] {
				delete(q.again, id)
				q.mu.Unlock()
				return
			}
			q.again[id] = false
			q.mu.Unlock()
		}
	}()
}
