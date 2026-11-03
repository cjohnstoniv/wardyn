// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A macOS VM task run's agent output, recorded by the organisation. Its runner keeps no recording of its
// own, so the agent's output, which the runner already sends into the run's output tail
// (SandboxSpec.ExecOutput), is masked into a cast that rides on that tail (tailWriter.cast). The cast is
// saved under <run>~agent when the copy ends, with a session.recording.write row, as a terminal
// session's is, and masked against the run's registry the same way (liveMaskWriter).
//
// Whether such a run counts as recorded is decided once, at dispatch, from the tail it actually got
// (recordMacOSAgent): with its cast it is recorded; with none it is marked runner_declined and follows
// the organisation's required rule, as any target that cannot record does.
//
// The cast lives in this process's memory until the copy ends. A process that then finalises or adopts
// the run re-reads the runner's copy into a fresh tail (openRecoveryTail) and records that too, as
// <run>~agent-recovered, so the two never replace each other. Where a re-read gives nothing, or a cast's
// save fails, the run is marked runner_declined with a session.recording.write failure row saying why
// (declineAgentOutput); a running run is never stopped for it. A cast of either session saved later
// clears the mark.

const (
	agentCastSession          = "agent"
	agentCastRecoveredSession = "agent-recovered"
)

// macOSAgentOutputRequiredMsg is the sentence a macOS VM task run fails at start with when recording is
// required and its agent output cast did not open.
const macOSAgentOutputRequiredMsg = "Recording is required, and this macOS VM run's agent output can't be recorded on this deployment, so the run didn't start."

// startRunRecording is dispatch's recording step: the run's level and mark (applyDispatchRecording), its
// output tail (openExecOutput), then a macOS VM task run's agent output cast and the decision it settles
// (recordMacOSAgent), in that order. false: the run failed, with its audit row.
func (s *Server) startRunRecording(ctx context.Context, run *types.AgentRun, spec *runner.SandboxSpec, policy types.RunPolicySpec,
	c dispatchCeiling, site types.SiteConfig, siteErr error, interactive bool,
) bool {
	if !s.applyDispatchRecording(ctx, run, spec, policy, c, site, siteErr) {
		return false
	}
	spec.ExecOutput = s.openExecOutput(*run, interactive)
	return s.recordMacOSAgent(ctx, run, spec, interactive)
}

// recordMacOSAgent settles whether a macOS VM task run marked recorded is: it is when its output tail is
// open and its agent output cast attached to it. Otherwise (no tail, by any of openExecOutput's exits, or
// no cast) it is marked runner_declined, which fails it under a requirement with
// macOSAgentOutputRequiredMsg and is stored otherwise (keepOrHoldNotRecorded). Any other run, an
// interactive one (it has no agent exec, and every terminal byte passes an organisation-recorded
// session), and a run already marked are left as they are.
func (s *Server) recordMacOSAgent(ctx context.Context, run *types.AgentRun, spec *runner.SandboxSpec, interactive bool) bool {
	if !run.MachineKind.IsMacOSVM() || interactive || run.NotRecorded != "" {
		return true
	}
	if tw, ok := spec.ExecOutput.(*tailWriter); ok && s.attachAgentCast(*run, tw, agentCastSession) {
		return true
	}
	run.NotRecorded = types.NotRecordedRunnerDeclined
	return s.keepOrHoldNotRecorded(ctx, run, macOSAgentOutputRequiredMsg)
}

// agentCastOwed is whether the organisation records run's agent output: a macOS VM task run above off,
// marked recorded or runner_declined (which a cast saved later clears), that does not exist to print a
// credential (runIsUnrecordable), on a deployment that keeps recordings.
func (s *Server) agentCastOwed(run types.AgentRun) bool {
	return s.cfg.RecordingStore != nil && run.MachineKind.IsMacOSVM() && !run.Interactive && !runIsUnrecordable(run) &&
		run.RecordingLevel != types.RecordingOff && (run.NotRecorded == "" || run.NotRecorded == types.NotRecordedRunnerDeclined)
}

// attachAgentCast attaches run's agent output cast, named session, to tw, and reports whether it did
// (agentCastOwed). It is called before tw is handed to a runner.
func (s *Server) attachAgentCast(run types.AgentRun, tw *tailWriter, session string) bool {
	if tw == nil || !s.agentCastOwed(run) {
		return false
	}
	buf := &capBuffer{}
	cast := recording.NewCastWriter(buf, 0, 0, s.cfg.Now().UTC())
	mw := &liveMaskWriter{reg: s.cfg.MaskRegistry, runID: run.ID, dst: cast, guard: s.maskGuard(run.ID)}
	tw.cast = &agentCast{s: s, run: run, session: session, mw: mw, pipe: newMaskPipe(mw), cast: cast, buf: buf}
	return true
}

