// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestApplyGovernanceResult_ProfileWriteFailureReportsItsStep(t *testing.T) {
	g, c := newGovServer(t, client.GovernanceDocument{})
	g.failOn = "POST /api/v1/governance/units"
	res, err := c.ApplyGovernanceResult(context.Background(), client.GovernanceDocument{Units: []client.GovernanceUnit{{Name: "fresh"}}}, false)
	if err == nil {
		t.Fatal("want the profile failure")
	}
	if len(res.Steps) != 1 {
		t.Fatalf("steps = %+v, want the one failed profile create", res.Steps)
	}
	if s := res.Steps[0]; s.Kind != "unit" || s.Op != "create" || s.Name != "fresh" || s.Outcome != "failed" || s.Error == "" {
		t.Fatalf("step = %+v, want a failed create of fresh with its error", s)
	}
}

func TestApplyGovernanceResult_PruneRecordsEveryDeleteStep(t *testing.T) {
	keepID, dropID, asgDrop := uuid.New(), uuid.New(), uuid.New()
	_, c := newGovServer(t, client.GovernanceDocument{
		Units:       []client.GovernanceUnit{{ID: keepID, Name: "kept"}, {ID: dropID, Name: "dropped"}},
		Memberships: []client.GovernanceMembership{{ID: asgDrop, SubjectType: "user", Subject: "b", UnitID: dropID}},
	})
	doc := client.GovernanceDocument{Units: []client.GovernanceUnit{{ID: keepID, Name: "kept"}}}
	res, err := c.ApplyGovernanceResult(context.Background(), doc, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) != 2 {
		t.Fatalf("steps = %+v, want the assignment delete then the profile delete", res.Steps)
	}
	a, p := res.Steps[0], res.Steps[1]
	if a.Kind != "membership" || a.Op != "delete" || a.Outcome != "applied" || a.ID == nil || *a.ID != asgDrop {
		t.Fatalf("assignment step = %+v, want an applied delete of the omitted assignment", a)
	}
	if p.Kind != "unit" || p.Op != "delete" || p.Name != "dropped" || p.Outcome != "applied" || p.ID == nil || *p.ID != dropID {
		t.Fatalf("profile step = %+v, want an applied delete of dropped", p)
	}
}

// TestApplyGovernanceResult_HeldWriteIsPendingNotApplied: a write the server holds for a second
// approver is not applied, and the step must not say it was.
func TestApplyGovernanceResult_HeldWriteIsPendingNotApplied(t *testing.T) {
	f := &pendingFake{holdProfiles: map[string]bool{"held": true}}
	res, err := f.server(t).ApplyGovernanceResult(context.Background(),
		client.GovernanceDocument{Units: []client.GovernanceUnit{{Name: "held"}, {Name: "open"}}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Steps) != 2 || res.Steps[0].Name != "held" || res.Steps[0].Outcome != "pending" ||
		res.Steps[1].Name != "open" || res.Steps[1].Outcome != "applied" {
		t.Fatalf("steps = %+v, want held pending then open applied", res.Steps)
	}
}

func TestApplyDrivesResult_DriveWriteFailureReportsItsStep(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "boom"})
	}))
	t.Cleanup(srv.Close)
	res, err := newTestClient(srv).ApplyDrivesResult(context.Background(),
		client.DrivesDocument{Drives: []client.UserDriveListItem{{UserDrive: client.UserDrive{Name: "home"}}}})
	if err == nil {
		t.Fatal("want the drive failure")
	}
	if len(res.Steps) != 1 {
		t.Fatalf("steps = %+v, want the one failed drive create", res.Steps)
	}
	if s := res.Steps[0]; s.Kind != "drive" || s.Op != "create" || s.Name != "home" || s.Outcome != "failed" || s.Error == "" {
		t.Fatalf("step = %+v, want a failed create of home with its error", s)
	}
}
