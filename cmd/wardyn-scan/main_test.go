// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// TestRunUploadsFactsWithoutAuthorization: the scan result goes to the proxy's
// brokered route as a JSON PUT for this run, carrying the workspace's facts and
// NO Authorization header — the proxy injects the run token, and the sandbox
// must never hold or send one.
func TestRunUploadsFactsWithoutAuthorization(t *testing.T) {
	const runID = "11111111-2222-3333-4444-555555555555"
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "Dockerfile"), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var (
		method, path, ctype string
		auth                []string
		facts               workspacescan.ScanFacts
		calls               int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		method, path, ctype = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		auth = r.Header.Values("Authorization")
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &facts); err != nil {
			t.Errorf("body is not ScanFacts JSON: %v: %q", err, body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("WARDYN_WORKSPACE_DIR", ws)
	t.Setenv("WARDYN_PROXY_URL", srv.URL+"/")
	t.Setenv("WARDYN_RUN_ID", runID)
	if err := run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	if calls != 1 {
		t.Fatalf("uploads = %d, want 1", calls)
	}
	if method != http.MethodPut || path != "/wardyn/v1/scan-results/"+runID || ctype != "application/json" {
		t.Errorf("request = %s %s (%s), want PUT /wardyn/v1/scan-results/%s (application/json)", method, path, ctype, runID)
	}
	if len(auth) != 0 {
		t.Errorf("the scan upload sent Authorization %q; the sandbox must not send a credential", auth)
	}
	if !facts.HasDockerfile {
		t.Errorf("facts = %+v, want the workspace's Dockerfile reported", facts)
	}
}

// TestRunRequiresProxyAndRunID: without somewhere to deliver, the scan fails
// loud instead of scanning into the void.
func TestRunRequiresProxyAndRunID(t *testing.T) {
	t.Setenv("WARDYN_WORKSPACE_DIR", t.TempDir())
	for _, env := range [][2]string{{"", "run"}, {"http://proxy", ""}} {
		t.Setenv("WARDYN_PROXY_URL", env[0])
		t.Setenv("WARDYN_RUN_ID", env[1])
		if err := run(); err == nil {
			t.Errorf("run with WARDYN_PROXY_URL=%q WARDYN_RUN_ID=%q succeeded, want an error", env[0], env[1])
		}
	}
}

// TestRunUploadFailureIsNotFatal: a refused upload leaves the workspace in
// pending_scan and the throwaway run exits cleanly.
func TestRunUploadFailureIsNotFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("WARDYN_WORKSPACE_DIR", t.TempDir())
	t.Setenv("WARDYN_PROXY_URL", srv.URL)
	t.Setenv("WARDYN_RUN_ID", "run")
	if err := run(); err != nil {
		t.Fatalf("a failed upload must not fail the scan, got %v", err)
	}
}
