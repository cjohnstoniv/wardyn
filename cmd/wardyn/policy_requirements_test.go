// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPolicySet_ReportsMissingSecrets pins #2342: the server answers a policy
// save with the stored secrets its grants name, but the CLI decoded into a bare
// RunPolicy and printed success for a policy whose first run fails at launch.
func TestPolicySet_ReportsMissingSecrets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"11111111-1111-1111-1111-111111111111","name":"p","spec":{},` +
			`"requirements":[{"kind":"secret","name":"stripe","status":"missing","fix":"add_secret"},` +
			`{"kind":"secret","name":"have","status":"present"}]}`))
	}))
	defer srv.Close()

	file := filepath.Join(t.TempDir(), "p.json")
	if err := os.WriteFile(file, []byte(`{"name":"p","spec":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	root := rootCmd()
	root.SetArgs([]string{"policy", "set", "-f", file, "--url", srv.URL, "--token", "tok"})
	stdout, stderr := &strings.Builder{}, &strings.Builder{}
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "created policy") {
		t.Errorf("stdout = %q, want the created line", stdout.String())
	}
	if !strings.Contains(stderr.String(), `"stripe"`) || !strings.Contains(stderr.String(), "secret set stripe") {
		t.Errorf("stderr = %q, want a warning naming the missing secret and how to store it", stderr.String())
	}
	if strings.Contains(stderr.String(), "have") {
		t.Errorf("stderr = %q, must not warn about a secret that is present", stderr.String())
	}
}
