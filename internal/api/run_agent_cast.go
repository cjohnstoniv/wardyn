// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A macOS VM task run's agent output, recorded by the organisation. Its runner keeps no recording of its
// own, so the agent's output, which the runner already sends into the run's output tail
// (SandboxSpec.ExecOutput), is masked into a cast that rides on that tail (tailWriter.cast). The cast is
// saved under <run>~agent when the copy ends, with a session.recording.write row, as a terminal
// session's is, and masked against the run's registry the same way (liveMaskWriter).
//
// Whether such a run counts as recorded is decided once, at dispatch, from the tail it actually got
// (recordMacOSAgent): with its cast, and the cast's obligation stored on the run
// (agent_runs.agent_cast_owed_at), it is recorded; otherwise it is marked runner_declined and follows the
// organisation's required rule, as any target that cannot record does.
//
// The obligation, not the run's output row, is what ends honestly. A stored cast of the run clears it
// (and any runner_declined mark). Where none can be stored, the run is marked runner_declined with a
// session.recording.write failure row saying why, then the obligation is cleared (declineAgentOutput); a
// running run is never stopped for it. A process that finalises or adopts the run re-reads the runner's
// copy and records that as <run>~agent-recovered, so the two never replace each other; and the output
// sweeper settles every terminal run still owing a cast (sweepAgentCasts), so a process that dies before
// its save never leaves a run marked recorded with no cast. A store error leaves the obligation for the
// next sweep.

const (
	agentCastSession          = "agent"
	agentCastRecoveredSession = "agent-recovered"
)

// macOSAgentOutputRequiredMsg is the sentence a macOS VM task run fails at start with when recording is
// required and its agent output cast did not open.
const macOSAgentOutputRequiredMsg = "Recording is required, and this macOS VM run's agent output can't be recorded on this deployment, so the run didn't start."

// macOSAgentDeclineUnrecordedMsg is the sentence a macOS VM task run fails at start with when its agent output
// cast was declined and the audit recorder would not take the row that says so.
const macOSAgentDeclineUnrecordedMsg = "Wardyn could not record why this run's agent output isn't recorded, so the run didn't start. Try again."

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
// open, its agent output cast attached to it and the cast's obligation stored. Otherwise (no tail, by any
// of openExecOutput's exits, no cast, or an obligation that could not be stored) it is marked
// runner_declined, which fails it under a requirement with macOSAgentOutputRequiredMsg and is stored
// otherwise (keepOrHoldNotRecorded). Any other run, an
// interactive one (it has no agent exec, and every terminal byte passes an organisation-recorded
// session), and a run already marked are left as they are.
func (s *Server) recordMacOSAgent(ctx context.Context, run *types.AgentRun, spec *runner.SandboxSpec, interactive bool) bool {
	if !run.MachineKind.IsMacOSVM() || interactive || run.NotRecorded != "" {
		return true
	}
	// The reason a declined run's failure row gives (keepRunNotRecorded): no cast opened on its tail, or its
	// obligation could not be stored.
	declined := "no_tail"
	if tw, ok := spec.ExecOutput.(*tailWriter); ok && s.attachAgentCast(*run, tw, agentCastSession) {
		if s.oweAgentCast(ctx, run.ID) {
			return true
		}
		tw.cast = nil
		declined = "save_failed"
	}
	run.NotRecorded = types.NotRecordedRunnerDeclined
	return s.keepOrHoldNotRecorded(ctx, run, macOSAgentOutputRequiredMsg, declined)
}

// agentOutputRecorded is whether the organisation records run's agent output: a macOS VM task run above
// off, marked recorded or runner_declined (which a cast saved later clears), that does not exist to print
// a credential (runIsUnrecordable), on a deployment that keeps recordings.
func (s *Server) agentOutputRecorded(run types.AgentRun) bool {
	return s.cfg.RecordingStore != nil && run.MachineKind.IsMacOSVM() && !run.Interactive && !runIsUnrecordable(run) &&
		run.RecordingLevel != types.RecordingOff && (run.NotRecorded == "" || run.NotRecorded == types.NotRecordedRunnerDeclined)
}

