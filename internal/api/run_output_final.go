// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The run-output finalisation contract (out-o2). Every path that ends a run
// goes through it, so a run's captured output is persisted once, masked, after
// the last bytes arrived, and never silently lost to a crash.
//
// It has two halves, both idempotent per run:
//
//   - prepareRunOutput, before teardown: a process that finalises a run it holds
//     no tail for recovers the output from the substrate, when its masking
//     manifest covers the run and the substrate can give it back, and otherwise
//     writes a capture_gap row (run_output_recover.go).
//   - finishRunOutput, once the process has exited or the sandbox is gone: the
//     drain barrier, the holdback flush, the fence and the persist.
//
// Entry points (each wired where the run ends): finalizeRunTail (the watcher's
// win, the boot reconciler, the lease end, the site-config probe reclaim), the
// watcher's CAS-loss returns, the kill, the idle stop (lifecycleStopper, through
// FinishRunOutput), and failAndRevoke. A watcher that finds its run kept does
// NOT finish: a kept run can be revived with its agent started again on the
// same run id, so the tail stays open until the run is terminal.

const (
	// runOutputDrainWait bounds how long finishRunOutput waits for the drivers'
	// copies into the tail to end after the process has exited.
	runOutputDrainWait = 5 * time.Second
	// runOutputSyncAttempts is how many times the final write is tried inline,
	// with backoff, before it is handed to a tracked retry loop.
	runOutputSyncAttempts = 3
	// runOutputRetryBase and runOutputRetryMax cap the backoff of those retries.
	runOutputRetryBase = 200 * time.Millisecond
	runOutputRetryMax  = 30 * time.Second
	// runOutputPendingStale is how long a pending row of a terminal run may sit
	// before the retention sweeper resolves it to a capture_gap row.
	runOutputPendingStale = 5 * time.Minute

	maskScopeRun = "run"
)

// outputCapture is the part of a liveMaskWriter that only a run's output tail
// uses. sealed: the capture is final, so a later write is accepted and dropped,
// never forwarded (the driver contract forbids failing it). dropped: a chunk was
// lost, to the seal or to the masking guard. uncovered: the guard turned false at
// some point, sticky, so the capture says it was not masked against the run's
// complete manifest throughout. onLate, when set, is called once, outside the
// writer's lock, for the first byte dropped after the seal. Guarded by the
// writer's mutex.
type outputCapture struct {
	sealed, dropped, uncovered bool
	lateSeen                   bool
	onLate                     func()
}

// dropSealed is the write path's first question: after the seal the chunk is
// dropped, and late is the one-time notice to call once the lock is released.
func (c *outputCapture) dropSealed() (late func(), sealed bool) {
	if !c.sealed {
		return nil, false
	}
	c.dropped = true
	if !c.lateSeen {
		c.lateSeen = true
		late = c.onLate
	}
	return late, true
}

// tailWriter is what the runner receives as SandboxSpec.ExecOutput: the masking
// writer's pipe, plus runner.OutputDrainer so the drivers report the start and
// end of each copy into it and the finisher can wait for the last bytes.
type tailWriter struct {
	t *execOutputTail
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.t.arrived.Store(w.t.ring.now().UnixNano())
	return w.t.in.Write(p)
}

func (w *tailWriter) BeginDrain() { w.t.beginDrain() }

// EndDrain must not block (runner.OutputDrainer), and the drain may end only
// once the copy's bytes are through the masker, or the finisher would seal ahead
// of them: the end waits for that on its own goroutine.
func (w *tailWriter) EndDrain(err error) {
	go func() {
		w.t.in.flush()
		w.t.endDrain(err)
	}()
}

// flushIn waits until every byte a driver has written so far is in the ring.
func (e *execOutputTail) flushIn() {
	if e.in != nil {
		e.in.flush()
	}
}

func (e *execOutputTail) beginDrain() {
	e.dmu.Lock()
	e.drains++
	e.wakeDrainLocked()
	e.dmu.Unlock()
}

func (e *execOutputTail) endDrain(err error) {
	e.dmu.Lock()
	if e.drains > 0 {
		e.drains--
	}
	if err != nil {
		e.drainErr = true
	}
	e.wakeDrainLocked()
	e.dmu.Unlock()
}

