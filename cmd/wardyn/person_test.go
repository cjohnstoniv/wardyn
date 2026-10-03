// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

func runPersonCmd(t *testing.T, srv *httptest.Server, args ...string) (string, error) {
	t.Helper()
	cmd := personCmd(func() *sdk.Client { return &sdk.Client{BaseURL: srv.URL} })
	out := &strings.Builder{}
	cmd.SetOut(out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// `person erase` posts the scopes to the person's erasure route, escaping the
// principal, and prints what finished.
func TestPersonEraseSendsTheScopesAndPrintsWhatFinished(t *testing.T) {
	var gotPath, gotMethod string
	var gotBody map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.EscapedPath()
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"person":"alice/sub","scopes":["run_tasks","credentials"],"outcome":{"run_tasks":"done","credentials":"done"},"detail":{}}`))
	}))
	t.Cleanup(srv.Close)
	out, err := runPersonCmd(t, srv, "erase", "alice/sub", "--scope", "run_tasks,credentials")
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/people/alice%2Fsub/erasure" {
		t.Errorf("request = %s %s, want POST /api/v1/people/alice%%2Fsub/erasure", gotMethod, gotPath)
	}
	if strings.Join(gotBody["scopes"], ",") != "run_tasks,credentials" {
		t.Errorf("scopes sent = %v", gotBody["scopes"])
	}
	if !strings.Contains(out, "erased run_tasks, credentials for alice/sub") {
		t.Errorf("output = %q", out)
	}
}

// Without --scope nothing is sent: erasing needs the scopes named.
func TestPersonEraseNeedsAScope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a request was sent with no scope")
	}))
	t.Cleanup(srv.Close)
	if _, err := runPersonCmd(t, srv, "erase", "alice"); err == nil || !strings.Contains(err.Error(), "--scope") {
		t.Fatalf("erase with no scope = %v, want the --scope refusal", err)
	}
}

// A partial failure is an error carrying the server's own sentence.
func TestPersonEraseSurfacesAPartialFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"erasure is not complete; retry with the same scopes to finish the rest","reason":"erasure_incomplete","done":["mask_copies"],"remaining":["run_tasks"]}`))
	}))
	t.Cleanup(srv.Close)
	_, err := runPersonCmd(t, srv, "erase", "alice", "--scope", "mask_copies,run_tasks")
	if err == nil || !strings.Contains(err.Error(), "erasure is not complete") {
		t.Fatalf("erase = %v, want the server's incomplete sentence", err)
	}
}
