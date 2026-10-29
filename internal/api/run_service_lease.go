// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A service's lease renews itself. The unit is the lease the run captured
// (max_end_ahead_sec, else default_end_sec), held to the boundary's maximum the way
// every PATCH is; the bounds are the pool's and deployment's lifetime cap, which this
// never passes. While the service is starting or healthy and its end is within half a
// unit, the lease sweep moves the end to now plus the furthest the walls allow, so a
// healthy service lives as long as its walls let it. An unhealthy one is not renewed
// and ends at its lease, with the usual run.ending_soon warnings.

// serviceRenewal is one renewal planServiceRenewal decided on.
type serviceRenewal struct {
	target time.Time
	unit   int // seconds: the lease unit after the boundary's maximum
	source types.LimitSource
	health types.ServiceHealthState
}

// planServiceRenewal is the end a service run's lease is renewed to now, or false for none.
// boundarySec is the boundary's end maximum in seconds, 0 for none. A run with no lease
// unit has no end to renew; a run with no health observation yet counts as starting.
func planServiceRenewal(run types.AgentRun, boundarySec int, now time.Time) (serviceRenewal, bool) {
	if run.Service == nil || run.EndsAt == nil || run.LostAt != nil || run.PausedAt != nil {
		return serviceRenewal{}, false
	}
	health := types.ServiceHealthStarting
	if run.ServiceStatus != nil {
		health = run.ServiceStatus.Health
	}
	if health != types.ServiceHealthStarting && health != types.ServiceHealthHealthy {
		return serviceRenewal{}, false
	}
	profile := cmp.Or(run.RunLimits.MaxEndAheadSec, run.RunLimits.DefaultEndSec)
	if profile == 0 {
		return serviceRenewal{}, false
	}
	unit := composer.TighterSec(profile, boundarySec)
	if run.EndsAt.Sub(now) > time.Duration(unit)*time.Second/2 {
		return serviceRenewal{}, false
	}
	var capAhead *time.Duration
	if run.LifetimeCapAt != nil {
		d := run.LifetimeCapAt.Sub(now)
		capAhead = &d
	}
	bound := latestEndAhead(unit, capAhead, run.LifetimeCapSource)
	target := now.Add(bound.ahead).Truncate(time.Second)
	if !target.After(*run.EndsAt) {
		return serviceRenewal{}, false
	}
	source := bound.source
	if unit < profile && source == types.LimitSourceGovernance {
		source = policyref.LayerBoundary
	}
	return serviceRenewal{target: target, unit: unit, source: source, health: health}, true
}

// renewServiceLease renews run's lease when planServiceRenewal says so and returns the run as
// the sweep goes on with it: the new end on success, the row it was given on any other outcome.
// The write is the PATCH route's compare-and-set, so a person's edit, a re-clamp or a lost
// race leaves the end alone and writes no audit row, as does an owner whose authority to extend
// is refused.
func (s *Server) renewServiceLease(ctx context.Context, leaser store.RunLeaser, run types.AgentRun, now time.Time) types.AgentRun {
	// The boundary can only shrink the unit, so a run declined without it is declined with it:
	// the boundary is read only for a run that is about to renew.
	if _, ok := planServiceRenewal(run, 0, now); !ok {
		return run
	}
	boundarySec, err := s.boundaryEndCap(ctx, run)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading the boundary to renew a service's lease failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return run
	}
	renewal, ok := planServiceRenewal(run, boundarySec, now)
	if !ok {
		return run
	}
	// A later end keeps a sandbox and its credentials alive, so a renewal needs the authority a
	// PATCH moving the end later does (extendRefusal), as the owner the run row names. A refusal,
	// or a check that cannot be answered, renews nothing and writes no audit row: the run ends at
	// its lease with the usual warnings.
	if ref := s.extendRefusal(ctx, run, false); ref != nil {
		// A check that could not be answered was logged where it failed (loggedMsg).
		if ref.status != http.StatusServiceUnavailable {
			slog.WarnContext(ctx, "wardynd: a service's lease is not renewed: its owner's authority is refused",
				slog.String("run_id", run.ID.String()), slog.String("reason", ref.reason))
		}
		return run
	}
	applied, err := leaser.SetRunEndAndWait(ctx, run.ID, run.RunLimits, run.EndsAt, run.WaitBudgetSec,
		&renewal.target, run.WaitBudgetSec, nil)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: renewing a service's lease failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return run
	}
	if !applied {
		return run
	}
	// The runner contains a run at the end its lease carries, as in reclampRun.
	if run.Placement == types.PlacementLocal {
		s.renewLeaseAfterEndChange(ctx, run.ID)
	}
	data := map[string]any{
		"reason": "service_renewed", "from": run.EndsAt, "to": renewal.target, "max": renewal.unit,
		"health": string(renewal.health),
	}
	if renewal.source != "" {
		data["source"] = string(renewal.source)
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.end.set", run.ID.String(),
		"success", mustJSON(data)))
	run.EndsAt, run.EndTightenedAt = &renewal.target, nil
	return run
}