func (e *execOutputTail) wakeDrainLocked() {
	close(e.drainWake)
	e.drainWake = make(chan struct{})
}

// awaitDrains waits until every copy into the tail has ended, up to wait. It
// reports whether they all did, and none ended in an error.
func (e *execOutputTail) awaitDrains(ctx context.Context, wait time.Duration) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		e.dmu.Lock()
		n, wake, bad := e.drains, e.drainWake, e.drainErr
		e.dmu.Unlock()
		if n == 0 {
			return !bad
		}
		select {
		case <-wake:
		case <-timer.C:
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// seal ends the capture: it flushes the masker's holdback into the ring (unless
// the run is uncovered, when the held bytes cannot be vouched for and are
// dropped), then fences the writer so a later byte is dropped, not kept. It
// returns what the row holds.
func (e *execOutputTail) seal(uncoveredNow bool) (out []byte, truncated, dropped, uncovered bool) {
	e.flushIn() // a write accepted before the seal is in the row
	e.mw.mu.Lock()
	defer e.mw.mu.Unlock()
	if uncoveredNow {
		e.mw.capture.uncovered, e.mw.capture.dropped = true, e.mw.capture.dropped || len(e.mw.tail) > 0
	} else if held := releasedHoldback(e.mw.tail); len(held) > 0 {
		_, _ = e.sink.Write(held)
	}
	if e.sink.q != nil {
		e.sink.q.stop() // sealed: the final row replaces the chunks
	}
	e.mw.tail = nil
	e.mw.capture.sealed = true
	e.fmu.Lock()
	e.finished = true
	e.fmu.Unlock()
	return append([]byte{}, e.ring.buf...), e.ring.truncated, e.mw.capture.dropped, e.mw.capture.uncovered
}

// liveMaskScope is the mask_scope a capture carries: "" when this deployment
// keeps no masking manifests (nothing was proven either way), "run" when the
// writer held the run's complete manifest throughout, else "globals_only".
func (s *Server) liveMaskScope(uncovered bool) string {
	switch {
	case s.cfg.MaskManifests == nil:
		return ""
	case uncovered:
		return maskScopeGlobalsOnly
	}
	return maskScopeRun
}

func (s *Server) tailFor(runID uuid.UUID) *execOutputTail {
	s.execOutputs.mu.Lock()
	defer s.execOutputs.mu.Unlock()
	return s.execOutputs.m[runID]
}

func (s *Server) outputDrainWait() time.Duration {
	if s.runOutputDrainWaitOverride > 0 {
		return s.runOutputDrainWaitOverride
	}
	return runOutputDrainWait
}

func (s *Server) outputRetryBase() time.Duration {
	if s.runOutputRetryBaseOverride > 0 {
		return s.runOutputRetryBaseOverride
	}
	return runOutputRetryBase
}

// FinishRunOutput is both halves of the contract, for a caller outside this
// package (cmd/wardynd's idle stop) that has already stopped the sandbox.
func (s *Server) FinishRunOutput(ctx context.Context, runID uuid.UUID) {
	s.prepareRunOutput(ctx, runID, false)
	s.finishRunOutput(ctx, runID)
}

// finishRunOutputDetached runs the contract on a tracked goroutine, so the
// caller (the kill) never waits for the drain barrier or the write.
func (s *Server) finishRunOutputDetached(ctx context.Context, runID uuid.UUID) {
	ctx = context.WithoutCancel(ctx)
	s.goBackground(func() {
		ctx, cancel := context.WithTimeout(ctx, killCascadeTimeout)
		defer cancel()
		s.FinishRunOutput(ctx, runID)
	})
}

// prepareRunOutput is the half before teardown. A process that holds no tail
// for a run that owes one recovers the output from the substrate when that is
// possible and safe, and otherwise writes the capture_gap row (run_output_recover.go):
// boot adoption, the reconciler, and the sweeper reach this on a restart or on
// another replica, where the bytes are not here. A run that is unrecordable or
// kept off the books owes nothing. graceful is true only when Wardyn is stopping
// a live sandbox into STOPPED (the idle and max-age stops and the lease end): an
// interactive run then keeps a snapshot of its pane (run_output_snapshot.go). Any
// other interactive run keeps nothing here.
func (s *Server) prepareRunOutput(ctx context.Context, runID uuid.UUID, graceful bool) {
	st := s.runOutputStore()
	if st == nil || s.cfg.ExecOutputTailOff || s.cfg.Store == nil {
		return
	}
	hasTail := s.tailFor(runID) != nil
	if hasTail && !graceful {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		return
	}
	switch {
	case run.Interactive:
		if graceful {
			s.snapshotRunPane(ctx, st, run)
		}
	case runIsUnrecordable(run), hasTail:
	default:
		s.recoverRunOutput(ctx, st, run, "no_tail")
	}
}

// writeGapRow resolves runID's pending row (or writes the row) as a capture
// gap with no bytes, never over a final row, and audits it.
func (s *Server) writeGapRow(ctx context.Context, st store.RunOutputStore, runID uuid.UUID, reason string) {
	if s.saveRowFromChunks(ctx, runID, reason) {
		return
	}
	wrote, err := st.SaveGapRunOutput(ctx, runID)
	switch {
	case errors.Is(err, store.ErrRunOutputErased):
		s.fenceRunOutput(runID)
	case err != nil:
		slog.WarnContext(ctx, "wardynd: could not write a run's capture-gap row",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	case wrote:
		s.auditOutputFinalize(ctx, runID, map[string]any{"capture_gap": true, "reason": reason})
	}
}

// finishRunOutput is the half after the process has exited or the sandbox is
// gone. The first caller does the work; a later one waits for it (bounded by
// ctx) and returns. A tail the process does not hold, or one already
// committed and released, is a no-op.
func (s *Server) finishRunOutput(ctx context.Context, runID uuid.UUID) {
	e := s.tailFor(runID)
	if e == nil {
		return
	}
	e.fmu.Lock()
	if e.started {
		done := e.done
		e.fmu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		return
	}
	e.started = true
	e.fmu.Unlock()
	defer close(e.done)

	st := s.runOutputStore()
	if st != nil {
		// The terminal-time claim: the sweeper must not read the old dispatch-time
		// one as an abandoned capture while this process is finishing it.
		if err := st.RefreshRunOutputClaim(ctx, runID); errors.Is(err, store.ErrRunOutputErased) {
			s.fenceRunOutput(runID)
			return
		}
	}
	clean := e.awaitDrains(ctx, s.outputDrainWait())
	uncoveredNow := s.cfg.MaskManifests != nil && !s.maskCovered(ctx, runID)
	out, truncated, dropped, uncovered := e.seal(uncoveredNow)
	e.fmu.Lock()
	fenced, gap := e.fenced, e.gapReason
	if gap == "" && e.recovered && uncovered {
		gap = "mask_uncovered" // a recovery never persists a row masked by the globals alone
	}
	e.fmu.Unlock()
	if fenced {
		return
	}
	if gap != "" {
		s.releaseRunOutput(runID, e)
		if st != nil {
			s.writeGapRow(ctx, st, runID, gap)
		}
		return
	}
	reasons := map[string]any{}
	if !clean {
		reasons["drain"] = true
	}
	if dropped {
		reasons["dropped"] = true
	}
	incomplete := !clean || dropped
	if st == nil {
		if incomplete {
			reasons["incomplete"] = true
			s.auditOutputFinalize(ctx, runID, reasons)
		}
		return
	}
	row := store.RunOutput{
		RunID: runID, Output: out, Truncated: truncated, Incomplete: incomplete,
		Source: "stdout", MaskScope: s.liveMaskScope(uncovered),
	}
	s.persistFinalOutput(ctx, e, st, row, reasons)
}

type persistOutcome int

const (
	persistFailed persistOutcome = iota
	persistDone
	persistStop // erased or fenced: nothing more to write, ever
)

// persistFinalOutput writes the final row: a few attempts inline with capped
// backoff, then a tracked retry loop for as long as the process lives. The
// bytes stay in memory meanwhile (bounded by the tail size per run), and each
// failed attempt refreshes the pending row's claim so the sweeper leaves it.
func (s *Server) persistFinalOutput(ctx context.Context, e *execOutputTail, st store.RunOutputStore, row store.RunOutput, reasons map[string]any) {
	backoff := s.outputRetryBase()
	for attempt := 1; attempt <= runOutputSyncAttempts; attempt++ {
		switch s.tryPersistOutput(ctx, e, st, &row) {
		case persistDone:
			if row.Incomplete {
				reasons["incomplete"] = true
				s.auditOutputFinalize(ctx, row.RunID, reasons)
			}
			return
		case persistStop:
			return
		}
		if attempt < runOutputSyncAttempts {
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, runOutputRetryMax)
		}
	}
	reasons["persist_failing"] = true
	s.auditOutputFinalize(ctx, row.RunID, reasons)
	s.goBackground(func() { s.retryPersistOutput(e, st, row, backoff) })
}

// retryPersistOutput is the loop behind persistFinalOutput's inline attempts: it
// stops on success, on a tombstone, or when the daemon shuts down (the pending
// row then remains, and the sweeper resolves it to a capture gap).
func (s *Server) retryPersistOutput(e *execOutputTail, st store.RunOutputStore, row store.RunOutput, backoff time.Duration) {
	base := s.cfg.BaseCtx
	for {
		select {
		case <-base.Done():
			return
		case <-time.After(backoff):
		}
		if s.tryPersistOutput(base, e, st, &row) != persistFailed {
			return
		}
		backoff = min(backoff*2, runOutputRetryMax)
	}
}

// tryPersistOutput is one write of the final row. A byte dropped since the seal
// is folded into the row's incomplete flag first; one dropped after the commit
// is the late-write path's (lateRunOutput).
func (s *Server) tryPersistOutput(ctx context.Context, e *execOutputTail, st store.RunOutputStore, row *store.RunOutput) persistOutcome {
	e.fmu.Lock()
	if e.fenced {
		e.fmu.Unlock()
		return persistStop
	}
	if e.late {
		row.Incomplete = true
	}
	wrote := row.Incomplete
	e.fmu.Unlock()
	err := st.SaveFinalRunOutput(ctx, *row)
	switch {
	case err == nil:
		e.fmu.Lock()
		e.committed = true
		lateSince := e.late && !wrote
		e.fmu.Unlock()
		s.releaseRunOutput(row.RunID, e)
		if lateSince {
			s.markLateIncomplete(ctx, row.RunID)
		}
		return persistDone
	case errors.Is(err, store.ErrRunOutputErased):
		s.fenceRunOutput(row.RunID)
		return persistStop
	}
	slog.WarnContext(ctx, "wardynd: could not write a run's final output row, will retry",
		slog.String("run_id", row.RunID.String()), slog.Any("err", err))
	if rerr := st.RefreshRunOutputClaim(ctx, row.RunID); errors.Is(rerr, store.ErrRunOutputErased) {
		s.fenceRunOutput(row.RunID)
		return persistStop
	}
	return persistFailed
}

// releaseRunOutput drops the memory copy once the row is committed: out of the
// map, and its buffer freed even though the driver still holds the writer until
// the sandbox goes.
func (s *Server) releaseRunOutput(runID uuid.UUID, e *execOutputTail) {
	s.execOutputs.mu.Lock()
	if s.execOutputs.m[runID] == e {
		delete(s.execOutputs.m, runID)
	}
	s.execOutputs.mu.Unlock()
	e.mw.mu.Lock()
	e.ring.buf, e.mw.tail = nil, nil
	e.mw.mu.Unlock()
}

// fenceRunOutput zeroes and drops this process's tail for runID and marks its
// finaliser fenced: the run's output was erased, so nothing may persist or
// serve it. Idempotent; called on any tombstone hit.
func (s *Server) fenceRunOutput(runID uuid.UUID) {
	s.execOutputs.mu.Lock()
	e := s.execOutputs.m[runID]
	delete(s.execOutputs.m, runID)
	s.execOutputs.mu.Unlock()
	if e == nil {
		return
	}
	e.fmu.Lock()
	e.fenced = true
	e.fmu.Unlock()
	e.mw.mu.Lock()
	clear(e.ring.buf[:cap(e.ring.buf)])
	clear(e.mw.tail)
	e.ring.buf, e.mw.tail, e.mw.capture.sealed = nil, nil, true
	e.mw.mu.Unlock()
}

// EraseRunOutputs erases runIDs' output for good: a tombstone and the deletion
// in one transaction (nothing at all if it fails), then this process's tails
// are zeroed and their finalisers fenced. Every other replica converges on its
// next touch, because every write and read checks the tombstone in its own
// transaction. ar-l1.4 calls it from erasure.Orchestrate.
func (s *Server) EraseRunOutputs(ctx context.Context, runIDs []uuid.UUID) error {
	if st, _ := s.cfg.Store.(store.RunOutputStore); st != nil {
		if err := st.EraseRunOutputs(ctx, runIDs); err != nil {
			return err
		}
	}
	for _, id := range runIDs {
		s.fenceRunOutput(id)
	}
	return nil
}

// lateRunOutput is the writer's notice that a byte arrived after the seal. Before
// the row commits, the persister folds it into the row; after, the row is
// marked incomplete and audited, on a tracked goroutine so the driver's Write
// never blocks on Postgres.
func (s *Server) lateRunOutput(runID uuid.UUID, e *execOutputTail) {
	e.fmu.Lock()
	committed, fenced := e.committed, e.fenced
	if !committed && !fenced {
		e.late = true
	}
	e.fmu.Unlock()
	if !committed || fenced {
		return
	}
	s.goBackground(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.cfg.BaseCtx), 10*time.Second)
		defer cancel()
		s.markLateIncomplete(ctx, runID)
	})
}

