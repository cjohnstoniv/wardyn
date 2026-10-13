// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func TestBuildWorkspace_SendsTheRefreshBodyOnlyWhenAsked(t *testing.T) {
	wsID := uuid.New()
	for _, tc := range []struct {
		name    string
		refresh bool
		want    string
	}{
		{"a plain build sends no body", false, ""},
		{"a refresh sends the refresh body", true, `{"refresh":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/workspaces/"+wsID.String()+"/build" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				checkAuth(t, r)
				body, _ := io.ReadAll(r.Body)
				if string(body) != tc.want {
					t.Errorf("body = %q, want %q", body, tc.want)
				}
				writeJSON(w, http.StatusAccepted, map[string]string{"state": "building"})
			}))
			defer srv.Close()

			got, err := newTestClient(srv).BuildWorkspace(context.Background(), wsID, tc.refresh)
			if err != nil {
				t.Fatalf("BuildWorkspace: %v", err)
			}
			if got.State != "building" {
				t.Errorf("state = %q, want building", got.State)
			}
		})
	}
}

func TestGetWorkspaceBuild_DecodesTheRecord(t *testing.T) {
	wsID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/workspaces/"+wsID.String()+"/build" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"state":"ready","image":"wardyn-ws:1",`+
			`"record":{"schema":1,"lane":"workspace","build_id":"b1","reason":"refresh","resolved":true},`+
			`"record_sha256":"abc123","reproducible":false}`)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).GetWorkspaceBuild(context.Background(), wsID)
	if err != nil {
		t.Fatalf("GetWorkspaceBuild: %v", err)
	}
	if got.State != "ready" || got.Image != "wardyn-ws:1" {
		t.Errorf("state/image = %q/%q", got.State, got.Image)
	}
	if got.Record == nil || got.Record.BuildID != "b1" || got.Record.Lane != "workspace" {
		t.Errorf("record = %+v, want the decoded build record", got.Record)
	}
	if got.RecordSHA256 != "abc123" {
		t.Errorf("record_sha256 = %q, want abc123", got.RecordSHA256)
	}
	if got.Reproducible == nil || *got.Reproducible {
		t.Errorf("reproducible = %v, want false", got.Reproducible)
	}
}
