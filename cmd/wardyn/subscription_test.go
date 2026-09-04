// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// disconnectServer answers DELETE /api/v1/setup/harness-credential/{provider}
// with one scripted status + body and counts the calls.
func disconnectServer(t *testing.T, status int, body string) (*httptest.Server, func() int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || !strings.HasPrefix(r.URL.Path, "/api/v1/setup/harness-credential/") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() int { return calls }
}

// `subscription disconnect` is idempotent on ABSENCE, and absence is a STATUS
// (404), never a phrase in a body. Matching "not found" anywhere in the body
// made every backend error that happens to contain those words — a Postgres
// `relation "secrets" does not exist`, an object-store 500, a gateway's own
// page — report "no managed Claude subscription was connected" and exit 0, so
// an operator revoking a credential after an incident was told it was gone
// while it was still there. handleHarnessDisconnect (internal/api/harnesscred.go)
// has no 404 arm at all: every one of its refusals is 400/500/503.
func TestSubscriptionDisconnect_IdempotentOnStatusNotOnBodyText(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"200-removed", http.StatusOK, `{"status":"disconnected"}`, false},
		{"404-absent-is-success", http.StatusNotFound, `{"error":"no managed credential"}`, false},
		// The regression: a real failure whose body merely CONTAINS the phrase.
		{"500-store-error-mentioning-not-found", http.StatusInternalServerError,
			`{"error":"delete managed credential: pg secretstore: relation \"secrets\" not found"}`, true},
		{"503-no-secret-store", http.StatusServiceUnavailable, `{"error":"secret store is not configured"}`, true},
		{"400-unknown-provider", http.StatusBadRequest, `{"error":"unknown provider: not found"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := disconnectServer(t, tc.status, tc.body)
			err := execCmd(t, "subscription", "disconnect", "--url", srv.URL, "--token", "tok")
			if tc.wantErr && err == nil {
				t.Fatalf("a %d disconnect exited 0 — the credential is still there and the operator was told it was gone", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("a %d disconnect returned %v, want success", tc.status, err)
			}
			if n := calls(); n != 1 {
				t.Errorf("server saw %d DELETEs, want 1", n)
			}
		})
	}
}
