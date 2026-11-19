// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

// A drive's second place must reach the server on every apply: the PUT replaces
// the whole row, so leaving it out clears one set in the console.
func TestApplyDrivesResult_CarriesSecondPlace(t *testing.T) {
	runnerID := uuid.New()
	want := map[string]any{
		"alt_backend":   "host_path",
		"alt_host_root": "/Users/u/team",
		"alt_runner_id": runnerID.String(),
	}
	// The export round trip: GET /drives JSON decoded and applied unchanged.
	var exported client.DrivesDocument
	exportJSON := `{"drives":[{"id":"` + uuid.NewString() + `","name":"team","backend":"share","alt_backend":"host_path",` +
		`"alt_host_root":"/Users/u/team","alt_runner_id":"` + runnerID.String() + `"}]}`
	if err := json.Unmarshal([]byte(exportJSON), &exported); err != nil {
		t.Fatal(err)
	}
	second := client.UserDrive{
		Name: "team", Backend: "share", AltBackend: client.DriveBackendHostPath,
		AltHostRoot: "/Users/u/team", AltRunnerID: &runnerID,
	}
	withID := second
	withID.ID = uuid.New()
	cases := []struct {
		name   string
		doc    client.DrivesDocument
		method string
	}{
		{"existing drive", client.DrivesDocument{Drives: []client.UserDriveListItem{{UserDrive: withID}}}, http.MethodPut},
		{"new drive", client.DrivesDocument{Drives: []client.UserDriveListItem{{UserDrive: second}}}, http.MethodPost},
		{"exported document", exported, http.MethodPut},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/drives":
					writeJSON(w, http.StatusOK, client.DrivesDocument{})
				case r.Method == tc.method && strings.HasPrefix(r.URL.Path, "/api/v1/drives"):
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode body: %v", err)
					}
					writeJSON(w, http.StatusOK, client.UserDrive{Name: "team"})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(srv.Close)
			if _, err := newTestClient(srv).ApplyDrivesResult(context.Background(), tc.doc); err != nil {
				t.Fatal(err)
			}
			for k, v := range want {
				if body[k] != v {
					t.Errorf("%s body %s = %v, want %v", tc.method, k, body[k], v)
				}
			}
		})
	}
}

// A change between two pages of the allocation read must refuse the document:
// fed back to ApplyDrives it would restore a partial set. One added ahead of
// the rows already read repeats a row; one removed there makes the next page
// skip a row, with nothing read twice (only grant_total moving shows it); one
// moved ahead (its priority raised) repeats a row and is itself never read,
// with grant_total unchanged (only the distinct count shows it).
func TestGetDrives_RefusesAReadThatChangedBetweenPages(t *testing.T) {
	const pageSize = 2
	all := make([]client.UserDriveGrant, 5)
	for i := range all {
		all[i] = client.UserDriveGrant{ID: uuid.New(), SubjectType: "user", Subject: string(rune('a' + i)), Enabled: true}
	}
	added := append([]client.UserDriveGrant{{ID: uuid.New(), SubjectType: "user", Subject: "0", Enabled: true}}, all...)
	moved := append([]client.UserDriveGrant{all[4]}, all[:4]...)
	for name, tc := range map[string]struct {
		later []client.UserDriveGrant
		want  string
	}{
		"added ahead":   {added, "allocations changed while being read, retry"},
		"removed ahead": {all[1:], "allocations changed while being read, retry"},
		"moved ahead":   {moved, "read 4 allocations but the server reports 5"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				now := all
				off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
				if off > 0 {
					now = tc.later
				}
				end := min(off+pageSize, len(now))
				if end < len(now) {
					w.Header().Set("X-Wardyn-Truncated", "true")
				}
				writeJSON(w, http.StatusOK, client.DrivesDocument{Grants: now[min(off, end):end], GrantTotal: len(now)})
			}))
			t.Cleanup(srv.Close)
			doc, err := newTestClient(srv).GetDrives(context.Background())
			if err == nil {
				t.Fatalf("GetDrives returned %d allocations from a read that changed between pages, want a refusal", len(doc.Grants))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
