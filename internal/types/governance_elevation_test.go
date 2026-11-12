// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestGovernanceMembershipExpiresAtWire: a permanent membership sends no expires_at, and an elevation's end
// survives a round trip.
func TestGovernanceMembershipExpiresAtWire(t *testing.T) {
	m := GovernanceMembership{ID: uuid.New(), SubjectType: CapabilitySubjectUser, Subject: "sub-a", UnitID: uuid.New()}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "expires_at") {
		t.Errorf("a permanent membership = %s, want no expires_at", b)
	}
	end := time.Now().UTC().Add(time.Hour)
	m.ExpiresAt = &end
	if b, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	var back GovernanceMembership
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.ExpiresAt == nil || !back.ExpiresAt.Equal(end) {
		t.Errorf("round trip of %s = %v, want %v", b, back.ExpiresAt, end)
	}
}

// TestLifetimeCapSourceOnWire: a run's lifetime-cap source is on the wire, and absent when none bound it.
func TestLifetimeCapSourceOnWire(t *testing.T) {
	b, err := json.Marshal(AgentRun{LifetimeCapSource: LimitSourceElevation})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"lifetime_cap_source":"elevation"`) {
		t.Errorf("a run capped by an elevation = %s, want lifetime_cap_source elevation", b)
	}
	if b, err = json.Marshal(AgentRun{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "lifetime_cap_source") {
		t.Errorf("a run with no cap source = %s, want the key omitted", b)
	}
}

// TestDeciderRuleElevationFields: eligible_requesters is required on an elevation rule and refused on any
// other; renew_after is 0s to 168h, stored canonical; a ticket pattern is at most 256 characters and compiles.
func TestDeciderRuleElevationFields(t *testing.T) {
	elevation := func(f func(*DeciderRule)) []DeciderRule {
		r := DeciderRule{Kinds: []string{DeciderKindElevation}, Who: []string{DeciderUnitAdmins}, MaxElevation: "4h",
			EligibleRequesters: []string{"group:support"}}
		f(&r)
		return []DeciderRule{r}
	}
	for name, tc := range map[string]struct {
		rules []DeciderRule
		field string
	}{
		"no eligible_requesters":        {elevation(func(r *DeciderRule) { r.EligibleRequesters = nil }), "eligible_requesters"},
		"an eligible entry not a group": {elevation(func(r *DeciderRule) { r.EligibleRequesters = []string{"unit_admins"} }), "eligible_requesters"},
		"an empty group":                {elevation(func(r *DeciderRule) { r.EligibleRequesters = []string{"group:"} }), "eligible_requesters"},
		"an eligible entry twice": {elevation(func(r *DeciderRule) {
			r.EligibleRequesters = []string{"group:a", "group:a"}
		}), "eligible_requesters"},
		"too many eligible entries": {elevation(func(r *DeciderRule) {
			r.EligibleRequesters = []string{"group:1", "group:2", "group:3", "group:4", "group:5", "group:6", "group:7", "group:8", "group:9"}
		}), "eligible_requesters"},
		"eligible on egress": {[]DeciderRule{{Kinds: []string{DeciderKindEgressDomain}, Who: []string{DeciderOwner},
			EligibleRequesters: []string{"group:a"}}}, "eligible_requesters"},
		"renew_after on image":       {[]DeciderRule{{Kinds: []string{DeciderKindImage}, RenewAfter: "1h"}}, "renew_after"},
		"ticket_pattern on egress":   {[]DeciderRule{{Kinds: []string{DeciderKindEgressDomain}, TicketPattern: "INC-[0-9]+"}}, "ticket_pattern"},
		"renew_after not a duration": {elevation(func(r *DeciderRule) { r.RenewAfter = "7d" }), "renew_after"},
		"renew_after negative":       {elevation(func(r *DeciderRule) { r.RenewAfter = "-1h" }), "renew_after"},
		"renew_after over 168h":      {elevation(func(r *DeciderRule) { r.RenewAfter = "169h" }), "renew_after"},
		"ticket_pattern over 256": {elevation(func(r *DeciderRule) {
			r.TicketPattern = strings.Repeat("a", 257)
		}), "ticket_pattern"},
		"ticket_pattern does not compile": {elevation(func(r *DeciderRule) { r.TicketPattern = "INC-(" }), "ticket_pattern"},
	} {
		_, err := ValidateDeciderRules(tc.rules)
		if err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: err = %v, want a refusal naming %s", name, err, tc.field)
		}
	}
	got, err := ValidateDeciderRules(elevation(func(r *DeciderRule) {
		r.EligibleRequesters = []string{"group:Support", " group:oncall "}
		r.RenewAfter = "1440m"
		r.TicketPattern = "INC-[0-9]{4,}"
	}))
	if err != nil {
		t.Fatalf("a valid elevation rule: %v", err)
	}
	if r := got[0]; r.RenewAfter != "24h" || r.TicketPattern != "INC-[0-9]{4,}" || len(r.EligibleRequesters) != 2 {
		t.Errorf("stored as %+v, want renew_after 24h, the pattern kept and both groups", r)
	}
	for in, want := range map[string]string{"0s": "0s", "168h": "168h", "90m": "1h30m"} {
		got, err := ValidateDeciderRules(elevation(func(r *DeciderRule) { r.RenewAfter = in }))
		if err != nil || got[0].RenewAfter != want {
			t.Errorf("renew_after %q = %v %v, want %q", in, got, err, want)
		}
	}
	if CanonicalDuration(4*time.Hour) != "4h" || CanonicalDuration(90*time.Minute) != "1h30m" {
		t.Errorf("CanonicalDuration = %q %q, want 4h 1h30m", CanonicalDuration(4*time.Hour), CanonicalDuration(90*time.Minute))
	}
}
