// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Recovering a run's output from the substrate (out-o2r). A process that
// finalises or adopts a run it holds no tail for (a restart, another replica)
// reads the agent's output back from the substrate when it can, so the run keeps
// its output, and writes a capture gap when it cannot. The agent is never
// re-run to get it.
//
// Coverage comes first. What a recovery reads is the agent's raw log, and it is
// persisted masked against the run's registry: a process whose registry does not
// hold the run's dispatch-time values would persist them verbatim. So the run's
// manifest is certified (maskCovered, which any replica may do, without the
// watcher lease) before the substrate is touched, and a run with no complete
// manifest, one from before manifests existed or one whose manifest is
// incomplete, gets a gap row and no read. A recovery whose coverage is lost
// mid-read also ends as a gap: a row masked by the globals alone is never
// written from a recovery.

// runOutputRecoverWait bounds one RecoverOutput, taken from the caller's
// deadline when that is sooner. A sandbox controls the size of its own log, so
// an unbounded re-read on the boot pass would delay /healthz and can crashloop
// the pod. On expiry the row is written incomplete, with what the ring holds.
const runOutputRecoverWait = 10 * time.Second

func (s *Server) outputRecoverWait() time.Duration {
	if s.runOutputRecoverWaitOverride > 0 {
		return s.runOutputRecoverWaitOverride
	}
	return runOutputRecoverWait
}

// recoverRunOutput is the synchronous half: it reads run's output back from the
// substrate into a fresh tail, which the finisher then persists, or writes the
// capture-gap row. reason is the gap's audited reason when there is nothing to
// recover from at all (no recoverer, no sandbox). A final row is never replaced.
func (s *Server) recoverRunOutput(ctx context.Context, st store.RunOutputStore, run types.AgentRun, reason string) {
	if !s.recoveryOwed(ctx, st, run.ID) {
		return
	}
	rec, ok := s.cfg.Runner.(runner.OutputRecoverer)
	switch {
	case !s.maskCovered(ctx, run.ID):
		s.writeGapRow(ctx, st, run.ID, "mask_uncovered")
		return
	case !ok || run.SandboxRef == "":
		s.writeGapRow(ctx, st, run.ID, reason)
		return
	}
	tw := s.openRecoveryTail(run)
	if tw == nil {
		// Erased, or coverage lost between the check and the open.
		s.writeGapRow(ctx, st, run.ID, "mask_uncovered")
		return
	}
	err := s.readSubstrateOutput(ctx, rec, run.SandboxRef, tw)
	s.noteRecoveryEnd(run.ID, tw.t, err)
	tw.EndDrain(recoveryDrainErr(err))
}

// recoveryOwed reports whether a recovery may run at all: no final row exists
// (a recovery would replace it) and the run was not erased.
func (s *Server) recoveryOwed(ctx context.Context, st store.RunOutputStore, runID uuid.UUID) bool {
	row, found, err := st.GetRunOutput(ctx, runID)
	switch {
	case errors.Is(err, store.ErrRunOutputErased):
		s.fenceRunOutput(runID)
		return false
	case err != nil:
		slog.WarnContext(ctx, "wardynd: could not read a run's output row before recovering it",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return false
	}
	return !found || row.CapturedAt == nil
}

// openRecoveryTail opens a fresh tail for run, marks it recovered and opens its
// drain, or returns nil when the run keeps none. A fresh ring is what makes a
// re-read replace the capture, never append to one.
func (s *Server) openRecoveryTail(run types.AgentRun) *tailWriter {
	w := s.openExecOutput(run, false)
	tw, _ := w.(*tailWriter)
	if tw == nil {
		return nil
	}
	tw.t.fmu.Lock()
	tw.t.recovered = true
	tw.t.fmu.Unlock()
	tw.BeginDrain()
	return tw
}

// readSubstrateOutput is one RecoverOutput, bounded by outputRecoverWait. It
// runs the call on its own goroutine so a runner that ignores ctx cannot hold
// the finaliser past the bound: whatever it writes afterwards reaches a sealed
// tail and is dropped.
func (s *Server) readSubstrateOutput(ctx context.Context, rec runner.OutputRecoverer, ref string, w *tailWriter) error {
	ctx, cancel := context.WithTimeout(ctx, s.outputRecoverWait())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- rec.RecoverOutput(ctx, ref, w) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// noteRecoveryEnd records a recovery that found nothing to read as a gap, which
// the finisher writes in place of the tail. Any other error leaves the tail, and
// its drain ends in that error, so the row is incomplete.
func (s *Server) noteRecoveryEnd(runID uuid.UUID, e *execOutputTail, err error) {
	reason := ""
	switch {
	case errors.Is(err, runner.ErrOutputUnrecoverable):
		reason = "unrecoverable"
	case errors.Is(err, runner.ErrSandboxGone):
		reason = "sandbox_gone"
	case err != nil:
		slog.Warn("wardynd: could not recover a run's output from the substrate; the row will be incomplete",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}
	if reason == "" {
		return
	}
	e.fmu.Lock()
	e.gapReason = reason
	e.fmu.Unlock()
}

// recoveryDrainErr is the error a recovery's drain ends with: none for a gap
// (the finisher resolves it as one), the read's own error otherwise.
func recoveryDrainErr(err error) error {
	if errors.Is(err, runner.ErrOutputUnrecoverable) || errors.Is(err, runner.ErrSandboxGone) {
		return nil
	}
	return err
}

// resumeRunOutput is a live adoption's half: the process adopting a running
// run, with no tail for it, re-reads the agent's log from its first byte and
// follows it until the agent exits, into a fresh tail that the run's
// finalisation then persists. A run that cannot be covered, or whose substrate
// keeps no log, gets nothing here and a capture gap when it ends.
func (s *Server) resumeRunOutput(ctx context.Context, runID uuid.UUID, ref string) {
	st := s.runOutputStore()
	if st == nil || s.cfg.ExecOutputTailOff || s.cfg.Store == nil || ref == "" || s.tailFor(runID) != nil {
		return
	}
	rec, ok := s.cfg.Runner.(runner.OutputRecoverer)
	if !ok {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || run.Interactive || runIsUnrecordable(run) || !s.recoveryOwed(ctx, st, runID) || !s.maskCovered(ctx, runID) {
		return
	}
	tw := s.openRecoveryTail(run)
	if tw == nil {
		return
	}
	s.goBackground(func() {
		err := rec.RecoverOutput(ctx, ref, tw)
		s.noteRecoveryEnd(runID, tw.t, err)
		tw.EndDrain(recoveryDrainErr(err))
	})
}
