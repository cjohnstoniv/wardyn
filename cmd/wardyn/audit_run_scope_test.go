// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// auditScopeServer answers GET /api/v1/runs/{id} with runStatus and the audit
// endpoint with 200 [] — the exact shape the finding names: the audit endpoint's
// member scoping answers an empty page rather than refusing, so the run lookup is
// the only thing that can tell "no events" from "not yours / does not exist".
func auditScopeServer(t *testing.T, runID uuid.UUID, runStatus int, runBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/runs/"+runID.String():
			w.WriteHeader(runStatus)
			_, _ = w.Write([]byte(runBody))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/audit":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runAuditCmd(t *testing.T, url string, args ...string) error {
	t.Helper()
	root := rootCmd()
	root.SetArgs(append(args, "--url", url, "--token", "tok"))
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})
	return root.Execute()
}

// TestAuditRunScopeRefusalIsNotSuccess: `wardyn audit <run-id>` must distinguish
// "this run has no audit events" from "this run does not exist, or you cannot see
// it" — the same way its sibling `wardyn logs` already does.
//
// GET /api/v1/audit is scoped per member SERVER-SIDE by filtering rows, so an
// unknown id and a run belonging to somebody else both answer 200 with an empty
// array. Rendering that as the header row alone and exit 0 turns an authz refusal
// into a clean bill of health: an operator checking whether a run was tampered
// with, or a CI job asserting an audit trail exists, reads "no findings" from a
// request that was actually refused. logsCmd guards the identical case with a
// GetRun first, and says so in a comment; auditCmd had no such call between
// parseID and the render loop.
func TestAuditRunScopeRefusalIsNotSuccess(t *testing.T) {
	id := uuid.New()
	for _, tc := range []struct {
		name     string
		status   int
		body     string
		wantExit int
		args     []string
	}{
		{"unknown run id", http.StatusNotFound, `{"error":"run not found"}`, 3, []string{"audit", id.String()}},
		{"a run the caller cannot see", http.StatusForbidden, `{"error":"forbidden"}`, 2, []string{"audit", id.String()}},
		// --json is the mode a CI job parses, so it is the mode where an
		// unnoticed `[]` does the most damage.
		{"unknown run id, --json", http.StatusNotFound, `{"error":"run not found"}`, 3, []string{"audit", id.String(), "--json"}},
		{"cannot see it, --json", http.StatusForbidden, `{"error":"forbidden"}`, 2, []string{"audit", id.String(), "--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := auditScopeServer(t, id, tc.status, tc.body)
			err := runAuditCmd(t, srv.URL, tc.args...)
			if err == nil {
				t.Fatal("`wardyn audit` exited 0 on a run the server would not show — an empty audit page and a " +
					"refusal are the same output, so the refusal has to come from the run lookup")
			}
			if got := exitCodeFor(err); got != tc.wantExit {
				t.Errorf("exit code %d, want %d (%v)", got, tc.wantExit, err)
			}
		})
	}
}

// TestAuditEmptyTrailOnARealRunStillSucceeds is the other half, and the reason
// the guard is a run LOOKUP rather than a refusal on an empty page: a run that
// genuinely has no audit events yet is not an error, and must keep exiting 0.
func TestAuditEmptyTrailOnARealRunStillSucceeds(t *testing.T) {
	id := uuid.New()
	srv := auditScopeServer(t, id, http.StatusOK, `{"id":"`+id.String()+`","state":"running"}`)
	for _, args := range [][]string{
		{"audit", id.String()},
		{"audit", id.String(), "--json"},
	} {
		if err := runAuditCmd(t, srv.URL, args...); err != nil {
			t.Errorf("%v: `wardyn audit` failed on a real run with an empty trail: %v", args, err)
		}
	}
}
