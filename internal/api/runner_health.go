// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/doctor"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runnerHealthStore keeps a runner's last offline summary and relay status (store.PG.RecordRunnerHealth).
type runnerHealthStore interface {
	RecordRunnerHealth(ctx context.Context, id uuid.UUID, summary *types.RunnerOfflineSummary, relay *types.RunnerRelayStatus, now time.Time, gap time.Duration) (bool, error)
	RunnerHealth(ctx context.Context, id uuid.UUID) (*types.RunnerOfflineSummary, *types.RunnerRelayStatus, error)
}

var _ runnerHealthStore = store.PG{}

// runnerSummaryGap is the least time between two stored offline summaries (and runner.offline_summary.record
// rows) of one runner; a summary sooner than that is dropped.
const runnerSummaryGap = 10 * time.Second

// healthFromRunner keeps the offline summary and relay status a runner's caps carry, once each is
// checked: a stage that is not a doctor stage name, a missing time or an unknown relay state drops that
// part; each reason is scrubbed again of any URL's user information and clipped, whatever the runner did.
// A stored summary writes runner.offline_summary.record. Like the rest of caps, nothing here refuses the event.
func (s *Server) healthFromRunner(ctx context.Context, runnerID uuid.UUID, caps runnerwire.Caps) {
	now := s.cfg.Now().UTC()
	summary, relay := checkedOfflineSummary(caps.OfflineSummary, now), checkedRelayStatus(caps.Relay, now)
	hs, ok := s.cfg.Store.(runnerHealthStore)
	if !ok || (summary == nil && relay == nil) {
		return
	}
	stored, err := hs.RecordRunnerHealth(ctx, runnerID, summary, relay, now, runnerSummaryGap)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: record runner health", slog.String("runner_id", runnerID.String()), slog.Any("err", err))
		return
	}
	if stored {
		s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "runner:"+runnerID.String(),
			placement.ActionRunnerOfflineSummary, runnerID.String(), "success",
			mustJSON(map[string]any{"offline_summary": summary, "source": types.RunnerPostureSourceRunnerAsserted})))
	}
}

func checkedOfflineSummary(in *types.RunnerOfflineSummary, now time.Time) *types.RunnerOfflineSummary {
	if in == nil || !slices.Contains(types.RunnerStages, in.Stage) || in.FirstFailureAt.IsZero() || in.LastFailureAt.Before(in.FirstFailureAt) {
		return nil
	}
	return &types.RunnerOfflineSummary{Stage: in.Stage, Reason: types.ClipRunnerReason(doctor.Scrub(in.Reason)),
		FirstFailureAt: in.FirstFailureAt.UTC(), LastFailureAt: in.LastFailureAt.UTC(), ReportedAt: now}
}

func checkedRelayStatus(in *types.RunnerRelayStatus, now time.Time) *types.RunnerRelayStatus {
	if in == nil || (in.State != types.RelayReachable && in.State != types.RelayUnreachable) || in.Since.IsZero() {
		return nil
	}
	out := &types.RunnerRelayStatus{State: in.State, Since: in.Since.UTC(), ReportedAt: now}
	if in.State == types.RelayUnreachable {
		out.Reason = types.ClipRunnerReason(doctor.Scrub(in.Reason))
	}
	return out
}