// attachAgentCast attaches run's agent output cast, named session, to tw, and reports whether it did
// (agentOutputRecorded). It is called before tw is handed to a runner.
func (s *Server) attachAgentCast(run types.AgentRun, tw *tailWriter, session string) bool {
	if tw == nil || !s.agentOutputRecorded(run) {
		return false
	}
	tw.cast = s.newAgentCast(run, session)
	return true
}

func (s *Server) newAgentCast(run types.AgentRun, session string) *agentCast {
	buf := &capBuffer{}
	cast := recording.NewCastWriter(buf, 0, 0, s.cfg.Now().UTC())
	mw := &liveMaskWriter{reg: s.cfg.MaskRegistry, runID: run.ID, dst: cast, guard: s.maskGuard(run.ID)}
	return &agentCast{s: s, run: run, session: session, mw: mw, pipe: newMaskPipe(mw), cast: cast, buf: buf}
}

// oweAgentCast stores run's agent cast obligation, and reports whether it did: a run whose obligation
// cannot be stored cannot promise its recording.
func (s *Server) oweAgentCast(ctx context.Context, runID uuid.UUID) bool {
	st, ok := s.cfg.Store.(store.RunAgentCastStore)
	if !ok {
		return false
	}
	if err := st.SetRunAgentCastOwed(ctx, runID); err != nil {
		slog.ErrorContext(ctx, "wardynd: store a run's agent output recording obligation; the run is not recorded",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return false
	}
	return true
}

// unoweAgentCast clears the agent output obligation of a macOS VM run whose sandbox was never created:
// it never ran, so it owes no recording, and gets neither a runner_declined mark nor a failure row. A
// failed clear is logged and left to the sweep.
func (s *Server) unoweAgentCast(ctx context.Context, run types.AgentRun) {
	st, ok := s.cfg.Store.(store.RunAgentCastStore)
	if !ok || !run.MachineKind.IsMacOSVM() {
		return
	}
	_ = agentCastStoreErr(ctx, run.ID, "clear the agent output obligation of a run that never started", st.ClearRunAgentCastOwed(ctx, run.ID, false))
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
	declined string // why a re-read found nothing to record (decline); seal then saves nothing
	readErr  error  // how a re-read failed with no gap's reason (endRead); see take
	once     sync.Once
}

func (c *agentCast) write(p []byte) { _, _ = c.pipe.Write(p) }

// decline is a re-read that ended with nothing to read (noteRecoveryEnd's reason): the copy's bytes are
// not the run's output, so seal saves no cast and leaves the run unrecorded with reason.
func (c *agentCast) decline(reason string) {
	c.mu.Lock()
	c.declined = reason
	c.mu.Unlock()
}

// endRead notes how a re-read into the cast ended: a gap's reason (recoveryGapReason) declines it; any
// other error (a timeout, a transport error) fails the read, which take tells apart from a clean end.
func (c *agentCast) endRead(reason string, err error) {
	switch {
	case reason != "":
		c.decline(reason)
	case err != nil:
		c.mu.Lock()
		c.readErr = err
		c.mu.Unlock()
	}
}

// errAgentCastRowRejected is a decline's failure row, or a stored cast's success row, that the audit
// recorder did not accept: the obligation stays, and the next sweep writes the row again.
var errAgentCastRowRejected = errors.New("the audit recorder did not accept the agent output's failure row")

// errAgentCastUnread is a re-read that failed before its first byte: the run's obligation stays for the
// next sweep, which gives up at agentCastReadGiveUp.
var errAgentCastUnread = errors.New("the runner's copy of the agent output could not be read")

// agentCastCutMarker ends a cast whose re-read failed part way, so it never reads as complete.
const agentCastCutMarker = "\n[Wardyn: the rest of this run's output is missing. The runner's copy could not be read to its end.]\n"

// seal ends the cast once, after its copy ended (tailWriter.EndDrain), and returns what settles it, for
// the caller to run once the tail's drain is released; nil when another call sealed it. A cast with no
// output from a copy that ended cleanly is not saved and its obligation is cleared with the run recorded:
// an agent that printed nothing left nothing to record (take). The obligation is durable, so nothing
// need be written before the release.
func (c *agentCast) seal() func() error {
	var end func() error
	c.once.Do(func() { end = c.take() })
	return end
}

// take flushes and copies what the cast holds, and returns what settles it. A copy with no output that
// ended cleanly settles the obligation (nothing to record); one whose read failed keeps it
// (errAgentCastUnread). A copy cut short by a failed read is saved with agentCastCutMarker at its end and
// incomplete on its row. Bytes the masker dropped or replaced because it could not vouch for them (its
// guard failed, or the registry was stale: capture.dropped) are never silence and never complete: with
// nothing kept the run is declined, mask_gap; with some kept the cast is saved incomplete, mask_gap.
func (c *agentCast) take() func() error {
	ctx := context.Background()
	c.mu.Lock()
	declined, readErr := c.declined, c.readErr
	c.mu.Unlock()
	had, maskGap := c.flush()
	switch {
	case declined != "":
		return func() error { return c.s.declineAgentOutput(ctx, c.run, declined) }
	case !had && readErr != nil:
		// Before mask_gap: the sweep's re-read masks afresh and may keep what this copy dropped, and its
		// give-up (agentCastReadGiveUp) still bounds it.
		return func() error {
			return agentCastStoreErr(ctx, c.run.ID, "re-read a run's agent output", fmt.Errorf("%w: %w", errAgentCastUnread, readErr))
		}
	case !had && maskGap:
		return func() error { return c.s.declineAgentOutput(ctx, c.run, "mask_gap") }
	case !had:
		return func() error { return c.s.settleAgentCast(ctx, c.run.ID) }
	case readErr != nil:
		c.write([]byte(agentCastCutMarker))
		c.flush()
	}
	c.mw.mu.Lock()
	snap := append([]byte(nil), c.buf.buf.Bytes()...)
	truncated := c.buf.truncated
	c.mw.mu.Unlock()
	return func() error { return c.save(ctx, snap, truncated, readErr, maskGap) }
}

// flush waits for every byte written to reach the cast, and reports whether any did and whether the
// masker dropped or replaced any it could not vouch for.
func (c *agentCast) flush() (had, maskGap bool) {
	c.pipe.flush()
	c.mw.mu.Lock()
	defer c.mw.mu.Unlock()
	c.mw.flushLocked()
	return c.cast.HadOutput(), c.mw.capture.dropped
}

// save stores the cast with its session.recording.write row: a success, once the row is durably
// accepted, clears the run's obligation and any runner_declined mark; a failure leaves the run
// unrecorded (declineAgentOutput, save_failed). A copy cut short by readErr says so on its row:
// incomplete, read_error (the error itself only in the log); one with bytes the masker dropped says
// incomplete and mask_gap. The same marks go in the cast's header (stampCastFlags).
func (c *agentCast) save(ctx context.Context, snap []byte, truncated bool, readErr error, maskGap bool) error {
	s, runID := c.s, c.run.ID
	key := recording.CastKey(runID.String(), c.session)
	flags := map[string]any{}
	if truncated {
		flags["truncated"], flags["limit_bytes"] = true, maxSessionCastBytes
	}
	if gap := c.gapBytes.Load(); gap > 0 {
		flags["gap_bytes"] = gap
	}
	if readErr != nil {
		// The error's text goes only to the log: the cast and its row are kept, and it is not masked.
		flags["incomplete"], flags["read_error"] = true, true
		slog.WarnContext(ctx, "wardynd: a run's agent output copy ended in an error; its cast is saved incomplete",
			slog.String("run_id", runID.String()), slog.String("session", c.session), slog.Any("err", readErr))
	}
	if maskGap {
		flags["incomplete"], flags["mask_gap"] = true, true
	}
	snap = stampCastFlags(snap, flags)
	saveCtx, cancel := context.WithTimeout(ctx, sessionRecordingSaveWait)
	err := s.cfg.RecordingStore.SaveCastNamed(saveCtx, runID.String(), c.session, bytes.NewReader(snap))
	cancel()
	if errors.Is(err, recording.ErrErased) {
		return s.dropErasedAgentCast(ctx, runID)
	}
	data := map[string]any{"session": c.session, "key": key, "bytes": len(snap)}
	maps.Copy(data, flags)
	if c.session == agentCastRecoveredSession {
		data["recovered"] = true
	}
	outcome := "success"
	if err != nil {
		outcome, data["error"] = "failure", err.Error()
	}
	accepted := s.recordStreamAuditDurably(ctx, c.run.SandboxRef, s.auditEvent(&runID, types.ActorSystem, "wardynd",
		"session.recording.write", key, outcome, mustJSON(data)))
	switch {
	case err != nil:
		return s.declineAgentOutput(ctx, c.run, "save_failed")
	case !accepted:
		// The run page finds a cast by its row: the obligation stays, and the sweep writes the row from
		// the stored cast (settleStoredAgentCasts).
		return agentCastStoreErr(ctx, runID, "record a run's stored agent output cast", errAgentCastRowRejected)
	}
	return s.settleAgentCast(ctx, runID)
}

// declineAgentOutput is a macOS VM task run that still owes its agent output cast and will get none,
// reason saying why: the run is marked runner_declined, a session.recording.write failure row for the
// agent session says why, then the obligation is cleared, in that order, so an error at any step leaves
// the obligation for the next sweep. The row must be durably accepted before the clear: a rejected row
// keeps the obligation, and the next sweep writes it again, so a row is never lost; a crash between the
// row and the clear leaves a second row, which is the price of that. A run that owes no cast (it has
// one, or was settled) is left as it is. The run is not stopped, under a requirement too: it was
// admitted recorded, and a recording lost to a restart is a gap on every kind, not a kill.
func (s *Server) declineAgentOutput(ctx context.Context, run types.AgentRun, reason string) error {
	st, ok := s.cfg.Store.(store.RunAgentCastStore)
	if !ok || !s.agentOutputRecorded(run) {
		return nil
	}
	owed, err := st.DeclineRunAgentCast(ctx, run.ID)
	if err != nil || !owed {
		return agentCastStoreErr(ctx, run.ID, "mark a run not recorded for want of its agent output", err)
	}
	key := recording.CastKey(run.ID.String(), agentCastSession)
	if !s.recordStreamAuditDurably(ctx, run.SandboxRef, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "session.recording.write",
		key, "failure", mustJSON(map[string]any{"session": agentCastSession, "key": key, "reason": reason}))) {
		return agentCastStoreErr(ctx, run.ID, "record why a run's agent output is not recorded", errAgentCastRowRejected)
	}
	return agentCastStoreErr(ctx, run.ID, "clear a run's agent output obligation", st.ClearRunAgentCastOwed(ctx, run.ID, false))
}