// markLateIncomplete flips the committed row to incomplete and audits it.
func (s *Server) markLateIncomplete(ctx context.Context, runID uuid.UUID) {
	st := s.runOutputStore()
	if st == nil {
		return
	}
	marked, err := st.MarkRunOutputIncomplete(ctx, runID)
	switch {
	case errors.Is(err, store.ErrRunOutputErased):
		s.fenceRunOutput(runID)
	case err != nil:
		slog.WarnContext(ctx, "wardynd: could not mark a run's output row incomplete",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	case marked:
		s.auditOutputFinalize(ctx, runID, map[string]any{"incomplete": true, "late_write": true})
	}
}

// auditOutputFinalize records a capture that was not clean: its flags, never
// its content.
func (s *Server) auditOutputFinalize(ctx context.Context, runID uuid.UUID, data map[string]any) {
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.output.finalize",
		runID.String(), "failure", mustJSON(data)))
}

// SweepRunOutputs is one pass of the retention sweeper: it deletes final rows
// older than the retention window, then resolves the pending rows of terminal
// runs whose claim went stale to capture-gap rows (when persistence is on, the
// only time pending rows are written). A tail this process still holds is the
// finisher's own, and is left alone.
func (s *Server) SweepRunOutputs(ctx context.Context) error {
	st, _ := s.cfg.Store.(store.RunOutputStore)
	if st == nil {
		return nil
	}
	var firstErr error
	if retention := s.cfg.RunOutputRetention; retention > 0 {
		n, err := st.DeleteRunOutputsOlderThan(ctx, retention)
		switch {
		case err != nil:
			firstErr = err
		case n > 0:
			s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardyn/run-output-sweeper", "run.output.retention.sweep",
				"run_outputs", "success", mustJSON(map[string]any{"deleted": n, "retention_days": int(retention.Hours() / 24)})))
		}
	}
	if s.cfg.RunOutputPersistOff {
		return firstErr
	}
	ids, err := st.ListStalePendingRunOutputs(ctx, runOutputPendingStale, 200)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if s.tailFor(id) == nil {
			s.resolveStalePending(ctx, st, id)
		}
	}
	return firstErr
}

// resolveStalePending resolves a terminal run's abandoned pending row: by
// recovery from the substrate under the same coverage rule and bound as
// finalisation when that is possible, else to a capture-gap row.
func (s *Server) resolveStalePending(ctx context.Context, st store.RunOutputStore, runID uuid.UUID) {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || run.Interactive || runIsUnrecordable(run) {
		s.writeGapRow(ctx, st, runID, "stale_pending")
		return
	}
	s.recoverRunOutput(ctx, st, run, "stale_pending")
	s.finishRunOutput(ctx, runID)
}
