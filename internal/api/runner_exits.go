// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// ExitFromRunner stages a durable candidate on the checked peer read loop.
// It never binds an execution handle or waits for a same-peer teardown RPC.
func (s *Server) ExitFromRunner(ctx context.Context, runnerID uuid.UUID, event runnerwire.Event) error {
	st, ok := s.cfg.Store.(store.PG)
	if !ok || event.Kind != runnerwire.EventAgentExit || event.RunID == nil || len(event.Data) > 4096 {
		return store.ErrRunnerExecutionRefused
	}
	var exit runnerwire.AgentExit
	dec := json.NewDecoder(bytes.NewReader(event.Data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&exit); err != nil {
		return store.ErrRunnerExecutionRefused
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return store.ErrRunnerExecutionRefused
	}
	return st.StageRunnerExit(ctx, runnerID, *event.RunID, exit)
}

// SweepRunnerExits resumes staged transitions and incomplete terminal effects.
// Every page is durable and generation fenced; failures cannot starve later runs.
func (s *Server) SweepRunnerExits(ctx context.Context) error {
	st, ok := s.cfg.Store.(store.PG)
	if !ok || s.cfg.RunnerAuditPrepare == nil {
		return nil
	}
	after := uuid.Nil
	var firstErr error
	failures := 0
	for {
		rows, err := st.ListRunnerExitWork(ctx, after)
		if err != nil {
			return errors.Join(firstErr, err)
		}
		for _, row := range rows {
			bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := s.applyRunnerExit(bounded, st, row)
			cancel()
			if err != nil {
				failures++
				if firstErr == nil {
					firstErr = err
				}
			}
			after = row.RunID
		}
		if len(rows) < 100 || ctx.Err() != nil {
			if failures != 0 {
				return errors.Join(fmt.Errorf("%d runner exits remain pending: %w", failures, firstErr), ctx.Err())
			}
			return ctx.Err()
		}
	}
}

func (s *Server) applyRunnerExit(ctx context.Context, st store.PG, row store.RunnerExitWork) error {
	applied, err := st.AcceptRunnerExit(ctx, row.RunnerID, row.RunID, row.Exit, s.cfg.RunnerAuditPrepare)
	if err != nil {
		return err
	}
	if applied {
		terminal := types.RunCompleted
		if row.Exit.Code != 0 {
			terminal = types.RunFailed
		}
		s.metrics.runTerminal(terminal)
		s.runEvents.moved(row.RunID, types.RunRunning, terminal)
		if terminal == types.RunFailed {
			s.noteModelAccessFromRecording(ctx, row.RunID)
		}
	}
	return st.RetryRunnerExitTail(ctx, row.RunnerID, row.RunID, row.Exit.Generation, func(ctx context.Context, run types.AgentRun, exit runnerwire.AgentExit) error {
		if s.cfg.Runner == nil {
			return errors.New("runner terminal teardown unavailable")
		}
		data := map[string]any{"exit_code": exit.Code, "state": run.State, "generation": exit.Generation}
		err := s.finalizeRunEffects(ctx, run.ID, exit.Ref, "run.complete", data, false, false)
		return errors.Join(withoutQueuedTeardown(err), s.runnerSettlementComplete(ctx, run.ID))
	})
}

// withoutQueuedTeardown drops runner.ErrPendingOnRunner from finalizeRunEffects'
// joined errors. That teardown was queued for the offline runner and audited
// once by finalizeRunEffects; the runner's pending action owns it, so it does
// not keep the exit tail open. Every other error still retries the tail.
func withoutQueuedTeardown(err error) error {
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err
	}
	var rest []error
	for _, e := range joined.Unwrap() {
		if !errors.Is(e, runner.ErrPendingOnRunner) {
			rest = append(rest, e)
		}
	}
	return errors.Join(rest...)
}

func (s *Server) runnerSettlementComplete(ctx context.Context, id uuid.UUID) error {
	run, err := s.cfg.Store.GetRun(ctx, id)
	if err != nil {
		return err
	}
	if run.SourceID != nil {
		source, err := s.cfg.Store.GetSource(ctx, *run.SourceID)
		if err != nil {
			return err
		}
		if source.ActiveRunID != nil && *source.ActiveRunID == id && source.Status == types.WorkspaceScanning {
			return errors.New("runner source settlement pending")
		}
	}
	if run.WorkspaceID != nil {
		ws, err := s.cfg.Store.GetWorkspace(ctx, *run.WorkspaceID)
		if err != nil {
			return err
		}
		if ws.ActiveRunID != nil && *ws.ActiveRunID == id && ws.Status == types.WorkspaceScanning {
			return errors.New("runner workspace settlement pending")
		}
		for _, result := range recordResultsMap(ws) {
			if result.RunID == id && result.Status == recordStatusRecording {
				return errors.New("runner record settlement pending")
			}
		}
	}
	return nil
}

// RunRunnerExitSweeper stays independent of stream lifetime. A receipt remains
// retryable through daemon restart, even after the runner has received its ACK.
func (s *Server) RunRunnerExitSweeper(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if err := s.SweepRunnerExits(ctx); err != nil {
			slog.WarnContext(ctx, "runner exit reconciliation will retry", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
