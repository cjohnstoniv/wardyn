// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Tightened limits reach live runs (long-holds design rev 4, §2.2, RL-8). A run
// captures its owner's profile run limits at create and every later PATCH
// clamps against those. When an admin tightens that profile, each live run that
// captured it takes the tighter bound on the next sweep: its captured limits,
// its end (measured from now) and its wait. Loosening never reaches a live run,
// and re-assigning the owner to another profile does not re-clamp: the run is
// tied to the profile it launched under.

// sweepRunLimits is one pass of the re-clamp over every live run that captured
// a profile. Each write is a compare-and-set on the values read, so every
// replica can run it on its own tick and each re-clamp lands, and is audited,
// once. A failed read changes nothing and is retried next tick.
func (s *Server) sweepRunLimits(ctx context.Context) error {
	rc, ok := s.cfg.Store.(store.RunLimitsReclamper)
	if !ok {
		return nil
	}
	runs, err := rc.ListProfiledLiveRuns(ctx)
	if err != nil || len(runs) == 0 {
		return err
	}
	profiles, err := s.cfg.Store.ListGovernanceProfiles(ctx)
	if err != nil {
		return err
	}
	current := make(map[uuid.UUID]types.RunLimits, len(profiles))
	for _, p := range profiles {
		current[p.ID] = p.Limits.RunLimits
	}
	now := s.cfg.Now()
	for _, run := range runs {
		// A deleted profile has no current limits to tighten to; the run keeps
		// the ones it captured.
		if l, ok := current[*run.GovernanceProfileID]; ok {
			s.reclampRun(ctx, rc, run, l, now)
		}
	}
	return nil
}

// reclampRun takes the profile's current limits into one run wherever they are
// tighter than what it captured, and cuts its end and wait to them.
func (s *Server) reclampRun(ctx context.Context, rc store.RunLimitsReclamper, run types.AgentRun, profile types.RunLimits, now time.Time) {
	limits := tightenRunLimits(run.RunLimits, profile)
	if limits == run.RunLimits {
		return
	}
	end := reclampEnd(run.EndsAt, limits, now)
	wait := reclampWait(run.WaitBudgetSec, limits, s.cfg.ApprovalExpiryAfter)
	endMoved := !sameEnd(end, run.EndsAt)
	var tightenedAt *time.Time
	if endMoved {
		tightenedAt = &now
	}
	applied, err := rc.ReclampRunLimits(ctx, run, limits, end, wait, tightenedAt)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: re-clamping a run's limits failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return
	}
	if !applied {
		return
	}
	if endMoved {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.end.set", run.ID.String(),
			"success", mustJSON(map[string]any{
				"reason": "limits_tightened", "from": run.EndsAt, "to": end, "max": limits.MaxEndAheadSec,
				"profile_id": run.GovernanceProfileID,
			})))
	}
	if wait != run.WaitBudgetSec {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.wait_budget.set", run.ID.String(),
			"success", mustJSON(map[string]any{
				"reason": "limits_tightened", "from": run.WaitBudgetSec, "to": wait,
				"max": waitCeilingSec(limits, s.cfg.ApprovalExpiryAfter), "profile_id": run.GovernanceProfileID,
			})))
	}
}

// tightenRunLimits is captured with each bound that binds a live run replaced
// by the profile's current one wherever that is tighter, and only there. The
// defaults are left alone: they shape a new run, not a live one.
func tightenRunLimits(captured, profile types.RunLimits) types.RunLimits {
	out := captured
	out.MaxEndAheadSec = tighterSec(captured.MaxEndAheadSec, profile.MaxEndAheadSec)
	out.MaxWaitSec = tighterSec(captured.MaxWaitSec, profile.MaxWaitSec)
	out.PauseIdleAfterSec = tighterSec(captured.PauseIdleAfterSec, profile.PauseIdleAfterSec)
	out.AllowNoEnd = captured.AllowNoEnd && profile.AllowNoEnd
	out.UserChangesLimits = captured.UserChangesLimits && profile.UserChangesLimits
	return out
}

// tighterSec is the tighter of two bounds where 0 is no bound.
func tighterSec(a, b int) int {
	if a == 0 || (b > 0 && b < a) {
		return b
	}
	return a
}

// reclampEnd is the run's end under limits: no later than now + the max, and
// an end where there was none once No end is no longer allowed. With no max
// there is nothing to cut to, as for a new run.
func reclampEnd(end *time.Time, l types.RunLimits, now time.Time) *time.Time {
	if l.MaxEndAheadSec <= 0 {
		return end
	}
	latest := now.Add(time.Duration(l.MaxEndAheadSec) * time.Second).Truncate(time.Second)
	if (end == nil && !l.AllowNoEnd) || (end != nil && end.After(latest)) {
		return &latest
	}
	return end
}

// reclampWait is the run's wait under limits: no longer than their ceiling.
// A wait of 0 (the deployment's expiry) is longer than any ceiling.
func reclampWait(wait int, l types.RunLimits, deployment time.Duration) int {
	if ceiling := waitCeilingSec(l, deployment); ceiling > 0 && (wait == 0 || wait > ceiling) {
		return ceiling
	}
	return wait
}

// sameEnd reports whether two ends are the same, nil being No end.
func sameEnd(a, b *time.Time) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && a.Equal(*b))
}
