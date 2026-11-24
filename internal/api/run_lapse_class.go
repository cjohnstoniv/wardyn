// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// lapseCauseOutage is the run.lost cause on every row the lapse sweep writes during an outage-class pass.
	lapseCauseOutage = "control_plane_outage"
	// lapseCauseRunnerLink is the run.lost cause on every row OfflineFromRunner writes.
	lapseCauseRunnerLink = "runner_link_lost"
	// lapseCensusWindow is how far back a lapse still counts toward rule M's mass-lapse share.
	lapseCensusWindow = 30 * time.Minute
	// lapseNoRenewAfter is how long none of the server's runs may have renewed for rule M's no-renewals condition.
	lapseNoRenewAfter = 20 * time.Minute
)

// lapseVerdict is one lapse sweep pass's answer to "is this mass silence our fault?" (rule M): a pass is
// outage-class when the server has just recovered, when none of its own runs has renewed for a while, or when
// a large share of them lapsed together.
type lapseVerdict struct {
	outage     bool
	conditions []string // "recovering", "no_renewals", "mass_lapse", "census_unread"
	cause      string   // the run.lost cause outside an outage pass; "" adds none
	census     store.LapseCensus
	censusRead bool
	healthyFor time.Duration
}

// holdsUnattended: an outage pass keeps the server's own headless runs as it keeps interactive ones.
func (v lapseVerdict) holdsUnattended(run types.AgentRun) bool {
	return v.outage && !run.Interactive && run.Placement != types.PlacementLocal && run.RunnerID == nil
}

// lapseVerdictNow is the verdict for the current pass. A nil tracker, or a grace of 0, turns rule M off.
func (s *Server) lapseVerdictNow(ctx context.Context) lapseVerdict {
	h := s.cfg.StoreHealth
	if h == nil || h.Grace() == 0 {
		return lapseVerdict{}
	}
	v := lapseVerdict{healthyFor: h.HealthyFor()}
	if h.Recovering() {
		v.outage, v.conditions = true, []string{"recovering"}
		return v
	}
	reader, ok := s.cfg.Store.(store.LapseCensusReader)
	if !ok {
		return lapseVerdict{}
	}
	census, err := reader.LapseCensus(ctx, runTokenLapseAfter, lapseCensusWindow)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: reading the lapse census failed; this pass is treated as an outage", slog.Any("err", err))
		v.outage, v.conditions = true, []string{"census_unread"}
		return v
	}
	v.census, v.censusRead = census, true
	if census.Running >= 1 && census.NewestRenewAge > lapseNoRenewAfter {
		v.conditions = append(v.conditions, "no_renewals")
	}
	minLapsed, sharePct := s.cfg.LapseMassMin, s.cfg.LapseMassSharePct
	if minLapsed == 0 {
		minLapsed = 5
	}
	if sharePct == 0 {
		sharePct = 25
	}
	if census.Lapsed >= minLapsed && census.Lapsed*100 >= sharePct*census.Running {
		v.conditions = append(v.conditions, "mass_lapse")
	}
	v.outage = len(v.conditions) > 0
	return v
}

// recordLapseHold writes the run.lapse_sweep.hold row: this pass kept held of the server's own runs under the verdict.
func (s *Server) recordLapseHold(ctx context.Context, v lapseVerdict, held int) {
	data := map[string]any{
		"conditions":      v.conditions,
		"healthy_for_sec": int(v.healthyFor / time.Second),
		"held":            held,
	}
	if v.censusRead {
		data["server_runs"] = v.census.Running
		data["lapsed_recent"] = v.census.Lapsed
		if v.census.Running >= 1 {
			data["newest_renew_age_sec"] = int(v.census.NewestRenewAge / time.Second)
		}
	}
	s.recordAudit(ctx, s.auditEvent(nil, types.ActorSystem, "wardynd", "run.lapse_sweep.hold", "lapse_sweep", "success", mustJSON(data)))
}