// agentCast is one agent output cast: the masking pipe the tail's bytes are teed into, the cast it
// writes, held to maxSessionCastBytes (capBuffer keeps the head and says truncated), and what its row
// says about it.
type agentCast struct {
	s       *Server
	run     types.AgentRun
	session string
	mw      *liveMaskWriter
	pipe    *maskPipe
	cast    *recording.CastWriter
	buf     *capBuffer
	// gapBytes is the head a recovered copy starts after (tailWriter.OutputGap), for the row.
	gapBytes atomic.Int64

	mu       sync.Mutex
	declined string // why a re-read found nothing to record (decline); finish then saves nothing
	once     sync.Once
}

func (c *agentCast) write(p []byte) { _, _ = c.pipe.Write(p) }

// decline is a re-read that ended with nothing to read (noteRecoveryEnd's reason): the copy's bytes are
// not the run's output, so finish saves no cast and leaves the run unrecorded with reason.
func (c *agentCast) decline(reason string) {
	c.mu.Lock()
	c.declined = reason
	c.mu.Unlock()
}

// finish saves the cast once, after its copy ended (tailWriter.EndDrain). A cast with no output is not
// saved and the run stays recorded: an agent that printed nothing left nothing to record. A save that
// fails marks the run runner_declined on its failure row; one that succeeds clears that mark.
func (c *agentCast) finish() {
	c.once.Do(c.save)
}

func (c *agentCast) save() {
	ctx := context.Background()
	c.pipe.flush()
	c.mw.mu.Lock()
	c.mw.flushLocked()
	had := c.cast.HadOutput()
	snap := append([]byte(nil), c.buf.buf.Bytes()...)
	truncated := c.buf.truncated
	c.mw.mu.Unlock()
	c.mu.Lock()
	declined := c.declined
	c.mu.Unlock()
	s, runID := c.s, c.run.ID
	if declined != "" {
		s.declineAgentOutput(ctx, c.run, declined)
		return
	}
	if !had {
		return
	}
	key := recording.CastKey(runID.String(), c.session)
	saveCtx, cancel := context.WithTimeout(ctx, sessionRecordingSaveWait)
	err := s.cfg.RecordingStore.SaveCastNamed(saveCtx, runID.String(), c.session, bytes.NewReader(snap))
	cancel()
	data := map[string]any{"session": c.session, "key": key, "bytes": len(snap)}
	if truncated {
		data["truncated"], data["limit_bytes"] = true, maxSessionCastBytes
	}
	if gap := c.gapBytes.Load(); gap > 0 {
		data["gap_bytes"] = gap
	}
	if c.session == agentCastRecoveredSession {
		data["recovered"] = true
	}
	outcome := "success"
	if err != nil {
		outcome, data["error"] = "failure", err.Error()
		run := c.run
		run.NotRecorded = types.NotRecordedRunnerDeclined
		s.keepRunNotRecorded(ctx, run)
	} else {
		s.clearAgentOutputDeclined(ctx, runID)
	}
	s.recordStreamAudit(ctx, c.run.SandboxRef, s.auditEvent(&runID, types.ActorSystem, "wardynd", "session.recording.write",
		key, outcome, mustJSON(data)))
}

// declineAgentOutput is a macOS VM task run marked recorded whose agent output a re-read could not
// record, reason saying what it found: the run is marked runner_declined, and a session.recording.write
// failure row for the agent session says why. The run is not stopped, under a requirement too: it was
// admitted recorded, and a recording lost to a restart is a gap on every kind, not a kill.
func (s *Server) declineAgentOutput(ctx context.Context, run types.AgentRun, reason string) {
	if run.NotRecorded != "" || !s.agentCastOwed(run) {
		return
	}
	run.NotRecorded = types.NotRecordedRunnerDeclined
	s.keepRunNotRecorded(ctx, run)
	key := recording.CastKey(run.ID.String(), agentCastSession)
	s.recordStreamAudit(ctx, run.SandboxRef, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "session.recording.write",
		key, "failure", mustJSON(map[string]any{"session": agentCastSession, "key": key, "reason": reason})))
}

// clearAgentOutputDeclined clears not_recorded: runner_declined once an agent output cast for the run is
// stored: the run now has the recording the mark said it lacked. Best-effort, like the mark.
func (s *Server) clearAgentOutputDeclined(ctx context.Context, runID uuid.UUID) {
	st, ok := s.cfg.Store.(interface {
		ClearRunDeclined(context.Context, uuid.UUID) (bool, error)
	})
	if !ok {
		return
	}
	if _, err := st.ClearRunDeclined(ctx, runID); err != nil {
		slog.WarnContext(ctx, "wardynd: clear a run's runner_declined after its agent output was recorded", slog.String("run_id", runID.String()), slog.Any("err", err))
	}
}
