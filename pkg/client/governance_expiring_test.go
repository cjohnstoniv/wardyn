// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// expiringFixture is a current state of one unit holding a permanent membership and an elevation, and a
// document that names the permanent one and an elevation that has already ended.
func expiringFixture() (current, doc client.GovernanceDocument) {
	unit := client.GovernanceUnit{ID: uuid.New(), Name: "support"}
	ends := time.Now().UTC().Add(time.Hour)
	ended := time.Now().UTC().Add(-time.Hour)
	permanent := client.GovernanceMembership{ID: uuid.New(), SubjectType: client.CapabilitySubjectType("user"), Subject: "sub-keep", UnitID: unit.ID, Priority: 1}
	elevation := client.GovernanceMembership{ID: uuid.New(), SubjectType: client.CapabilitySubjectType("user"), Subject: "sub-elevated", UnitID: unit.ID, ExpiresAt: &ends}
	stale := client.GovernanceMembership{SubjectType: client.CapabilitySubjectType("user"), Subject: "sub-was-elevated", UnitID: unit.ID, ExpiresAt: &ended}
	current = client.GovernanceDocument{Units: []client.GovernanceUnit{unit}, Memberships: []client.GovernanceMembership{permanent, elevation}}
	doc = client.GovernanceDocument{Units: []client.GovernanceUnit{unit}, Memberships: []client.GovernanceMembership{permanent, stale}}
	return current, doc
}

// TestPlanSkipsExpiringMemberships: a plan with prune never deletes a live elevation the document omits,
// and never creates an elevation the document carries (one captured by a get before it ended).
func TestPlanSkipsExpiringMemberships(t *testing.T) {
	current, doc := expiringFixture()
	_, c := newGovServer(t, current)
	plan, err := c.PlanGovernance(context.Background(), doc, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range plan.Steps {
		if strings.Contains(s.Subject, "elevated") {
			t.Errorf("plan step %+v touches an elevation; want none", s)
		}
	}
}

// TestApplySkipsExpiringMemberships: the same document applied with prune writes no membership.
func TestApplySkipsExpiringMemberships(t *testing.T) {
	current, doc := expiringFixture()
	g, c := newGovServer(t, current)
	if _, err := c.ApplyGovernanceResult(context.Background(), doc, true); err != nil {
		t.Fatal(err)
	}
	for _, w := range g.writes {
		if strings.Contains(w, "/memberships") {
			t.Errorf("apply wrote %s; want no membership write", w)
		}
	}
}

// TestRequestElevationWire: RequestElevation posts kind elevation with the unit, a canonical duration, the
// reason and the ticket, and returns the request from the 202.
func TestRequestElevationWire(t *testing.T) {
	unit, change := uuid.New(), uuid.New()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/governance/requests" {
			t.Errorf("request %s %s, want POST /api/v1/governance/requests", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"pending_change": map[string]any{"id": change, "state": "pending"},
			"request": map[string]any{"id": change, "state": "pending", "kind": "elevation", "unit_id": unit,
				"unit_name": "support", "duration": "4h", "mine": true, "decidable": false,
				"deciders": []map[string]any{{"kind": "security"}, {"kind": "group", "name": "support-leads"}}},
		})
	}))
	t.Cleanup(srv.Close)
	req, err := newTestClient(srv).RequestElevation(context.Background(), unit, 4*time.Hour, "on call", "INC-1234")
	if err != nil {
		t.Fatal(err)
	}
	if got["kind"] != "elevation" || got["unit_id"] != unit.String() || got["duration"] != "4h" || got["reason"] != "on call" || got["ticket"] != "INC-1234" {
		t.Errorf("body = %v, want kind, unit_id, duration 4h, reason and ticket", got)
	}
	if req.ID != change || req.Kind != "elevation" || req.UnitID == nil || *req.UnitID != unit || req.Duration != "4h" || !req.Mine ||
		len(req.Deciders) != 2 || req.Deciders[1].Kind != "group" || req.Deciders[1].Name != "support-leads" {
		t.Errorf("request = %+v, want the 202's request", req)
	}
}

// TestListGovernanceRequests404IsAPIError: an older server answers 404, which the caller reads as an
// *APIError with that status.
func TestListGovernanceRequests404IsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}))
	t.Cleanup(srv.Close)
	_, err := newTestClient(srv).ListGovernanceRequests(context.Background())
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Errorf("err = %v, want an *APIError with status 404", err)
	}
}
