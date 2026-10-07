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

// RunSignIn GETs the run's sign-in route and decodes the wire names; a refusal
// carries its reason.
func TestRunSignIn_PathDecodeAndRefusal(t *testing.T) {
	runID := uuid.New()
	status, body := http.StatusOK, `{"state":"waiting","verification_url":"https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH","user_code":"ABCD-EFGH"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/runs/"+runID.String()+"/sign-in" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := newTestClient(srv)

	got, err := c.RunSignIn(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	want := client.RunSignIn{State: client.SignInWaiting,
		VerificationURL: "https://device.sso.us-east-1.amazonaws.com/?user_code=ABCD-EFGH", UserCode: "ABCD-EFGH"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}

	status, body = http.StatusForbidden, `{"error":"only the person who started this run can open it interactively","reason":"run_owner_only"}`
	_, err = c.RunSignIn(context.Background(), runID)
	if apiErr := assertAPIError(t, err, http.StatusForbidden); apiErr.Reason != "run_owner_only" {
		t.Errorf("reason = %q, want run_owner_only", apiErr.Reason)
	}
}
