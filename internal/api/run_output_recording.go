// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const recordingOutputSource = "recording"

// errRecordingOutputOverBudget is a source read that ended on its deadline
// rather than failing: the work stays pending for a later attempt.
var errRecordingOutputOverBudget = errors.New("recording output read outlived its budget")

func (s *Server) recordingOutputEnabled(ctx context.Context, run types.AgentRun) bool {
	if s.cfg.RecordingStore == nil || s.cfg.ExecOutputTailOff || s.runOutputStore() == nil ||
		run.Interactive || runIsUnrecordable(run) || s.runOutputExpiredByRetention(run) {
		return false
	}
	uncaptured, known := s.execOutputCapture(ctx)
	return known && uncaptured
}

// uploadedRecordingOutput runs only after the cast commit. The durable work
// record precedes the receipt; a crash between those stores remains an
// unacknowledged upload, repairable by an authorized missing/gap output read.
func (s *Server) uploadedRecordingOutput(ctx context.Context, runID uuid.UUID) error {
	if s.cfg.Store == nil || s.cfg.RecordingStore == nil || s.cfg.ExecOutputTailOff || s.runOutputStore() == nil {
		return nil
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || run.Interactive || runIsUnrecordable(run) || s.runOutputExpiredByRetention(run) {
		return err
	}
	// The runner never withholds a receipt: one that cannot be asked owes
	// nothing here, like one that captures. The cast is kept, and the run's end
	// or an authorized read asks the runner again.
	if uncaptured, _ := s.execOutputCapture(ctx); !uncaptured {
		return nil
	}
	st := s.runOutputStore()
	if err := s.queueRecordingOutput(ctx, st, run, true); err != nil {
		return err
	}
	if run.State.IsTerminal() {
		// The recording is already kept and its recovery owed. Failure here is
		// retried from that durable record and does not withdraw the receipt.
		s.logRecordingOutputError(ctx, run.ID, s.recoverRecordingOutput(ctx, st, run))
	}
	return nil
}

func (s *Server) queueRecordingOutput(ctx context.Context, st store.RunOutputStore, run types.AgentRun, newRecording bool) error {
	err := st.QueueRecordingRunOutput(ctx, run.ID, s.cfg.RunOutputRetention, runOutputPendingStale, newRecording)
	if errors.Is(err, store.ErrRunOutputErased) || errors.Is(err, store.ErrRecordingOutputErased) {
		return nil // the selected output scope owes no new copy of this recording
	}
	return err
}

func (s *Server) finalizeRecordingOutput(ctx context.Context, st store.RunOutputStore, run types.AgentRun) {
	err := s.queueRecordingOutput(ctx, st, run, false)
	if err == nil {
		err = s.recoverRecordingOutput(ctx, st, run)
	}
	s.logRecordingOutputError(ctx, run.ID, err)
}

func (s *Server) logRecordingOutputError(ctx context.Context, runID uuid.UUID, err error) {
	if err != nil && !errors.Is(err, store.ErrRunOutputErased) && !errors.Is(err, store.ErrRecordingOutputErased) {
		slog.WarnContext(ctx, "wardynd: recording output recovery remains pending", slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}

func (s *Server) recoverRecordingOutput(ctx context.Context, st store.RunOutputStore, run types.AgentRun) error {
	// One newer upload may have invalidated the first read. Further work stays
	// durable for the elected sweep, rather than looping behind a busy uploader.
	for attempt := 0; attempt < 2; attempt++ {
		claim, ok, err := st.ClaimRecordingRunOutput(ctx, run.ID, s.cfg.RunOutputRetention, runOutputPendingStale)
		if err != nil || !ok {
			return err
		}
		row, reason := s.readRecordingOutput(ctx, run.ID)
		switch reason {
		case "recording_over_budget":
			return errRecordingOutputOverBudget
		case "recording_unavailable":
			return errors.New("recording output source is temporarily unavailable")
		case "recording_erased":
			_, err := st.EraseRecordingRunOutputs(ctx, []uuid.UUID{run.ID})
			return err
		}
		wrote, err := st.SaveRecordingRunOutput(ctx, claim, row, s.cfg.RunOutputRetention)
		if errors.Is(err, store.ErrRecordingOutputUncovered) {
			reason = "mask_uncovered"
			clear(row.Output)
			row = recordingOutputGap(run.ID)
			wrote, err = st.SaveRecordingRunOutput(ctx, claim, row, s.cfg.RunOutputRetention)
		}
		if err != nil {
			return err
		}
		if wrote {
			s.auditRecordingOutput(ctx, row, reason)
			return nil
		}
	}
	return nil
}

// auditRecordingOutput is the recording path's run.output.finalize row. Output
// that was recovered is a success, and its incomplete flag says it can never be
// vouched whole; only a gap is a failed capture.
func (s *Server) auditRecordingOutput(ctx context.Context, row store.RunOutput, reason string) {
	outcome := "success"
	if row.CaptureGap {
		outcome = "failure"
	}
	s.recordAudit(ctx, s.auditEvent(&row.RunID, types.ActorSystem, "wardynd", "run.output.finalize", row.RunID.String(), outcome,
		mustJSON(map[string]any{"source": row.Source, "incomplete": row.Incomplete, "capture_gap": row.CaptureGap, "reason": reason})))
}

func recordingOutputGap(runID uuid.UUID) store.RunOutput {
	return store.RunOutput{RunID: runID, Source: recordingOutputSource, Incomplete: true, CaptureGap: true}
}

func (s *Server) readRecordingOutput(ctx context.Context, runID uuid.UUID) (store.RunOutput, string) {
	gap := recordingOutputGap(runID)
	if !s.maskCovered(ctx, runID) {
		return gap, "mask_uncovered"
	}
	tail := newExecOutputTail(s.cfg.RunOutputTailBytes, s.cfg.Now)
	tail.sink = &tailSink{ring: &tail.ring}
	tail.mw = &liveMaskWriter{reg: s.cfg.MaskRegistry, runID: runID, dst: tail.sink, guard: s.maskGuard(runID)}
	readCtx, cancel := context.WithTimeout(ctx, s.outputRecoverWait())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		rc, err := recording.OpenJoined(readCtx, s.cfg.RecordingStore, runID.String())
		if err == nil {
			// Each batch still refreshes after its bytes arrive; per-event
			// freshness would cap a healthy recovery at 20 events per second.
			batch := bufio.NewWriterSize(tail.mw, maskPipeMax)
			err = recording.CopyOutput(readCtx, batch, rc)
			if err == nil {
				err = readCtx.Err()
			}
			if err == nil {
				err = batch.Flush()
			}
			if err == nil {
				err = readCtx.Err()
			}
			_ = rc.Close()
		}
		done <- err
	}()
	var readErr error
	select {
	case readErr = <-done:
	case <-readCtx.Done():
		readErr = readCtx.Err()
	}
	// A read that ran out of time keeps no bytes, so it asks no closing coverage
	// question: in a sweep pass the caller's own deadline is what ended it, and
	// a coverage read on that context could only answer "uncovered".
	overBudget := readErr != nil && errors.Is(readCtx.Err(), context.DeadlineExceeded)
	out, truncated, _, uncovered := tail.seal(!overBudget && !s.maskCovered(ctx, runID), true)
	s.releaseRunOutput(runID, tail)
	if errors.Is(readErr, recording.ErrErased) {
		clear(out)
		return gap, "recording_erased"
	}
	if uncovered {
		clear(out)
		return gap, "mask_uncovered"
	}
	if readErr != nil {
		clear(out)
		switch {
		case errors.Is(readErr, recording.ErrNotFound):
			return gap, "recording_missing"
		case errors.Is(readErr, recording.ErrInvalidCast):
			return gap, "recording_invalid"
		case overBudget:
			return gap, "recording_over_budget"
		}
		return gap, "recording_unavailable"
	}
	return store.RunOutput{
		RunID: runID, Output: out, Truncated: truncated, Source: recordingOutputSource,
		Incomplete: true, MaskScope: s.liveMaskScope(false),
	}, ""
}

// repairRecordingOutput is reachable only after recordingReader. A cooldown
// bounds rereads of unchanged gaps, including a lost upload receipt on restart.
func (s *Server) repairRecordingOutput(ctx context.Context, run types.AgentRun, row store.RunOutput, found bool) (store.RunOutput, bool, error) {
	if !run.State.IsTerminal() || found && row.CapturedAt != nil && !row.CaptureGap || !s.recordingOutputEnabled(ctx, run) {
		return row, found, nil
	}
	st := s.runOutputStore()
	if err := s.queueRecordingOutput(ctx, st, run, false); err != nil {
		return row, found, err
	}
	s.logRecordingOutputError(ctx, run.ID, s.recoverRecordingOutput(ctx, st, run))
	return s.readStoredRunOutput(ctx, run.ID)
}

func (s *Server) sweepRecordingOutputs(ctx context.Context, st store.RunOutputStore) error {
	if s.cfg.RecordingStore == nil || s.cfg.ExecOutputTailOff {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ids, err := st.ListPendingRecordingRunOutputs(ctx, runOutputPendingStale, 200)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil // the remaining page is still durable for the next pass
		}
		run, err := s.cfg.Store.GetRun(ctx, id)
		if err != nil {
			return err
		}
		if run.Interactive || runIsUnrecordable(run) {
			continue
		}
		err = s.recoverRecordingOutput(ctx, st, run)
		switch {
		case errors.Is(err, errRecordingOutputOverBudget):
			// That run's outcome, not the pass's: a sandbox sizes its own recording,
			// and one slow read must not turn the whole sweep's health stale.
			s.logRecordingOutputError(ctx, run.ID, err)
		case err != nil && !errors.Is(err, store.ErrRunOutputErased) && !errors.Is(err, store.ErrRecordingOutputErased):
			return err
		}
	}
	return nil
}
