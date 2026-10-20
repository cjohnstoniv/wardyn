// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
	_ "time/tzdata" // a start time is read in an IANA zone; a host or image without zoneinfo must still read it

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// One-time scheduled runs: a create with start_at waits in its pool's queue as a QUEUED run that no
// promotion judges before that time (store.queuePage), then starts through the queue's own start, which
// re-checks everything as the owner. Repeating schedules are not built.

// envScheduledRuns switches scheduled runs on; off (the default) refuses start_at.
const envScheduledRuns = "WARDYN_SCHEDULED_RUNS"

// scheduleMaxAhead is the furthest a start may be scheduled: the queue's longest wait.
const scheduleMaxAhead = queueMaxWaitSec * time.Second

// startAtLocalLayouts are the wall-clock forms start_at may take with no offset, read in its zone.
var startAtLocalLayouts = []string{"2006-01-02T15:04:05", "2006-01-02T15:04"}

// runSchedule is a create's validated start: the instant and the zone it was given in.
type runSchedule struct {
	at   time.Time
	zone string
}

// parseRunSchedule validates a create's start_at and start_time_zone against now: nil, nil when the
// request schedules nothing. Off, a malformed time or zone, a time already past or one more than seven
// days ahead is refused, each with its own reason.
func parseRunSchedule(start, zone string, enabled bool, now time.Time) (*runSchedule, *runRefusal) {
	if start == "" && zone == "" {
		return nil, nil
	}
	if !enabled {
		return nil, runError(http.StatusUnprocessableEntity, reasonScheduledRunsOff, scheduledRunsOffMsg)
	}
	invalid := runError(http.StatusBadRequest, reasonStartAtInvalid, startAtInvalidMsg)
	if start == "" || zone == "" || zone == "Local" {
		return nil, invalid
	}
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return nil, invalid
	}
	at, err := time.Parse(time.RFC3339, start)
	for _, layout := range startAtLocalLayouts {
		if err == nil {
			break
		}
		at, err = time.ParseInLocation(layout, start, loc)
	}
	if err != nil {
		return nil, invalid
	}
	shown := at.In(loc).Format("2006-01-02 15:04 MST")
	switch {
	case !at.After(now):
		return nil, runError(http.StatusUnprocessableEntity, reasonStartAtPast, fmt.Sprintf(startAtPastMsg, shown))
	case at.After(now.Add(scheduleMaxAhead)):
		return nil, runError(http.StatusUnprocessableEntity, reasonStartAtTooFar, fmt.Sprintf(startAtTooFarMsg, shown))
	}
	return &runSchedule{at: at.UTC(), zone: zone}, nil
}

const (
	scheduledRunsOffMsg         = "Scheduled runs are off on this deployment. Start the run now, or ask an admin to turn scheduled runs on."
	scheduledRunsUnavailableMsg = "This deployment places no runs on runners, so a run cannot be scheduled here."
	startAtInvalidMsg           = "start_at needs a date and time (2026-10-21T02:00, or RFC 3339) and start_time_zone an IANA time zone (Europe/London)."
	startAtPastMsg              = "The start time %s has already passed. Choose a time in the future."
	startAtTooFarMsg            = "The start time %s is more than 7 days ahead. Choose a time within 7 days."
)

// scheduledDeadline is a scheduled run's deadline: its wait counted from its start, and never more than
// the queue's longest wait from the request, so nothing is held longer than an unscheduled run can be.
func scheduledDeadline(queuedAt, startAt time.Time, waitSec int) time.Time {
	end, limit := startAt.Add(time.Duration(waitSec)*time.Second), queuedAt.Add(scheduleMaxAhead)
	if end.After(limit) {
		return limit
	}
	return end
}

// scheduledWaitMsg is the 201's sentence for a run scheduled for later.
func scheduledWaitMsg(sch *runSchedule) string {
	loc, err := time.LoadLocation(sch.zone)
	if err != nil {
		loc = time.UTC
	}
	return fmt.Sprintf("This run is scheduled to start at %s (%s).", sch.at.In(loc).Format("2006-01-02 15:04 MST"), sch.zone)
}

// queueOwnerSignIn is the wait of a due scheduled run whose owner's sign-in to its model provider has
// expired and cannot be renewed without them: a status read of their own credential, renewing nothing,
// so the run waits for them to sign in again instead of failing at the start. "" when nothing holds it
// here; the start's own fold still judges the provider.
func (s *Server) queueOwnerSignIn(ctx context.Context, e store.RunQueueEntry, launch queuedLaunch) runnerpool.WaitReason {
	if e.StartAt == nil || launch.ProviderID == "" || s.cfg.Store == nil {
		return ""
	}
	var req createRunRequest
	if json.Unmarshal(launch.Request, &req) != nil {
		return ""
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return ""
	}
	p, ok := modelProviderByID(sc.ModelProviders, launch.ProviderID)
	if !ok {
		return ""
	}
	_, d, err := s.providerLiveness(ctx, p, req.Agent, runIdentitySubject(ctx, e.Owner), false)
	if err != nil || !d.credential {
		return ""
	}
	return runnerpool.WaitOwnerSignIn
}
