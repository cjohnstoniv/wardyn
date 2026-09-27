// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeProxy answers the create with createBody verbatim and every poll with
// pollState, counting the polls.
func fakeProxy(t *testing.T, createStatus int, createBody, pollState string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/wardyn/v1/approvals":
			w.WriteHeader(createStatus)
			_, _ = w.Write([]byte(createBody))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/wardyn/v1/approvals/"):
			polls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]string{"state": pollState})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &polls
}

const readArgs = `{"tool_name":"Read","input":{"file_path":"a.go"},"tool_use_id":"t"}`

// A create answer with a state and no id is the run's tool_rules deciding: the
// gate answers from it without polling, and anything but APPROVED is a deny —
// an unrecognised state must never read as permission.
func TestPolicyDecisionWithoutIDShortCircuits(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"approved", `{"state":"APPROVED","decided_by":"policy"}`, "allow"},
		{"denied", `{"state":"DENIED","decided_by":"policy"}`, "deny"},
		{"denied bare", `{"state":"DENIED"}`, "deny"},
		{"pending without id", `{"state":"PENDING"}`, "deny"},
		{"lower case", `{"state":"approved"}`, "deny"},
		{"unknown", `{"state":"SOMETHING_NEW"}`, "deny"},
		{"neither", `{}`, "deny"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, polls := fakeProxy(t, http.StatusOK, tc.body, "APPROVED")
			pr := drive(t, srv.URL, readArgs)
			if pr["behavior"] != tc.want {
				t.Fatalf("behavior = %v, want %s (%+v)", pr["behavior"], tc.want, pr)
			}
			if n := polls.Load(); n != 0 {
				t.Fatalf("gate polled %d times for a policy decision — there is no approval row to poll", n)
			}
		})
	}
}

// TestProxyRecordedResponsesDriveTheGate is the contract across the seam: the
// bytes internal/egress/proxy's TestToolRulesAtTheLocalRoute pins as the
// route's answers, fed to the gate. held.json is a real raise relayed through
// the proxy — the control plane's created row, which carries BOTH an id and
// "state":"PENDING" — and must be polled, not read as a decision.
func TestProxyRecordedResponsesDriveTheGate(t *testing.T) {
	dir := filepath.Join("..", "..", "internal", "egress", "proxy", "testdata", "toolgate-contract")
	for _, tc := range []struct {
		file       string
		status     int
		want       string
		wantPolled bool
	}{
		{"approved.json", http.StatusOK, "allow", false},
		{"denied.json", http.StatusOK, "deny", false},
		{"held.json", http.StatusCreated, "allow", true},
	} {
		t.Run(tc.file, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(dir, tc.file))
			if err != nil {
				t.Fatal(err)
			}
			srv, polls := fakeProxy(t, tc.status, string(body), "APPROVED")
			pr := drive(t, srv.URL, readArgs)
			if pr["behavior"] != tc.want {
				t.Fatalf("behavior = %v, want %s (%+v)", pr["behavior"], tc.want, pr)
			}
			if polled := polls.Load() > 0; polled != tc.wantPolled {
				t.Fatalf("polled = %v, want %v", polled, tc.wantPolled)
			}
			if tc.want == "allow" {
				ui, _ := pr["updatedInput"].(map[string]any)
				if ui["file_path"] != "a.go" {
					t.Fatalf("allow did not echo the input: %+v", pr)
				}
			}
		})
	}
}
