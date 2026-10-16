// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

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
// organisation's mode does not reach it. A runner that stopped recording after placement admitted the
// run under a required setting fails the run (runner_recording_off) and false is returned.
func (s *Server) applyRunRecording(ctx context.Context, run *types.AgentRun, spec *runner.SandboxSpec) bool {
	orgOn := s.cfg.RecordingStore != nil
	spec.Record = &orgOn
	run.NotRecorded = types.NotRecordedFor(orgOn, s.targetRecords(ctx, *run))
	if run.NotRecorded == "" {
		return true
	}
	if run.NotRecorded == types.NotRecordedRunnerDeclined {
		if pool, required := s.recordingRequired(ctx, *run); required {
			s.failAndRevoke(ctx, run.ID, types.RunStarting, runnerpool.RunnerRecordingOffMsg(pool))
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
				run.ID.String(), "failure", mustJSON(map[string]any{"reason": placement.ReasonRunnerRecordingOff})))
			return false
		}
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

// recordingRequired reports whether the organisation or the run's pool requires recording, with the
// pool's name for the refusal. A setting that cannot be read is not required here: placement already
// held the run to it at create.
func (s *Server) recordingRequired(ctx context.Context, run types.AgentRun) (string, bool) {
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if !ok || run.RunnerPoolID == nil {
		return "", false
	}
	pool, err := ps.GetRunnerPool(ctx, *run.RunnerPoolID)
	if err != nil {
		return "", false
	}
	org, err := ps.GetRunnerPoolOrgDefaults(ctx)
	return pool.Name, pool.Recording == types.RecordingRequired || (err == nil && org.Recording == types.RecordingRequired)
}

// targetRecords is whether the run's target records a run its organisation asks it to: the bound
// self-hosted runner's or remote-hosted runner's own facts, else this deployment's runner. Unknown
// facts read as not recording.
func (s *Server) targetRecords(ctx context.Context, run types.AgentRun) bool {
	switch {
	case run.RunnerID != nil:
		f, _ := s.runnerFacts(*run.RunnerID)
		return f.Records
	case run.RunnerPoolExecutor != "":
		f, _ := s.executorFacts(ctx, run.RunnerPoolExecutor)
		return f.Records
	case s.cfg.Runner != nil:
		caps, err := s.cfg.Runner.Capabilities(ctx)
		return err == nil && caps.SessionRecording
	}
	return false
}
