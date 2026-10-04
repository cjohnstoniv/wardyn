// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The pane snapshot (out-o3): when Wardyn itself stops an interactive run
// gracefully, the last screen and scrollback of its tmux pane is kept as the
// run's output row (source "pane_snapshot"), taken after the credential
// revocations and before the sandbox goes. A kill, a failure and a reconcile
// never snapshot, and neither does the run that exists to print a credential
// (runIsUnrecordable): for it no exec is issued at all.
//
// The pane is sandbox-controlled text: the sandbox runs as the tmux server's
// user and can make the pane, or tmux itself, print anything. The snapshot is
// therefore bounded in time and bytes, masked as it is copied, and stored as
// bytes; a failure at any step means no row and teardown goes on.

const (
	// paneSnapshotTimeout bounds the whole capture: the exec, the copy and the
	// wait for the exit.
	paneSnapshotTimeout = 3 * time.Second
	// paneSnapshotSource is the run_outputs.source of a snapshot row.
	paneSnapshotSource = "pane_snapshot"
	// paneSnapshotAction is the audit action of one attempt.
	paneSnapshotAction = "run.output.snapshot"
)

// paneSnapshotArgv dumps the whole scrollback of the session both drivers name
// "wardyn" (runner.TmuxAttachSh) as plain text: -p prints, -J joins wrapped
// lines, -S - starts at the first line of history, and no -e keeps escape
// sequences out of the bytes.
var paneSnapshotArgv = []string{"tmux", "capture-pane", "-p", "-J", "-S", "-", "-t", "wardyn"}

func (s *Server) paneSnapshotBound() time.Duration {
	if s.paneSnapshotTimeoutOverride > 0 {
		return s.paneSnapshotTimeoutOverride
	}
	return paneSnapshotTimeout
}

// SnapshotRunPane is the graceful-stop half of the output contract, for a
// caller outside this package (cmd/wardynd's idle and max-age stops): it runs
// after the run's revocations and before StopSandbox, and does nothing for a
// run that keeps no snapshot.
func (s *Server) SnapshotRunPane(ctx context.Context, runID uuid.UUID) {
	s.prepareRunOutput(ctx, runID, true)
}

// snapshotRunPane takes one snapshot of an interactive run's pane. It never
// fails its caller: whatever goes wrong is audited, and no row is written.
func (s *Server) snapshotRunPane(ctx context.Context, st store.RunOutputStore, run types.AgentRun) {
	// Checked first: the sign-in run's pane holds the credential it exists to
	// print, so nothing is executed in it, not even to find out it is empty.
	if runIsUnrecordable(run) || run.SandboxRef == "" || s.cfg.Runner == nil {
		return
	}
	// One snapshot per run: a final row already there is left as it is.
	if row, found, err := st.GetRunOutput(ctx, run.ID); err == nil && found && row.CapturedAt != nil {
		return
	}
	n, reason := s.capturePane(ctx, st, run)
	outcome := "success"
	data := map[string]any{"bytes": n}
	if reason != "" {
		outcome, data["reason"] = "failure", reason
		slog.WarnContext(ctx, "wardynd: no pane snapshot was kept for a stopped run",
			slog.String("run_id", run.ID.String()), slog.String("reason", reason))
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", paneSnapshotAction,
		run.ID.String(), outcome, mustJSON(data)))
}

// capturePane does the capture and the write, returning the bytes kept, or the
// reason none were. Neither is ever pane content.
func (s *Server) capturePane(ctx context.Context, st store.RunOutputStore, run types.AgentRun) (kept int, reason string) {
	if s.cfg.MaskManifests != nil && !s.maskCovered(ctx, run.ID) {
		return 0, "mask_uncovered"
	}
	capCtx, cancel := context.WithTimeout(ctx, s.paneSnapshotBound())
	defer cancel()
	sess, err := s.cfg.Runner.ExecStream(capCtx, run.SandboxRef, runner.ExecSpec{Argv: paneSnapshotArgv})
	switch {
	case errors.Is(err, runner.ErrSandboxGone):
		return 0, "sandbox_gone"
	case err != nil, sess == nil, sess != nil && sess.Stdout == nil:
		return 0, "exec_failed"
	}
	defer func() {
		if sess.Close != nil {
			_ = sess.Close() // tears down only this exec, never the sandbox
		}
	}()
	// Streaming contract (runner.ExecSession): an undrained stderr blocks Wait
	// and Stdout, and the bound would then be the only thing that ends the capture.
	if sess.Stderr != nil {
		go func() { _, _ = io.Copy(io.Discard, sess.Stderr) }()
	}

	tail := newExecOutputTail(s.cfg.RunOutputTailBytes, s.cfg.Now)
	tail.sink = &tailSink{ring: &tail.ring} // one read of the pane: nothing is mirrored into run_output_chunks
	tail.mw = &liveMaskWriter{reg: s.cfg.MaskRegistry, runID: run.ID, dst: tail.sink, guard: s.maskGuard(run.ID)}
	type exit struct {
		code int
		err  error
	}
	done := make(chan exit, 1)
	go func() {
		if _, err := io.Copy(tail.mw, sess.Stdout); err != nil {
			done <- exit{err: err}
			return
		}
		if sess.Wait == nil {
			done <- exit{}
			return
		}
		code, werr := sess.Wait()
		done <- exit{code, werr}
	}()
	select {
	case x := <-done:
		if x.err != nil {
			return 0, "exec_failed"
		}
		if x.code != 0 {
			return 0, "exit_nonzero"
		}
	case <-capCtx.Done():
		return 0, "timeout"
	}

	out, truncated, dropped, uncovered := tail.seal(s.cfg.MaskManifests != nil && !s.maskCovered(ctx, run.ID))
	if uncovered {
		return 0, "mask_uncovered"
	}
	row := store.RunOutput{
		RunID: run.ID, Output: out, Truncated: truncated, Incomplete: dropped,
		Source: paneSnapshotSource, MaskScope: s.liveMaskScope(false),
	}
	switch err := st.SaveFinalRunOutput(ctx, row); {
	case errors.Is(err, store.ErrRunOutputErased):
		s.fenceRunOutput(run.ID)
		return 0, "erased"
	case err != nil:
		return 0, "persist_failed"
	}
	return len(out), ""
}