// settleAgentCast clears a run's agent output obligation with the run recorded: a cast of it is stored,
// or there was nothing to record. Any runner_declined mark goes with it.
func (s *Server) settleAgentCast(ctx context.Context, runID uuid.UUID) error {
	st, ok := s.cfg.Store.(store.RunAgentCastStore)
	if !ok {
		return nil
	}
	return agentCastStoreErr(ctx, runID, "clear a run's agent output obligation after its cast", st.ClearRunAgentCastOwed(ctx, runID, true))
}

// dropErasedAgentCast clears the obligation of a run whose recordings were erased (the recording
// store's fence, recording.ErrErased): nothing can be saved or re-read for it, so it owes nothing. No
// row and no mark are written, and the fence stays. Without this the sweep would meet the fence on
// every pass, and runs erased in numbers would hold the oldest places in its list.
func (s *Server) dropErasedAgentCast(ctx context.Context, runID uuid.UUID) error {
	st, ok := s.cfg.Store.(store.RunAgentCastStore)
	if !ok {
		return nil
	}
	return agentCastStoreErr(ctx, runID, "clear an erased run's agent output obligation", st.ClearRunAgentCastOwed(ctx, runID, false))
}

// agentCastStoreErr logs a failed obligation write, which the next sweep retries, and returns it.
func agentCastStoreErr(ctx context.Context, runID uuid.UUID, what string, err error) error {
	if err != nil {
		slog.WarnContext(ctx, "wardynd: "+what+"; the next sweep retries", slog.String("run_id", runID.String()), slog.Any("err", err))
	}
	return err
}

