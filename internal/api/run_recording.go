// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// applyRunRecording decides, once at dispatch, whether the run is recorded. The runner is asked to
// record exactly when the organisation keeps recordings (SandboxSpec.Record; a runner never records
// past its own --recording-store), and the run keeps why it will not be (not_recorded), so an
// unrecorded run is never silently so. The choice is the run's for its life: a later change of the
// organisation's mode does not reach it. A run that would go unrecorded under a required setting
// (the organisation's, or its pool's) fails here, recording_store_off or runner_recording_off, and
// false is returned: placement refused it at create, so only a change since then reaches this.
//
// The level (types.RecordingChoice) is resolved here too and kept on the run. A run resolved to off is
// asked not to record and marked policy_off, unless the organisation keeps no recordings at all, which
// stays org_off whatever the level: WARDYN_RECORDING_STORE=off is the hard off.
//
// A macOS VM's runner keeps no recording of its own, so it is asked not to record, and the organisation
// records the run instead: the console and SSH terminal sessions it opens (newSessionRecorder), the
// desktop while someone watches it at Screen and actions, and a task run's agent output
// (recordMacOSAgent). The run is marked recorded here; a task run's mark is settled once its output tail
// is open, from the cast that tail did or did not get.
func (s *Server) applyRunRecording(ctx context.Context, run *types.AgentRun, spec *runner.SandboxSpec, choice types.RecordingChoice) bool {
	orgOn := s.cfg.RecordingStore != nil
	macOS := run.MachineKind.IsMacOSVM()
	if run.RecordingLevel = choice.Resolve(); run.RecordingLevel == types.RecordingOff {
		// Required (the organisation's or the pool's) is read only for a run that would go unrecorded.
		if _, choice.Required, _ = s.recordingRequired(ctx, *run); choice.Required {
			run.RecordingLevel = choice.Resolve()
		}
	}
	if orgOn && run.RecordingLevel == types.RecordingOff {
		off := false
		spec.Record = &off
		run.NotRecorded = types.NotRecordedPolicyOff
		// The off decision is stored or the run does not start: a run that went unrecorded with no
		// mark on its row would be recorded by the next session that reads it.
		if !s.keepRunRecordingOff(ctx, *run) {
			s.failAndRevoke(ctx, run.ID, types.RunStarting, createRunInternalError)
			return false
		}
		return true
	}
	s.keepRunRecordingLevel(ctx, *run)
	asked := orgOn && !macOS
	spec.Record = &asked
	// A target whose facts cannot be read now is not marked: only a known non-recording runner is.
	records, known := s.targetRecords(ctx, *run)
	if macOS {
		records, known = true, true
	}
	run.NotRecorded = types.NotRecordedFor(orgOn, records || !known)
	if run.NotRecorded == "" {
		return true
	}
	return s.keepOrHoldNotRecorded(ctx, run, "", "")
}

// keepOrHoldNotRecorded is a run that will not be recorded, run.NotRecorded saying why: it is stored and
// the run starts, unless the organisation or the run's pool requires recording (a setting that cannot be
// read counts as required), which fails the run with a run.create failure row. msg, when set, is the
// sentence for a requirement that was read; else the reason's own. declined, when set, is why the run's agent
// output cast was declined at dispatch: a run that starts anyway gets its failure row (keepRunNotRecorded), and one
// whose row the audit recorder will not accept is stopped (macOSAgentDeclineUnrecordedMsg), never started with the
// decline unsaid.
func (s *Server) keepOrHoldNotRecorded(ctx context.Context, run *types.AgentRun, msg, declined string) bool {
	if pool, required, readErr := s.recordingRequired(ctx, *run); required {
		reason, held := recordingHold(run.NotRecorded == types.NotRecordedOrgOff, pool, readErr)
		if msg != "" && readErr == nil {
			held = msg
		}
		s.failAndRevoke(ctx, run.ID, types.RunStarting, held)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"reason": reason})))
		return false
	}
	if !s.keepRunNotRecorded(ctx, *run, declined) {
		s.failAndRevoke(ctx, run.ID, types.RunStarting, macOSAgentDeclineUnrecordedMsg)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"reason": "decline_not_recorded"})))
		return false
	}
	return true
}

