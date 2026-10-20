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
func (s *Server) applyRunRecording(ctx context.Context, run *types.AgentRun, spec *runner.SandboxSpec) bool {
	orgOn := s.cfg.RecordingStore != nil
	spec.Record = &orgOn
	// A target whose facts cannot be read now is not marked: only a known non-recording runner is.
	records, known := s.targetRecords(ctx, *run)
	run.NotRecorded = types.NotRecordedFor(orgOn, records || !known)
	if run.NotRecorded == "" {
		return true
	}
	if pool, required, readErr := s.recordingRequired(ctx, *run); required {
		reason, msg := recordingHold(run.NotRecorded == types.NotRecordedOrgOff, pool, readErr)
		s.failAndRevoke(ctx, run.ID, types.RunStarting, msg)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"reason": reason})))
		return false
	}
	st, ok := s.cfg.Store.(interface {
		SetRunNotRecorded(context.Context, uuid.UUID, types.RunNotRecorded) error
	})
	if ok {
		if err := st.SetRunNotRecorded(ctx, run.ID, run.NotRecorded); err != nil {
			slog.WarnContext(ctx, "wardynd: keep why a run is not recorded", slog.String("run_id", run.ID.String()), slog.Any("err", err))
		}
	}
	return true
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
func (s *Server) refuseDeclinedUpload(w http.ResponseWriter, run types.AgentRun) bool {
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
