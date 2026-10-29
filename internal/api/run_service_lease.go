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
// healthy service lives as long as its walls let it. An unhealthy one, or one
// not heard from lately (renewalHealth), is not renewed and ends at its lease, with the usual run.ending_soon warnings.

// serviceStatusStale is how old a service's last health observation may be before renewal reads
// it as unknown. The health sweep rewrites an unchanged status only every serviceStatusRefresh, so
// a live service's observed_at is up to that old; three refreshes missed means no replica is
// reading the run any more (the one holding its runner is gone).
const serviceStatusStale = 3 * serviceStatusRefresh

// renewalHealth is a service's health as renewal reads it: the last observation while it is fresh;
// with none yet, starting inside the supervisor's grace from the run's start (the rule
// serviceObservation applies to a missing state file); unknown otherwise.
func renewalHealth(run types.AgentRun, now time.Time) types.ServiceHealthState {
	if st := run.ServiceStatus; st != nil {
		if now.Sub(st.ObservedAt) > serviceStatusStale {
			return types.ServiceHealthUnknown
		}
		return st.Health
	}
	since := run.CreatedAt
	if run.AdmittedAt != nil {
		since = *run.AdmittedAt
	}
	if now.Sub(since) < serviceSupervisorGrace {
		return types.ServiceHealthStarting
	}
	return types.ServiceHealthUnknown
}

// serviceRenewal is one renewal planServiceRenewal decided on.
type serviceRenewal struct {
	target time.Time
	unit   int // seconds: the lease unit after the boundary's maximum
	source types.LimitSource
	health types.ServiceHealthState
}

// planServiceRenewal is the end a service run's lease is renewed to now, or false for none.
// boundarySec is the boundary's end maximum in seconds, 0 for none. A run with no lease
// unit has no end to renew; health is renewalHealth's, so a stale observation renews nothing.
func planServiceRenewal(run types.AgentRun, boundarySec int, now time.Time) (serviceRenewal, bool) {
	if run.Service == nil || run.EndsAt == nil || run.LostAt != nil || run.PausedAt != nil {
		return serviceRenewal{}, false
	}
	health := renewalHealth(run, now)
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

// serviceRenewsLater is whether a service run planServiceRenewal declined now is one it will renew
// when its half-life opens: starting or healthy, with a lease unit, and an end short of the target
// the lifetime cap allows (the cap's second, as planServiceRenewal truncates it).
func serviceRenewsLater(run types.AgentRun, now time.Time) bool {
	if run.Service == nil || run.EndsAt == nil || run.LostAt != nil || run.PausedAt != nil {
		return false
	}
	if h := renewalHealth(run, now); h != types.ServiceHealthStarting && h != types.ServiceHealthHealthy {
		return false
	}
	if cmp.Or(run.RunLimits.MaxEndAheadSec, run.RunLimits.DefaultEndSec) == 0 {
		return false
	}
	return run.LifetimeCapAt == nil || run.LifetimeCapAt.Truncate(time.Second).After(*run.EndsAt)
}

// renewServiceLease renews run's lease when planServiceRenewal says so and returns the run as
// the sweep goes on with it: the new end on success, the row it was given on any other outcome.
// The bool says the run's end is renewal's to move, so the sweep sends no run.ending_soon: the run
// was renewed this pass, or it is a service renewal will renew when its half-life opens and whose
// owner still has the authority a renewal needs (extendRefusal). A unit of an hour or less opens the
// warning window before the half-life, and every renewal would warn.
// The write is the PATCH route's compare-and-set, so a person's edit, a re-clamp or a lost
// race leaves the end alone and writes no audit row, as does an owner whose authority to extend
// is refused.
func (s *Server) renewServiceLease(ctx context.Context, leaser store.RunLeaser, run types.AgentRun, now time.Time) (types.AgentRun, bool) {
	// The boundary can only shrink the unit, so a run declined without it is declined with it:
	// the boundary is read only for a run that is about to renew.
	if _, ok := planServiceRenewal(run, 0, now); !ok {
		// Quiet only while the renewal it waits for will be allowed: a run whose owner lost the
		// authority to extend is warned now, not first at its half-life.
		return run, serviceRenewsLater(run, now) && s.extendRefusal(ctx, run, false) == nil
	}
	boundarySec, err := s.boundaryEndCap(ctx, run)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading the boundary to renew a service's lease failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return run, false
	}
	renewal, ok := planServiceRenewal(run, boundarySec, now)
	if !ok {
		return run, false
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
		return run, false
	}
	applied, err := leaser.SetRunEndAndWait(ctx, run.ID, run.RunLimits, run.EndsAt, run.WaitBudgetSec,
		&renewal.target, run.WaitBudgetSec, nil)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: renewing a service's lease failed",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		return run, false
	}
	if !applied {
		return run, false
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
	return run, true
}
