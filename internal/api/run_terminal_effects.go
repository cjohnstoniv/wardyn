// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// finalizeRunEffects is shared by ordinary finalization and durable runner exit
// receipts, whose success audit already committed with their state transition.
func (s *Server) finalizeRunEffects(ctx context.Context, runID uuid.UUID, ref, action string, data map[string]any, finishAfterStop, gracefulStop bool) error {
	errs := []error{s.revokeRunCascade(ctx, runID), s.cancelRunApprovals(ctx, runID)}
	if !finishAfterStop {
		s.prepareRunOutput(ctx, runID, gracefulStop)
		s.finishRunOutput(ctx, runID)
	}
	teardownOK := false
	if ref != "" && s.cfg.Runner != nil {
		if serr := s.cfg.Runner.StopSandbox(ctx, ref); serr != nil {
			errs = append(errs, serr)
			td := make(map[string]any, len(data)+1)
			for k, v := range data {
				td[k] = v
			}
			td["teardown_error"] = serr.Error()
			s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", action,
				ref, "failure", mustJSON(td)))
		} else {
			teardownOK = true
		}
	}
	if finishAfterStop {
		s.FinishRunOutput(ctx, runID)
	}
	s.reconcileWorkspaceRun(ctx, runID)
	s.reconcileRecordRun(ctx, runID)
	// Clear the ref LAST, and only when this call's own StopSandbox succeeded.
	// LAST because reconcileRecordRun above still reads run.SandboxRef to tell
	// "the sandbox never started" from "capture was empty". Success-only because
	// the boot orphan sweep keys on a terminal run STILL carrying a ref — after
	// this line that means exactly "teardown failed", the case it exists for.
	// Kill/idle-stop tear down on their own paths and never clear; the evidence
	// endpoints' terminal-state guard covers those runs. A failed clear is
	// deliberately ignored: it degrades to the pre-clear behavior the boot
	// sweep already retries.
	if teardownOK && s.cfg.Store != nil {
		errs = append(errs, s.clearSandboxRef(ctx, runID))
	}
	return errors.Join(append(errs, ctx.Err())...)
}

// clearSandboxRef clears a terminal run's ref. On Postgres a runner run's
// binding is snapshotted first, in the same transaction, so its retirement
// can still bind the runner's kill and retire_run results to it.
func (s *Server) clearSandboxRef(ctx context.Context, runID uuid.UUID) error {
	if st, ok := s.cfg.Store.(store.PG); ok {
		return st.ClearSandboxRefRetaining(ctx, runID)
	}
	return s.cfg.Store.SetSandboxRef(ctx, runID, "")
}

// recordRunnerOutputWatermark records, before a runner run's create, whether
// the create asks the runner to keep its output. A failed write leaves the run
// with no row, which only means it is never retired (its runner keeps its
// output and spool, as before retirement existed).
func (s *Server) recordRunnerOutputWatermark(ctx context.Context, run types.AgentRun, capture bool) {
	st, ok := s.cfg.Store.(store.PG)
	if !ok || run.RunnerID == nil {
		return
	}
	if err := st.InsertRunnerOutputWatermark(ctx, run.ID, capture); err != nil {
		slog.WarnContext(ctx, "wardynd: runner run output watermark not recorded; the run will not be retired",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}
}

// produceRunnerRetirements is the run watcher's retirement pass: it abandons
// the open retirements of revoked runners, then takes one step of each
// eligible terminal runner run's retirement (store.AdvanceRunnerRetirement).
// The runner receives what it queues at its next fresh session.
func (s *Server) produceRunnerRetirements(ctx context.Context) error {
	st, ok := s.cfg.Store.(store.PG)
	if !ok || s.cfg.RunnerAuditPrepare == nil {
		return nil
	}
	if _, err := st.AbandonRevokedRunnerRetirements(ctx, s.cfg.RunnerAuditPrepare); err != nil {
		return err
	}
	candidates, err := st.RunnerRetirementCandidates(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range candidates {
		// An offline runner answers nothing; its runs wait for it to connect.
		if !s.runnerEndpoint(c.RunnerID).link.Online() {
			continue
		}
		if _, err := st.AdvanceRunnerRetirement(ctx, c.RunID, s.cfg.RunnerAuditPrepare); err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, fmt.Errorf("run %s: %w", c.RunID, err))
		}
	}
	return errors.Join(errs...)
}
