// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"
	"time"
)

// start_at is refused while the switch is off, malformed, past or more than seven days ahead, each with
// its own reason; a wall-clock time is read in its zone.
func TestParseRunSchedule(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name, start, zone string
		enabled           bool
		status            int
		reason            string
		want              time.Time
	}{
		{name: "nothing scheduled", enabled: false},
		{name: "switch off", start: "2026-10-21T02:00", zone: "UTC", status: http.StatusUnprocessableEntity, reason: reasonScheduledRunsOff},
		{name: "wall clock in its zone", start: "2026-10-21T02:00", zone: "Europe/London", enabled: true, want: time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)},
		{name: "rfc 3339 keeps its offset", start: "2026-10-21T02:00:00Z", zone: "America/New_York", enabled: true, want: time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC)},
		{name: "zone missing", start: "2026-10-21T02:00", enabled: true, status: http.StatusBadRequest, reason: reasonStartAtInvalid},
		{name: "time missing", zone: "UTC", enabled: true, status: http.StatusBadRequest, reason: reasonStartAtInvalid},
		{name: "zone unknown", start: "2026-10-21T02:00", zone: "Mars/Olympus", enabled: true, status: http.StatusBadRequest, reason: reasonStartAtInvalid},
		{name: "server local zone", start: "2026-10-21T02:00", zone: "Local", enabled: true, status: http.StatusBadRequest, reason: reasonStartAtInvalid},
		{name: "not a time", start: "tomorrow", zone: "UTC", enabled: true, status: http.StatusBadRequest, reason: reasonStartAtInvalid},
		{name: "past", start: "2026-10-20T11:59", zone: "UTC", enabled: true, status: http.StatusUnprocessableEntity, reason: reasonStartAtPast},
		{name: "now", start: "2026-10-20T12:00", zone: "UTC", enabled: true, status: http.StatusUnprocessableEntity, reason: reasonStartAtPast},
		{name: "exactly seven days", start: "2026-10-27T12:00", zone: "UTC", enabled: true, want: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)},
		{name: "past seven days", start: "2026-10-27T12:01", zone: "UTC", enabled: true, status: http.StatusUnprocessableEntity, reason: reasonStartAtTooFar},
	} {
		t.Run(c.name, func(t *testing.T) {
			sch, refusal := parseRunSchedule(c.start, c.zone, c.enabled, now)
			switch {
			case c.reason != "":
				if refusal == nil || refusal.status != c.status || refusal.body.Reason != c.reason {
					t.Fatalf("refusal = %+v; want %d %s", refusal, c.status, c.reason)
				}
			case refusal != nil:
				t.Fatalf("refused %d %s: %s", refusal.status, refusal.body.Reason, refusal.body.Error)
			case c.want.IsZero():
				if sch != nil {
					t.Fatalf("schedule = %+v; want none", sch)
				}
			case sch == nil || !sch.at.Equal(c.want) || sch.zone != c.zone:
				t.Fatalf("schedule = %+v; want %s in %s", sch, c.want, c.zone)
			}
		})
	}
}

// A scheduled run's wait counts from its start, and nothing is held more than seven days from the request.
func TestScheduledDeadline(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if got, want := scheduledDeadline(now, now.Add(time.Hour), queueBackgroundWaitSec), now.Add(25*time.Hour); !got.Equal(want) {
		t.Errorf("an hour ahead = %s; want %s", got, want)
	}
	if got, want := scheduledDeadline(now, now.Add(scheduleMaxAhead), queueBackgroundWaitSec), now.Add(scheduleMaxAhead); !got.Equal(want) {
		t.Errorf("seven days ahead = %s; want %s", got, want)
	}
}
