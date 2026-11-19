// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestSupportCalls_RouteAndBody: each support call goes to its route with the body the server reads, and
// the reply decodes into the grant.
func TestSupportCalls_RouteAndBody(t *testing.T) {
	run, grant := uuid.New(), uuid.New()
	base := "/api/v1/runs/" + run.String() + "/support"
	type seen struct{ route, body string }
	var got []seen
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&raw)
		got = append(got, seen{r.Method + " " + r.URL.Path, string(raw)})
		writeJSON(w, http.StatusOK, map[string]any{"grant": grant, "helper": "sam@example.test", "state": "requested", "doors_asked": []string{"terminal"}, "requested_at": time.Now().Add(-5 * time.Minute)})
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(srv)
	ctx := context.Background()

	g, err := c.RequestSupport(ctx, run, client.SupportRequest{Doors: []string{"terminal", "files"}, Reason: "stuck build"})
	if err != nil || g.Grant != grant || g.State != "requested" || g.Helper != "sam@example.test" {
		t.Fatalf("RequestSupport = %+v, %v", g, err)
	}
	if _, err := c.AcceptSupport(ctx, run, grant, client.SupportAccept{Doors: []string{"terminal"}, Minutes: 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DeclineSupport(ctx, run, grant); err != nil {
		t.Fatal(err)
	}
	if _, err := c.EndSupport(ctx, run, grant); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SupportStatus(ctx, run); err != nil {
		t.Fatal(err)
	}
	want := []seen{
		{"POST " + base, `{"doors":["terminal","files"],"reason":"stuck build"}`},
		{"POST " + base + "/" + grant.String() + "/accept", `{"doors":["terminal"],"minutes":30}`},
		{"POST " + base + "/" + grant.String() + "/decline", ""},
		{"POST " + base + "/" + grant.String() + "/end", ""},
		{"GET " + base, ""},
	}
	if len(got) != len(want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestSupportStatus_DecodesOpenEndedAndDoors: both grants, the doors offered and the recorded doors decode;
// an absent grant is nil.
func TestSupportStatus_DecodesOpenEndedAndDoors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"open":           nil,
			"ended":          map[string]any{"grant": uuid.New(), "helper": "sam", "state": "ended", "end_reason": "expired", "doors_asked": []string{"files"}, "requested_at": time.Now().Add(-5 * time.Minute)},
			"doors_offered":  []string{"terminal", "commands", "files", "forward"},
			"recorded_doors": []string{"terminal"},
		})
	}))
	t.Cleanup(srv.Close)
	st, err := newTestClient(srv).SupportStatus(context.Background(), uuid.New())
	if err != nil || st.Open != nil || st.Ended == nil || st.Ended.EndReason != "expired" || len(st.DoorsOffered) != 4 || st.RecordedDoors[0] != "terminal" {
		t.Fatalf("SupportStatus = %+v, %v", st, err)
	}
}

// TestSupportRequest_RefusalIsAnAPIError: the server's reason comes back on the *APIError.
func TestSupportRequest_RefusalIsAnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "this run already has a help request or session open", "reason": "support_grant_open"})
	}))
	t.Cleanup(srv.Close)
	_, err := newTestClient(srv).RequestSupport(context.Background(), uuid.New(), client.SupportRequest{Doors: []string{"terminal"}, Reason: "x"})
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusConflict || apiErr.Reason != "support_grant_open" {
		t.Fatalf("err = %v, want a 409 support_grant_open APIError", err)
	}
}
