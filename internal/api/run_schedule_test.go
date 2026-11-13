// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// start_at is refused while the switch is off, malformed, past or later than seven days less the run's
// wait, each with its own reason; a wall-clock time is read in its zone.
func TestParseRunSchedule(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name, start, zone string
		enabled           bool
		interactive       bool
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
		{name: "background latest start", start: "2026-10-26T12:00", zone: "UTC", enabled: true, want: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)},
		{name: "background past its latest start", start: "2026-10-26T12:01", zone: "UTC", enabled: true, status: http.StatusUnprocessableEntity, reason: reasonStartAtTooFar},
		{name: "interactive latest start", start: "2026-10-27T11:00", zone: "UTC", enabled: true, interactive: true, want: time.Date(2026, 10, 27, 11, 0, 0, 0, time.UTC)},
		{name: "interactive past its latest start", start: "2026-10-27T11:01", zone: "UTC", enabled: true, interactive: true, status: http.StatusUnprocessableEntity, reason: reasonStartAtTooFar},
		{name: "exactly seven days", start: "2026-10-27T12:00", zone: "UTC", enabled: true, interactive: true, status: http.StatusUnprocessableEntity, reason: reasonStartAtTooFar},
	} {
		t.Run(c.name, func(t *testing.T) {
			sch, refusal := parseRunSchedule(c.start, c.zone, c.enabled, now, scheduleLeadWaitSec(c.interactive))
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

// A wall-clock start its zone's clocks skip (spring forward) or repeat (fall back) names no single moment, so
// it is refused, as the console refuses it, instead of being moved to whichever offset the parse picked; the
// minutes either side are kept, and an explicit offset names one instant.
func TestParseRunSchedule_ClockChange(t *testing.T) {
	spring, fall := time.Date(2027, 3, 25, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name, start, zone string
		now, want         time.Time
	}{
		{name: "skipped in London", start: "2027-03-28T01:30", zone: "Europe/London", now: spring},
		{name: "skipped in New York", start: "2027-03-14T02:30", zone: "America/New_York", now: time.Date(2027, 3, 10, 12, 0, 0, 0, time.UTC)},
		{name: "the first minute after the skip", start: "2027-03-28T02:00", zone: "Europe/London", now: spring, want: time.Date(2027, 3, 28, 1, 0, 0, 0, time.UTC)},
		{name: "repeated in London", start: "2026-10-25T01:30", zone: "Europe/London", now: fall},
		{name: "repeated, with seconds", start: "2026-10-25T01:30:00", zone: "Europe/London", now: fall},
		{name: "the last minute before the repeat", start: "2026-10-25T00:59", zone: "Europe/London", now: fall, want: time.Date(2026, 10, 24, 23, 59, 0, 0, time.UTC)},
		{name: "a repeated hour named by its offset", start: "2026-10-25T01:30:00+01:00", zone: "Europe/London", now: fall, want: time.Date(2026, 10, 25, 0, 30, 0, 0, time.UTC)},
		{name: "a one-digit hour is no clock change", start: "2027-03-28T2:00", zone: "Europe/London", now: spring, want: time.Date(2027, 3, 28, 1, 0, 0, 0, time.UTC)},
		{name: "a skipped one-digit hour", start: "2027-03-28T1:30", zone: "Europe/London", now: spring},
		{name: "fractional seconds are no clock change", start: "2026-10-25T00:59:00.5", zone: "UTC", now: fall, want: time.Date(2026, 10, 25, 0, 59, 0, 500_000_000, time.UTC)},
	} {
		t.Run(c.name, func(t *testing.T) {
			sch, refusal := parseRunSchedule(c.start, c.zone, true, c.now, scheduleLeadWaitSec(false))
			if c.want.IsZero() {
				if refusal == nil || refusal.status != http.StatusBadRequest || refusal.body.Reason != reasonStartAtInvalid || refusal.body.Detail != startAtClockChangeDetail || !strings.Contains(refusal.body.Error, "when the clocks change") {
					t.Fatalf("schedule = %+v, refusal = %+v; want a 400 %s, detail %s, that names the clock change", sch, refusal, reasonStartAtInvalid, startAtClockChangeDetail)
				}
				return
			}
			if refusal != nil || sch == nil || !sch.at.Equal(c.want) {
				t.Fatalf("schedule = %+v, refusal = %+v; want %s", sch, refusal, c.want)
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
	// The latest start parseRunSchedule accepts keeps the run's whole wait before the cap, so the sweep,
	// which expires before it pages due rows, judges the run before it can expire; a minute later is refused.
	for _, interactive := range []bool{false, true} {
		wait := scheduleLeadWaitSec(interactive)
		latest := now.Add(scheduleMaxAhead - time.Duration(wait)*time.Second)
		sch, refusal := parseRunSchedule(latest.Format(time.RFC3339), "UTC", true, now, wait)
		if refusal != nil {
			t.Fatalf("interactive %v: the latest start %s refused: %s", interactive, latest, refusal.body.Error)
		}
		if got, want := scheduledDeadline(now, sch.at, wait), sch.at.Add(time.Duration(wait)*time.Second); !got.Equal(want) {
			t.Errorf("interactive %v: the latest start's deadline = %s; want its whole wait, %s", interactive, got, want)
		}
		_, refusal = parseRunSchedule(latest.Add(time.Minute).Format(time.RFC3339), "UTC", true, now, wait)
		if refusal == nil || refusal.body.Reason != reasonStartAtTooFar || !strings.Contains(refusal.body.Error, latest.Format(scheduleShownLayout)) {
			t.Errorf("interactive %v: a minute past the latest start = %+v; want %s naming %s", interactive, refusal, reasonStartAtTooFar, latest.Format(scheduleShownLayout))
		}
	}
}

// GET /setup/status says whether scheduled runs are on, to a member too; /healthz, which is anonymous,
// says nothing new.
func TestSetupStatus_FeaturesScheduledRuns(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, &integStore{govEscapeStore: newGovEscapeStore(&capStore{})})
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	for _, on := range []bool{true, false} {
		t.Setenv(envScheduledRuns, map[bool]string{true: "1", false: ""}[on])
		for who, c := range map[string]*http.Cookie{"admin": ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin), "member": member} {
			code, st := decodeSetupSSO(t, srv, c)
			if code != http.StatusOK || st.Features.ScheduledRuns != on {
				t.Errorf("%s with the switch %v: %d features %+v", who, on, code, st.Features)
			}
		}
	}
	if got := redactSetupStatusForUser(SetupStatus{Features: SetupFeatures{ScheduledRuns: true}}); !got.Features.ScheduledRuns {
		t.Error("the member redaction dropped features")
	}

	t.Setenv(envScheduledRuns, "1")
	want, err := os.ReadFile("testdata/healthz_no_org_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	got := do(t, newHarness(t).srv, http.MethodGet, "/healthz", "", "").Body.Bytes()
	if keys := func(b []byte) []string {
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return slices.Sorted(maps.Keys(m))
	}; !slices.Equal(keys(got), keys(want)) {
		t.Errorf("/healthz keys with the switch on = %v; want the golden's %v", keys(got), keys(want))
	}
}
