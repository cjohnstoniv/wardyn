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

func TestApplyDrivesResult_PartialFailureReportsStepsSoFar(t *testing.T) {
	driveID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /api/v1/drives":
			writeJSON(w, http.StatusCreated, client.UserDrive{ID: driveID, Name: "home"})
		case "POST /api/v1/drives/grants":
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "boom"})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	doc := client.DrivesDocument{
		Drives: []client.UserDriveListItem{{UserDrive: client.UserDrive{Name: "home"}}},
		Grants: []client.UserDriveGrant{{SubjectType: "user", Subject: "alice@example.com", DriveID: driveID}},
	}
	res, err := newTestClient(srv).ApplyDrivesResult(context.Background(), doc)
	if err == nil {
		t.Fatal("want the grant failure")
	}
	if len(res.Steps) != 2 {
		t.Fatalf("steps = %+v, want the drive create then the failed grant", res.Steps)
	}
	drive, grant := res.Steps[0], res.Steps[1]
	if drive.Kind != "drive" || drive.Op != "create" || drive.Name != "home" || drive.Outcome != "applied" {
		t.Fatalf("drive step = %+v, want an applied create of home", drive)
	}
	if grant.Kind != "drive_grant" || grant.Op != "upsert" || grant.Outcome != "failed" || grant.Error == "" ||
		grant.ID == nil || *grant.ID != driveID {
		t.Fatalf("grant step = %+v, want a failed upsert on the drive with its error", grant)
	}
}
