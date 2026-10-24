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
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/retrylog"
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
// A receipt that failed is skipped until its own retry is due (runnerExitRetry), and
// one refused runnerExitParkAfter times in a row is parked. It logs each failure itself.
func (s *Server) SweepRunnerExits(ctx context.Context) error {
	st, ok := s.cfg.Store.(store.PG)
	if !ok || s.cfg.RunnerAuditPrepare == nil {
		return nil
	}
	s.exitRetry.prune(s.cfg.Now())
	after := uuid.Nil
	var firstErr error
	failures := 0
	for {
		rows, err := st.ListRunnerExitWork(ctx, after)
		if err != nil {
			s.exitRetry.warn(ctx, s.cfg.Now(), err, "error", err)
			return errors.Join(firstErr, err)
		}
		for _, row := range rows {
			after = row.RunID
			key, now := runnerExitKey{row.RunID, row.Exit.Generation}, s.cfg.Now()
			if !s.exitRetry.due(key, now) {
				continue
			}
			bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := s.applyRunnerExit(bounded, st, row)
			cancel()
			if err == nil {
				s.exitRetry.forget(key)
				continue
			}
			wait, refused := s.exitRetry.failed(key, err, now)
			if refused >= runnerExitParkAfter {
				bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
				parked, perr := st.ParkRunnerExit(bounded, row.RunnerID, row.RunID, row.Exit.Generation, refused, err.Error(), s.cfg.RunnerAuditPrepare)
				cancel()
				if perr == nil {
					s.exitRetry.forget(key)
					if parked {
						slog.WarnContext(ctx, "runner exit receipt parked: refused on every try, it is kept and no longer retried",
							"run_id", row.RunID, "runner_id", row.RunnerID, "generation", row.Exit.Generation, "attempts", refused, "error", err)
					}
					continue
				}
				err = errors.Join(err, perr)
			}
			s.exitRetry.warn(ctx, now, err, "run_id", row.RunID, "error", err, "retry_in", wait)
			failures++
			if firstErr == nil {
				firstErr = err
			}
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
// retryable through daemon restart, even after the runner has received its ACK,
// until its tail completes or it is parked: one refused as a foreign, stale or
// incomplete incarnation on runnerExitParkAfter tries in a row is kept, audited
// and no longer retried. The sweep runs every retrylog.First; a failed receipt
// waits its own doubling delay, and a restart retries every unparked one at once.
func (s *Server) RunRunnerExitSweeper(ctx context.Context) {
	ticker := time.NewTicker(retrylog.First)
	defer ticker.Stop()
	for {
		_ = s.SweepRunnerExits(ctx) // it logs each failure, spaced out
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// runnerExitParkAfter is how many tries in a row the organisation must refuse a receipt as a foreign,
// stale or incomplete incarnation before it is parked. Refusal is permanent: the runner was revoked,
// the run moved on, or a newer generation holds it. Eight tries span about ten minutes of retrylog
// delays (5 s doubling to the 5 min cap), so the last is at least one full capped wait after the
// first: long past any lock wait or replica race that can refuse a receipt once.
const runnerExitParkAfter = 8

// parkedExitStore counts a runner's parked receipts for its one-runner read (store.PG).
type parkedExitStore interface {
	ParkedRunnerExits(ctx context.Context, runnerID uuid.UUID) (int, error)
}

type runnerExitKey struct{ run, generation uuid.UUID }

// runnerExitRetry spaces out the exit sweep's retries of each receipt, and its log lines. A failed
// receipt's next try waits retrylog.Delay of its failures in a row; an identical error is logged once
// per retrylog interval with how often it repeated. Its clock is the server's (Config.Now). The zero
// value is ready to use.
type runnerExitRetry struct {
	mu       sync.Mutex
	receipts map[runnerExitKey]*runnerExitTries
	logs     retrylog.Dedupe
}

type runnerExitTries struct {
	failed, refused int // failures in a row, and of them the refusals in a row
	next, last      time.Time
}

func (b *runnerExitRetry) due(key runnerExitKey, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	t := b.receipts[key]
	return t == nil || !now.Before(t.next)
}

// failed records a failed try and returns the wait until the next, and the refusals in a row.
func (b *runnerExitRetry) failed(key runnerExitKey, err error, now time.Time) (time.Duration, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.receipts == nil {
		b.receipts = map[runnerExitKey]*runnerExitTries{}
	}
	t := b.receipts[key]
	if t == nil {
		t = &runnerExitTries{}
		b.receipts[key] = t
	}
	t.failed++
	t.refused++
	if !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.refused = 0
	}
	wait := retrylog.Delay(1)
	t.next, t.last = now.Add(wait), now
	return wait, t.refused
}

func (b *runnerExitRetry) forget(key runnerExitKey) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.receipts, key)
}

// prune forgets a receipt not tried for twice the cap: no sweep lists it any more (another replica
// completed or parked it).
func (b *runnerExitRetry) prune(now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, t := range b.receipts {
		if now.Sub(t.last) > 2*retrylog.Cap {
			delete(b.receipts, k)
		}
	}
}

// warn logs "runner exit reconciliation will retry" for err unless an identical one was logged within
// its current interval; the line says how many it did not log since.
func (b *runnerExitRetry) warn(ctx context.Context, now time.Time, err error, attrs ...any) {
	b.mu.Lock()
	log, repeated := b.logs.Note(err.Error(), now)
	b.mu.Unlock()
	if log {
		slog.WarnContext(ctx, "runner exit reconciliation will retry", append(attrs, "repeated", repeated)...)
	}
}