// agentCastReadGiveUp is how long after a run ended (its ended_at, else its last update) the sweep keeps
// retrying a re-read that fails before its first byte: about 25 sweeps (runOutputSweepInterval, one
// minute, after agentCastSweepAge), each bounded by outputRecoverWait. Past it the run is marked
// runner_declined, reason read_failed.
const agentCastReadGiveUp = 30 * time.Minute

// agentCastSweepAge is how long a terminal run's state must have stood before the sweep settles its
// obligation: longer than a cast's save may take (sessionRecordingSaveWait), so a save in flight is
// never raced.
const agentCastSweepAge = runOutputPendingStale

// agentCastSweepPage is how many owed runs one sweep takes.
const agentCastSweepPage = 200

// sweepAgentCasts settles every terminal run that still owes its agent output cast, whatever its output
// row says: a cast already stored gets its row and settles it (settleStoredAgentCasts); else the
// runner's copy is re-read into an agent-recovered cast; else the run is marked runner_declined with the
// failure row. A run this process still holds a tail for is left to that tail. One page per pass, from where the last pass stopped (the page wraps to the oldest after a short one). The first error is returned; the obligation stays for the next sweep.
func (s *Server) sweepAgentCasts(ctx context.Context) error {
	st, ok := s.cfg.Store.(store.RunAgentCastStore)
	if !ok || s.cfg.RecordingStore == nil {
		return nil
	}
	after := s.execOutputs.sweep.get(&s.execOutputs.sweep.agentCast)
	ids, next, err := st.ListRunsAgentCastOwed(ctx, agentCastSweepAge, after, agentCastSweepPage)
	if err != nil {
		return err // the position is kept: the next sweep reads the same page
	}
	var firstErr error
	for _, id := range ids {
		if s.tailFor(id) != nil {
			continue
		}
		if err := s.sweepAgentCast(ctx, st, id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if ctx.Err() == nil {
		s.execOutputs.sweep.set(&s.execOutputs.sweep.agentCast, next)
	}
	return firstErr
}

func (s *Server) sweepAgentCast(ctx context.Context, st store.RunAgentCastStore, id uuid.UUID) error {
	run, err := s.cfg.Store.GetRun(ctx, id)
	if err != nil {
		return err
	}
	stored, err := s.storedAgentCasts(ctx, id)
	switch {
	case errors.Is(err, recording.ErrErased):
		return s.dropErasedAgentCast(ctx, id)
	case err != nil:
		return err
	case len(stored) > 0:
		return s.settleStoredAgentCasts(ctx, st, run, stored)
	case !s.agentOutputRecorded(run):
		return st.ClearRunAgentCastOwed(ctx, id, false)
	}
	rec, ok := s.cfg.Runner.(runner.OutputRecoverer)
	switch {
	case !s.maskCovered(ctx, id):
		return s.declineAgentOutput(ctx, run, "mask_uncovered")
	case !ok || run.SandboxRef == "":
		return s.declineAgentOutput(ctx, run, "no_tail")
	}
	c := s.newAgentCast(run, agentCastRecoveredSession)
	readErr := s.readSubstrateOutput(ctx, rec, run.SandboxRef, castCopy{c})
	c.endRead(recoveryGapReason(readErr), readErr)
	err = c.seal()()
	if errors.Is(err, errAgentCastUnread) && s.cfg.Now().Sub(runEnded(run)) >= agentCastReadGiveUp {
		return s.declineAgentOutput(ctx, run, "read_failed")
	}
	return err
}

// runEnded is when run reached its terminal state: ended_at, else its last update.
func runEnded(run types.AgentRun) time.Time {
	if run.EndedAt != nil {
		return *run.EndedAt
	}
	return run.UpdatedAt
}

// storedAgentCast is one agent output cast of a run found in the recording store, and its size.
type storedAgentCast struct {
	session string
	size    int64
}

// storedAgentCasts is the run's agent output casts in the recording store.
func (s *Server) storedAgentCasts(ctx context.Context, runID uuid.UUID) ([]storedAgentCast, error) {
	var out []storedAgentCast
	for _, session := range []string{agentCastSession, agentCastRecoveredSession} {
		size, _, err := s.cfg.RecordingStore.StatAndTail(ctx, recording.CastKey(runID.String(), session), 0)
		switch {
		case err == nil:
			out = append(out, storedAgentCast{session: session, size: size})
		case !errors.Is(err, recording.ErrNotFound):
			return nil, err
		}
	}
	return out, nil
}

// settleStoredAgentCasts settles a run whose agent output cast is stored while it still owes one: the
// process that saved it went, or its recorder refused, before the row. The run page finds a cast by its
// session.recording.write row, so a success row naming each stored cast is durably written first, then
// the obligation is cleared with the run recorded. A crash between leaves a second row on the next
// sweep; never none. The row (swept: true) carries the completeness the saver stamped in the cast's
// header (storedCastFlags); a header that cannot be read says incomplete, completeness unknown, so a
// cast is never called complete on no evidence.
func (s *Server) settleStoredAgentCasts(ctx context.Context, st store.RunAgentCastStore, run types.AgentRun, stored []storedAgentCast) error {
	for _, sc := range stored {
		key := recording.CastKey(run.ID.String(), sc.session)
		data := map[string]any{"session": sc.session, "key": key, "bytes": sc.size, "swept": true}
		if flags, known := s.storedCastFlags(ctx, key); known {
			maps.Copy(data, flags)
		} else {
			data["incomplete"], data["completeness"] = true, "unknown"
		}
		if sc.session == agentCastRecoveredSession {
			data["recovered"] = true
		}
		if !s.recordStreamAuditDurably(ctx, run.SandboxRef, s.auditEvent(&run.ID, types.ActorSystem, "wardynd",
			"session.recording.write", key, "success", mustJSON(data))) {
			return agentCastStoreErr(ctx, run.ID, "record a run's stored agent output cast", errAgentCastRowRejected)
		}
	}
	return agentCastStoreErr(ctx, run.ID, "clear a run's agent output obligation after its cast", st.ClearRunAgentCastOwed(ctx, run.ID, true))
}

// castCopy is a sweep's re-read of the runner's copy straight into an agent cast, with no output tail:
// the run's output row is already settled. A gapped copy carries the marker, as a tail's does.
type castCopy struct{ c *agentCast }

func (w castCopy) Write(p []byte) (int, error) {
	w.c.write(p)
	return len(p), nil
}

func (w castCopy) OutputGap(skipped int64) {
	w.c.gapBytes.Store(skipped)
	w.c.write([]byte(outputGapMarker(skipped)))
}

// agentCastFlagsKey is the asciicast header key a saved agent cast keeps its completeness under: the
// marks its row carries (truncated, gap_bytes, incomplete, read_error, mask_gap), absent when it has
// none. Players ignore a header key they do not know.
const agentCastFlagsKey = "wardyn_completeness"

// stampCastFlags is snap with flags in its header line, or snap as it is when there are none.
func stampCastFlags(snap []byte, flags map[string]any) []byte {
	head, rest, ok := bytes.Cut(snap, []byte("\n"))
	var h map[string]any
	if len(flags) == 0 || !ok || json.Unmarshal(head, &h) != nil {
		return snap
	}
	h[agentCastFlagsKey] = flags
	b, err := json.Marshal(h)
	if err != nil {
		return snap
	}
	return append(append(b, '\n'), rest...)
}

// storedCastFlags reads the completeness a stored cast's header keeps (stampCastFlags): known is false
// when the header cannot be read, and flags is nil for a cast saved with none.
func (s *Server) storedCastFlags(ctx context.Context, key string) (flags map[string]any, known bool) {
	rc, err := s.cfg.RecordingStore.OpenCast(ctx, key)
	if err != nil {
		return nil, false
	}
	defer rc.Close()
	line, err := bufio.NewReader(io.LimitReader(rc, 64<<10)).ReadBytes('\n')
	var h map[string]any
	if (err != nil && !errors.Is(err, io.EOF)) || json.Unmarshal(bytes.TrimSpace(line), &h) != nil {
		return nil, false
	}
	flags, _ = h[agentCastFlagsKey].(map[string]any)
	return flags, true
}