// recordingChoice is what applyRunRecording resolves the run's level from: its policy's level (already
// raised to its ceiling's), its ceiling, and the organisation's level. A member's run with no stored
// policy runs their inline policy (or the ceiling itself). An organisation document that cannot be read
// is taken as terminal, locked: the run is recorded at least as it was before levels. wallsFloor is the
// run owner's walls' recording floor (wallsRecordingFloor), applied after the organisation's default.
func recordingChoice(run types.AgentRun, policy types.RunPolicySpec, c dispatchCeiling, site types.SiteConfig, siteErr error, wallsFloor types.RecordingLevel) types.RecordingChoice {
	org := site.Recording
	if siteErr != nil {
		org = &types.SiteRecording{Level: types.RecordingTerminal, Locked: true}
	}
	choice := types.RecordingChoice{Policy: policy.Recording, MemberAuthored: c.recordingBound && run.PolicyID == nil, Org: org, WallsFloor: wallsFloor}
	if c.recordingBound {
		choice.Ceiling, choice.CeilingBy = c.recording, c.profile
	}
	return choice
}

// applyDispatchRecording is applyRunRecording over recordingChoice with the run owner's walls' recording
// floor read in. Walls that cannot be read refuse the run (false, the run failed): it is never recorded
// at a lower level than they ask.
func (s *Server) applyDispatchRecording(ctx context.Context, run *types.AgentRun, spec *runner.SandboxSpec, policy types.RunPolicySpec, c dispatchCeiling, site types.SiteConfig, siteErr error) bool {
	floor, err := s.wallsRecordingFloor(ctx, *run)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: read the run owner's walls for the recording floor; the run is refused",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "your walls could not be read, so the run's recording level cannot be set; walls that cannot be read refuse the run")
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": "read the run owner's walls"})))
		return false
	}
	return s.applyRunRecording(ctx, run, spec, recordingChoice(*run, policy, c, site, siteErr, floor))
}

