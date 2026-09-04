// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A download that dies mid-body must not leave a file that LOOKS complete.
// `run recording -o` os.Create'd the destination and io.Copy'd into it, so an
// interrupted transfer (the CLI's whole-request timeout firing, a dropped
// link, a killed server) left a partial .cast at exactly the path a player —
// or the next script in a CI job — would then open, and the only symptom was a
// parse error much later somewhere else.
func TestRunRecording_InterruptedDownloadLeavesNoFile(t *testing.T) {
	runID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/recording/") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/x-asciicast")
		// Promise more than we send, then abort: the client's io.Copy fails
		// with an unexpected EOF partway through, exactly as a cut transfer does.
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":2,"width":80,"height":24}` + "\n"))
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}))
	t.Cleanup(srv.Close)

	out := filepath.Join(t.TempDir(), "run.cast")
	err := execCmd(t, "run", "recording", runID.String(), "-o", out, "--url", srv.URL, "--token", "tok")
	if err == nil {
		t.Fatal("an interrupted download returned nil — the CLI reported success for a partial cast")
	}
	if !strings.Contains(err.Error(), "recording") {
		t.Errorf("err = %q, want it to name the recording download that did not complete", err)
	}
	if _, serr := os.Stat(out); serr == nil {
		b, _ := os.ReadFile(out)
		t.Fatalf("a truncated %d-byte cast was left at %s — a player opens it and fails there instead of here", len(b), out)
	} else if !os.IsNotExist(serr) {
		t.Fatalf("stat %s: %v", out, serr)
	}
}
