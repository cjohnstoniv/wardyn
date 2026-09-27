// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// Pins the exact wrapped shape: finalizePartFile is shared
	// with support-bundle's writer, and it is this wrap —
	// "recording download for run %s did not complete: %w" — that a future
	// refactor of the shared helper could silently drop.
	want := fmt.Sprintf("recording download for run %s did not complete", runID)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want it to contain %q", err, want)
	}
	if _, serr := os.Stat(out); serr == nil {
		b, _ := os.ReadFile(out)
		t.Fatalf("a truncated %d-byte cast was left at %s — a player opens it and fails there instead of here", len(b), out)
	} else if !os.IsNotExist(serr) {
		t.Fatalf("stat %s: %v", out, serr)
	}
	if entries, err := os.ReadDir(filepath.Dir(out)); err != nil || len(entries) != 0 {
		t.Errorf("interrupted download left temporary files: %v, %v", entries, err)
	}
}

// A RENAME failure is a distinct code path from the
// copy/close failure above — pre-lane, run recording's own .part+rename
// block returned the bare os.Rename error; it now flows through the shared
// finalizePartFile and is wrapped the same way. Occupying the destination
// with a directory makes the download itself succeed and only the rename
// fail, isolating that path.
func TestRunRecording_RenameFailureIsWrappedAndLeavesNoPartFile(t *testing.T) {
	runID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-asciicast")
		_, _ = w.Write([]byte(`{"version":2,"width":80,"height":24}` + "\n"))
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	out := filepath.Join(dir, "run.cast")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", out, err)
	}

	err := execCmd(t, "run", "recording", runID.String(), "-o", out, "--url", srv.URL, "--token", "tok")
	if err == nil {
		t.Fatal("a rename onto an occupied path returned nil")
	}
	want := fmt.Sprintf("recording download for run %s did not complete", runID)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %q, want it to contain %q (the shared finalizePartFile wrap)", err, want)
	}
	if _, serr := os.Stat(out + ".part"); !os.IsNotExist(serr) {
		t.Errorf(".part file was left behind after a failed rename")
	}
}

// a peer that sends headers then never finishes the body (a dead
// connection TCP keepalive won't catch for minutes) otherwise hangs `run
// recording` forever — pkg/client's stream_timeout_test.go FORBIDS fixing
// this by re-imposing Client.Timeout (that cuts off a legitimately large,
// slow .cast). --timeout bounds it instead via context.WithTimeout at the
// call site; the SDK itself is untouched.
func TestRunRecording_TimeoutFlagAbortsAHangingBody(t *testing.T) {
	runID := uuid.New()
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-asciicast")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"version":2,"width":80,"height":24}` + "\n"))
		w.(http.Flusher).Flush()
		<-block // headers sent, body never finishes, connection never closes
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(block) })

	start := time.Now()
	err := execCmd(t, "run", "recording", runID.String(), "--timeout", "100ms", "--url", srv.URL, "--token", "tok")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a hung body past --timeout returned nil — the download never aborted")
	}
	if !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("err = %q, want a deadline error", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("took %s to fail, want ~100ms — --timeout did not bound the download", elapsed)
	}
}
