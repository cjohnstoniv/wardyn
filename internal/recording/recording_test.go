// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// ── store tests ──────────────────────────────────────────────────────────────

func TestFSStore_TraversalRejection(t *testing.T) {
	store, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	evil := []string{
		"../etc/passwd",
		"../../etc/shadow",
		"run/../../secret",
		"",
		"run\x00bad",
		"/abs/path",
	}
	for _, id := range evil {
		if err := store.SaveCast(ctx, id, strings.NewReader("x")); err == nil {
			t.Errorf("SaveCast(%q) should have been rejected", id)
		}
		if _, err := store.OpenCast(ctx, id); err == nil {
			t.Errorf("OpenCast(%q) should have been rejected", id)
		}
	}
}

func TestFSStore_SweepRemovesOnlyAgedFiles(t *testing.T) {
	root := t.TempDir()
	store, err := recording.NewFSStore(root)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	for _, id := range []string{"old-run", "fresh-run"} {
		if err := store.SaveCast(ctx, id, strings.NewReader("{}\n")); err != nil {
			t.Fatalf("SaveCast(%s): %v", id, err)
		}
	}
	// An orphaned atomic-write temp file (crash between CreateTemp and Rename).
	orphan := filepath.Join(root, ".tmp-cast-orphan")
	if err := os.WriteFile(orphan, []byte("partial"), 0o600); err != nil {
		t.Fatalf("write orphan: %v", err)
	}
	aged := time.Now().Add(-48 * time.Hour)
	for _, p := range []string{filepath.Join(root, "old-run.cast"), orphan} {
		if err := os.Chtimes(p, aged, aged); err != nil {
			t.Fatalf("Chtimes(%s): %v", p, err)
		}
	}

	n, err := store.Sweep(24 * time.Hour)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 2 {
		t.Fatalf("Sweep removed %d, want 2 (aged cast + orphaned temp)", n)
	}
	if _, err := store.OpenCast(ctx, "fresh-run"); err != nil {
		t.Fatalf("fresh cast must survive: %v", err)
	}
	if _, err := store.OpenCast(ctx, "old-run"); !errors.Is(err, recording.ErrNotFound) {
		t.Fatalf("aged cast should be gone, got err %v", err)
	}
}

// ── handler tests ─────────────────────────────────────────────────────────────

func newTestRouter(store recording.Store) http.Handler {
	r := chi.NewRouter()
	// Mirror the mount point the assignment prescribes.
	r.Mount("/api/v1/runs/{id}/recording", recording.Handler(store))
	return r
}

func TestHandler_Serve(t *testing.T) {
	store, _ := recording.NewFSStore(t.TempDir())
	ctx := context.Background()
	const cast = `{"version":2}` + "\n" + `[1.0,"o","hi\r\n"]` + "\n"
	const runID = "abc123"
	_ = store.SaveCast(ctx, runID, strings.NewReader(cast))

	srv := httptest.NewServer(newTestRouter(store))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/runs/" + runID + "/recording/" + runID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "application/x-asciicast" {
		t.Errorf("Content-Type = %q, want application/x-asciicast", ct)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != cast {
		t.Errorf("body mismatch:\ngot  %q\nwant %q", got, cast)
	}
}

func TestHandler_NotFound(t *testing.T) {
	store, _ := recording.NewFSStore(t.TempDir())
	srv := httptest.NewServer(newTestRouter(store))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/runs/ghost/recording/ghost")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}
