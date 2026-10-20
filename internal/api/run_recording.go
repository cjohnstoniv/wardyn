// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
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
func (s *Server) applyRunRecording(ctx context.Context, run *types.AgentRun, spec *runner.SandboxSpec, choice types.RecordingChoice) bool {
	orgOn := s.cfg.RecordingStore != nil
	if run.RecordingLevel = choice.Resolve(); run.RecordingLevel == types.RecordingOff {
		// Required (the organisation's or the pool's) is read only for a run that would go unrecorded.
		if _, choice.Required = s.recordingRequired(ctx, *run); choice.Required {
			run.RecordingLevel = choice.Resolve()
		}
	}
	s.keepRunRecordingLevel(ctx, *run)
	if orgOn && run.RecordingLevel == types.RecordingOff {
		off := false
		spec.Record = &off
		run.NotRecorded = types.NotRecordedPolicyOff
		s.keepRunNotRecorded(ctx, *run)
		return true
	}
	spec.Record = &orgOn
	// A target whose facts cannot be read now is not marked: only a known non-recording runner is.
	records, known := s.targetRecords(ctx, *run)
	run.NotRecorded = types.NotRecordedFor(orgOn, records || !known)
	if run.NotRecorded == "" {
		return true
	}
	if pool, required := s.recordingRequired(ctx, *run); required {
		reason, msg := recordingRefusal(run.NotRecorded == types.NotRecordedOrgOff, pool)
		s.failAndRevoke(ctx, run.ID, types.RunStarting, msg)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"reason": reason})))
		return false
	}
	s.keepRunNotRecorded(ctx, *run)
	return true
}

// recordingChoice is what applyRunRecording resolves the run's level from: its policy's level (already
// raised to its ceiling's), its ceiling, and the organisation's level. A member's run with no stored
// policy runs their inline policy (or the ceiling itself). An organisation document that cannot be read
// is taken as terminal, locked: the run is recorded at least as it was before levels.
func recordingChoice(run types.AgentRun, policy types.RunPolicySpec, c dispatchCeiling, site types.SiteConfig, siteErr error) types.RecordingChoice {
	org := site.Recording
	if siteErr != nil {
		org = &types.SiteRecording{Level: types.RecordingTerminal, Locked: true}
	}
	return types.RecordingChoice{Policy: policy.Recording, MemberAuthored: c.recordingBound && run.PolicyID == nil,
		Ceiling: c.recording, Org: org}
}

// keepRunNotRecorded stores why the run is not recorded; best-effort, like the decision's audit.
func (s *Server) keepRunNotRecorded(ctx context.Context, run types.AgentRun) {
	st, ok := s.cfg.Store.(interface {
		SetRunNotRecorded(context.Context, uuid.UUID, types.RunNotRecorded) error
	})
	if ok {
		if err := st.SetRunNotRecorded(ctx, run.ID, run.NotRecorded); err != nil {
			slog.WarnContext(ctx, "wardynd: keep why a run is not recorded", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
	}
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

// recordingRequired reports whether the organisation or the run's pool requires recording, with the
// pool's name ("" for a run with no pool) for the refusal. A setting that cannot be read is not
// required here: placement already held the run to it at create.
func (s *Server) recordingRequired(ctx context.Context, run types.AgentRun) (string, bool) {
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if !ok {
		return "", false
	}
	org, err := ps.GetRunnerPoolOrgDefaults(ctx)
	required := err == nil && org.Recording == types.RecordingRequired
	if run.RunnerPoolID == nil {
		return "", required
	}
	pool, err := ps.GetRunnerPool(ctx, *run.RunnerPoolID)
	if err != nil {
		return "", required
	}
	return pool.Name, required || pool.Recording == types.RecordingRequired
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
func (s *Server) refuseDeclinedUpload(w http.ResponseWriter, run types.AgentRun) bool {
	// A run whose level resolved to off asked its runner to record nothing: a cast can only come
	// from the sandbox, and whatever the runner's age it is not kept.
	if run.NotRecorded == types.NotRecordedPolicyOff {
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
