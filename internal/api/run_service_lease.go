// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A service's lease renews itself only when an absolute lifetime cap applies: the pool's
// background.lifetime.max_sec or the deployment's WARDYN_RUN_MAX_AGE. The unit is the lease
// the run captured (max_end_ahead_sec, else default_end_sec), held to the boundary's maximum
// the way every PATCH is; the bound is that cap, which this never passes. While the service is
// starting or healthy and its end is within half a unit, the lease sweep moves the end to now
// plus the furthest the cap allows, so a healthy service lives as long as its cap lets it. A
// service with no cap, an unhealthy one, or one not heard from lately (renewalHealth) is not
// renewed and ends at its lease, with the usual run.ending_soon warnings; the uncapped one is
// told so before launch (serviceNoCapWarning).

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

// projectServiceHealth is what a run's service_status says on the wire, from one place for every route
// that serves a run (projectStatusDetailFor): a paused run's last read (reason paused), and one older than
// serviceStatusStale (reason unreachable: the health sweep skips a run whose runner is offline, so nothing
// rewrites it), is unknown, never the health it last had. observed_at is kept, so the reader still sees
// when it was last checked; the stored row is not changed.
func projectServiceHealth(runs []types.AgentRun, now time.Time) {
	for i := range runs {
		st := runs[i].ServiceStatus
		if st == nil {
			continue
		}
		derived := *st
		switch {
		case runs[i].PausedAt != nil:
			derived.Reason = "paused"
		case now.Sub(st.ObservedAt) > serviceStatusStale:
			derived.Reason = "unreachable"
		default:
			continue
		}
		derived.Health = types.ServiceHealthUnknown
		runs[i].ServiceStatus = &derived
	}
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
	if run.Service == nil || run.LifetimeCapAt == nil || run.EndsAt == nil || run.LostAt != nil || run.PausedAt != nil {
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
	capAhead := run.LifetimeCapAt.Sub(now)
	bound := latestEndAhead(unit, &capAhead, run.LifetimeCapSource)
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
// when its half-life opens: capped, starting or healthy, with a lease unit, and an end short of the
// target the lifetime cap allows (the cap's second, as planServiceRenewal truncates it). An uncapped
// service never renews, so it gets the normal run.ending_soon warnings.
func serviceRenewsLater(run types.AgentRun, now time.Time) bool {
	if run.Service == nil || run.LifetimeCapAt == nil || run.EndsAt == nil || run.LostAt != nil || run.PausedAt != nil {
		return false
	}
	if h := renewalHealth(run, now); h != types.ServiceHealthStarting && h != types.ServiceHealthHealthy {
		return false
	}
	if cmp.Or(run.RunLimits.MaxEndAheadSec, run.RunLimits.DefaultEndSec) == 0 {
		return false
	}
	return run.LifetimeCapAt.Truncate(time.Second).After(*run.EndsAt)
}

// renewServiceLease renews run's lease when planServiceRenewal says so and returns the run as
// the sweep goes on with it: the new end on success, the row it was given on any other outcome.
// The bool says the run's end is renewal's to move, so the sweep sends no run.ending_soon: the run
// was renewed this pass, or it is a service renewal will renew when its half-life opens and whose
// owner still has the authority a renewal needs (extendRefusal). A unit of an hour or less opens the
// warning window before the half-life, and every renewal would warn.
// The write is the PATCH route's compare-and-set, so a person's edit, a re-clamp or a lost
// race leaves the end alone and writes no audit row, as does an owner whose authority to extend
// is refused or whose service identity has lost its authority.
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
	// A service identity that is retired or suspended, or whose sponsor is no longer live, has lost
	// the authority that started the run: the rule that ends its tokens and a binding's launch
	// (serviceRunLive). A refusal, or a check that cannot be answered, renews nothing and audits nothing.
	if live, err := s.serviceRunLive(ctx, run); err != nil || !live {
		slog.WarnContext(ctx, "wardynd: a service's lease is not renewed: its service identity or sponsor is not live",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
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

// serviceRunLive reports whether the service identity that created run still carries the authority it
// launched with: the rules that end its tokens (queueOwnerLive) as of the run's creation. A run
// of a person is live.
func (s *Server) serviceRunLive(ctx context.Context, run types.AgentRun) (bool, error) {
	name, ok := strings.CutPrefix(run.CreatedBy, types.ServicePrincipalPrefix)
	if !ok {
		return true, nil
	}
	si, err := s.serviceRecord(ctx, name)
	if err != nil {
		return false, err
	}
	return s.queueOwnerLive(ctx, serviceAuthority(si, si.Sponsor), run.CreatedAt)
}