// keepRunNotRecorded stores why the run is not recorded; best-effort, like the decision's audit. declined,
// when set, is the reason the run's agent output cast was declined at dispatch: it goes in a
// session.recording.write failure row, the one declineAgentOutput writes for a later decline, through the
// durable path (recordStreamAuditDurably), so Audit says why. It reports false only for a row the recorder
// would not accept, durably or into its spool: there is nothing to retry the row from at dispatch, so the
// caller stops the run.
func (s *Server) keepRunNotRecorded(ctx context.Context, run types.AgentRun, declined string) bool {
	st, ok := s.cfg.Store.(interface {
		SetRunNotRecorded(context.Context, uuid.UUID, types.RunNotRecorded) error
	})
	if ok {
		if err := st.SetRunNotRecorded(ctx, run.ID, run.NotRecorded); err != nil {
			slog.WarnContext(ctx, "wardynd: keep why a run is not recorded", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
	}
	if declined == "" {
		return true
	}
	key := recording.CastKey(run.ID.String(), agentCastSession)
	if !s.recordStreamAuditDurably(ctx, run.SandboxRef, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "session.recording.write",
		key, "failure", mustJSON(map[string]any{"session": agentCastSession, "key": key, "reason": declined}))) {
		slog.ErrorContext(ctx, "wardynd: record why a run's agent output is not recorded; the run is stopped", slog.String("run_id", run.ID.String()), slog.String("reason", declined))
		return false
	}
	return true
}

// keepRunRecordingOff stores an off decision, the level and policy_off in one write, and reports
// whether it did.
func (s *Server) keepRunRecordingOff(ctx context.Context, run types.AgentRun) bool {
	st, ok := s.cfg.Store.(interface {
		SetRunRecordingOff(context.Context, uuid.UUID) error
	})
	if !ok {
		return true
	}
	if err := st.SetRunRecordingOff(ctx, run.ID); err != nil {
		slog.ErrorContext(ctx, "wardynd: keep a run's recording off", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return false
	}
	return true
}

// keepRunRecordingLevel stores the run's level. Terminal is not written: the column's empty value
// reads as terminal, so a run resolved the way every run was before levels writes nothing new.
func (s *Server) keepRunRecordingLevel(ctx context.Context, run types.AgentRun) {
	st, ok := s.cfg.Store.(interface {
		SetRunRecordingLevel(context.Context, uuid.UUID, types.RecordingLevel) error
	})
	if !ok || run.RecordingLevel == types.RecordingTerminal {
		return
	}
	if err := st.SetRunRecordingLevel(ctx, run.ID, run.RecordingLevel); err != nil {
		slog.WarnContext(ctx, "wardynd: keep a run's recording level", slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}
}

// recordingRefusal is the reason and sentence for a run a required setting holds: the organisation
// keeps no recordings (storeOff), or the runner does not record; pool is "" for a run with no pool.
func recordingRefusal(storeOff bool, pool string) (placement.Reason, string) {
	switch {
	case storeOff:
		return placement.ReasonRecordingStoreOff, runnerpool.RecordingStoreOffMsg()
	case pool == "":
		return placement.ReasonRunnerRecordingOff, runnerpool.RunnerRecordingOffUnpooledMsg()
	}
	return placement.ReasonRunnerRecordingOff, runnerpool.RunnerRecordingOffMsg(pool)
}

// recordingHold is recordingRefusal for a run held at dispatch: the reason stays the one the
// setting would have given, and a hold that came from a setting that could not be read says so.
func recordingHold(storeOff bool, pool string, readErr error) (placement.Reason, string) {
	reason, msg := recordingRefusal(storeOff, pool)
	if readErr != nil {
		msg = runnerpool.RecordingSettingUnreadableMsg()
	}
	return reason, msg
}

// recordingRequired reports whether the organisation or the run's pool requires recording, with the
// pool's name ("" for a run with no pool) for the refusal. It is asked only for a run known not to
// record, so a setting that cannot be read is required: the run fails closed rather than start
// unrecorded under a requirement it could not rule out, and readErr says that was the cause. A pool
// that no longer exists requires nothing.
func (s *Server) recordingRequired(ctx context.Context, run types.AgentRun) (pool string, required bool, readErr error) {
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if !ok {
		return "", false, nil
	}
	org, err := ps.GetRunnerPoolOrgDefaults(ctx)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: read the organisation's recording requirement; holding the run", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return "", true, err
	}
	required = org.Recording == types.RecordingRequired
	if run.RunnerPoolID == nil {
		return "", required, nil
	}
	rp, err := ps.GetRunnerPool(ctx, *run.RunnerPoolID)
	if errors.Is(err, store.ErrNotFound) {
		return "", required, nil
	}
	if err != nil {
		slog.WarnContext(ctx, "wardynd: read the run's pool recording requirement; holding the run", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return "", true, err
	}
	return rp.Name, required || rp.Recording == types.RecordingRequired, nil
}

// unpooledRecordingRefusal holds a run with no pool to the organisation's required setting at create:
// refused when the organisation keeps no recordings, or when the runner the run names does not record.
// A headless deployment's remote run is never placed, so nothing records it unrecorded; dispatch holds
// every run again.
func (s *Server) unpooledRecordingRefusal(ctx context.Context, req createRunRequest) *runRefusal {
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if !ok {
		return nil
	}
	org, err := ps.GetRunnerPoolOrgDefaults(ctx)
	if err != nil {
		return runServerError("read runner pool defaults", err)
	}
	if org.Recording != types.RecordingRequired {
		return nil
	}
	records := true
	switch {
	case s.cfg.RecordingStore == nil:
		records = false
	case requestPlacement(req) == types.PlacementLocal:
		// Unlike dispatch (applyRunRecording), a runner whose facts cannot be read is refused here:
		// nothing has started yet, so required recording fails closed at the door.
		f, _ := s.runnerFacts(requestRunnerID(req))
		records = f.Records
	case s.cfg.Runner != nil:
		caps, err := s.cfg.Runner.Capabilities(ctx)
		records = err == nil && caps.SessionRecording
	}
	if records {
		return nil
	}
	reason, msg := recordingRefusal(s.cfg.RecordingStore == nil, "")
	return runError(reason.Status(), string(reason), msg)
}

// refuseRecordingWithoutStore answers 422 recording_store_off to a save that would require recording
// while this organisation keeps no session recordings, and reports whether it did.
func (s *Server) refuseRecordingWithoutStore(w http.ResponseWriter, rec types.RecordingRequirement) bool {
	if rec != types.RecordingRequired || s.cfg.RecordingStore != nil {
		return false
	}
	writeErrorReason(w, placement.ReasonRecordingStoreOff.Status(), string(placement.ReasonRecordingStoreOff), runnerpool.RecordingSaveStoreOffMsg())
	return true
}

// refuseDeclinedUpload answers 409 recording_off to an upload for a run marked runner_declined unless
// its runner records whatever the organisation asks (TargetFacts.RecordsUnasked), and reports whether
// it did. A runner that takes the per-run flag and declined, or one that records nothing, wraps
// nothing in wardyn-rec, so a cast for its run can only come from the sandbox; the mark stays. Only a
// self-hosted runner can be that old, and one whose facts cannot be read now is refused too: the mark
// is cleared only on the runner's own word.
//
// A macOS VM run is refused first, whatever its level or mark: the organisation records it (its
// terminal sessions, its agent's output, its watched desktop), so a cast under its key can only come
// from the guest. Later, a recorded macOS run (no mark) would pass the mark test below.
func (s *Server) refuseDeclinedUpload(w http.ResponseWriter, run types.AgentRun) bool {
	if run.MachineKind.IsMacOSVM() {
		writeErrorReason(w, http.StatusConflict, reasonRecordingOff, "a macOS VM run is recorded by the organisation, not uploaded")
		return true
	}
	// A run whose level resolved to off asked its runner to record nothing: a cast can only come
	// from the sandbox, and whatever the runner's age it is not kept. The level is read as well as the
	// mark, so a row whose mark is missing still refuses.
	if run.NotRecorded == types.NotRecordedPolicyOff || run.RecordingLevel == types.RecordingOff {
		writeErrorReason(w, http.StatusConflict, reasonRecordingOff, "this run's recording level is off")
		return true
	}
	if run.NotRecorded != types.NotRecordedRunnerDeclined {
		return false
	}
	if run.RunnerID != nil {
		if f, ok := s.runnerFacts(*run.RunnerID); ok && f.RecordsUnasked {
			return false
		}
	}
	writeErrorReason(w, http.StatusConflict, reasonRecordingOff, "this run's runner does not record it")
	return true
}

// clearDeclinedOnUpload clears not_recorded: runner_declined once a cast for the run is stored: a
// runner that records anyway (one older than per-run recording, refuseDeclinedUpload) proves the
// mark wrong. It reports whether the mark was cleared, for the upload's audit row.
func (s *Server) clearDeclinedOnUpload(ctx context.Context, run types.AgentRun) bool {
	st, ok := s.cfg.Store.(interface {
		ClearRunDeclined(context.Context, uuid.UUID) (bool, error)
	})
	if !ok || run.NotRecorded != types.NotRecordedRunnerDeclined {
		return false
	}
	cleared, err := st.ClearRunDeclined(ctx, run.ID)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: clear a run's runner_declined after its upload", slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}
	return cleared
}

// targetRecords is whether the run's target records a run its organisation asks it to: the bound
// self-hosted runner's or remote-hosted runner's own facts, else this deployment's runner. known is
// false when those facts cannot be read now (a runner not registered here, a capabilities read that
// failed).
func (s *Server) targetRecords(ctx context.Context, run types.AgentRun) (records, known bool) {
	switch {
	case run.RunnerID != nil:
		f, ok := s.runnerFacts(*run.RunnerID)
		return f.Records, ok
	case run.RunnerPoolExecutor != "":
		f, ok := s.executorFacts(ctx, run.RunnerPoolExecutor)
		return f.Records, ok
	case s.cfg.Runner != nil:
		caps, err := s.cfg.Runner.Capabilities(ctx)
		return err == nil && caps.SessionRecording, err == nil
	}
	return false, true
}

// The notices of a macOS VM run, at Review and in the 201's warnings, by the level dispatch will resolve:
// at Screen and actions its desktop is recorded while someone watches it, unmasked, and it gets no window
// on the Mac; at Terminal its desktop is not recorded. Its terminal sessions and a task run's agent output
// are recorded and masked at either. The agent's output is named only where it is kept: not for an interactive
// run (it has no agent exec) or where the output tail is off (openExecOutput), which leaves a task run with no
// agent output cast.
const (
	macOSVMScreenNotice                = "This run's desktop is recorded while someone watches it from the run page. The screen recording isn't masked: anything the desktop shows, a secret included, is kept. A window on your Mac isn't recorded, so this run can't open one. Terminal sessions are recorded and masked."
	macOSVMTerminalNotice              = "At the Terminal level this run's desktop isn't recorded. Terminal sessions and the agent's output are recorded and masked."
	macOSVMTerminalNoticeNoAgentOutput = "At the Terminal level this run's desktop isn't recorded. Terminal sessions are recorded and masked."
)

// macOSVMRecordingNotice is a macOS VM run's recording notice at create and Review, by the level its
// dispatch will resolve (createRecordingChoice): none at off. Nothing about recording refuses a macOS VM run
// here; the caller skips it where the organisation keeps no recordings. noAgentOutput is set for a run that
// keeps no agent output cast (an interactive run, or a task run with the output tail off).
func macOSVMRecordingNotice(level types.RecordingLevel, noAgentOutput bool) string {
	switch level {
	case types.RecordingScreenActions:
		return macOSVMScreenNotice
	case types.RecordingTerminal:
		if noAgentOutput {
			return macOSVMTerminalNoticeNoAgentOutput
		}
		return macOSVMTerminalNotice
	}
	return ""
}

// createRecordingChoice is, at create and Review, the choice dispatch resolves the run's level from
// (recordingChoice): the request's policy level, the caller's ceiling, the organisation's level, the
// organisation's or the pool's requirement, and the caller's walls' floor (wallsRecordingFloor). A read
// that fails is a server error.
func (s *Server) createRecordingChoice(ctx context.Context, r *http.Request, policy types.RecordingLevel, place runPlacement,
	ceiling governanceCeiling, memberAuthored bool) (types.RecordingChoice, *runRefusal) {
	choice := types.RecordingChoice{Policy: policy}
	if s.cfg.Store == nil {
		return choice, nil
	}
	if ps, ok := s.cfg.Store.(store.RunnerPoolStore); ok {
		org, err := ps.GetRunnerPoolOrgDefaults(ctx)
		if err != nil {
			return choice, runServerError("read runner pool defaults", err)
		}
		choice.Required = org.Recording == types.RecordingRequired || (place.pooled && place.row.Recording == types.RecordingRequired)
	}
	site, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return choice, runServerError("read site config", err)
	}
	choice.Org = site.Recording
	if !ceiling.Operator {
		choice.MemberAuthored = memberAuthored
		choice.Ceiling = ceiling.Spec.Recording
	}
	_, principal := actorFromRequest(r)
	if choice.WallsFloor, err = s.wallsRecordingFloor(ctx, types.AgentRun{CreatedBy: principal}); err != nil {
		return choice, runServerError("read your walls", err)
	}
	return choice, nil
}
