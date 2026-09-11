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

// TestNew_DefaultIsPG pins the S3 default flip: an empty selector must
// resolve to "pg", not "fs", and "fs" must remain explicitly selectable so
// WARDYN_RECORDING_STORE=fs still recovers the legacy per-pod store. The pg
// constructor needs a live pool to fully construct (see the WARDYN_TEST_PG
// -gated pgstore_pg_test.go for that), but DEFAULT RESOLUTION itself needs no
// database: New("", ...) with no pool hits pg's constructor and observes ITS
// nil-pool guard — proving "" resolved to pg, not fs (which would have
// happily returned a nil Store for an empty Dir instead of erroring).
// 0.7.1: "off" is the one spelling of "no recording" an environment can carry
// now that an empty env value keeps the compiled default (FlagEnv, F011/F067).
func TestNew_OffIsDisabled(t *testing.T) {
	s, err := recording.New("off", recording.Deps{})
	if err != nil || s != nil {
		t.Fatalf(`New("off", no deps) = %v, %v; want a nil Store and no error`, s, err)
	}
}

func TestNew_DefaultIsPG(t *testing.T) {
	if _, err := recording.New("", recording.Deps{}); err == nil {
		t.Fatal(`New("", no pool) succeeded; want the pg constructor's nil-pool error (proves "" resolves to pg)`)
	}
	s, err := recording.New("fs", recording.Deps{Dir: t.TempDir()})
	if err != nil || s == nil {
		t.Fatalf(`New("fs", ...) = %v, %v; "fs" must remain explicitly selectable`, s, err)
	}
}

// Traversal/invalid-key rejection lives in the SHARED conformance suite
// (recordingtest.RunConformance's rejects_invalid_keys), which fs_conformance_test.go
// runs against FSStore and pgstore_pg_test.go runs against PGStore — the two
// stores must reject the same keys, which an fs-only test could not pin.

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

// allowAllAuthorizer is the test stub for recording.Authorizer: the mechanics
// under test here are the Handler's own route/store plumbing and its
// outer-id/inner-prefix enforcement, not any particular authorization
// DECISION — internal/api's own recordingAuthorizer (and its ownership rules)
// is exercised at that layer instead.
func allowAllAuthorizer(*http.Request, string) bool { return true }

// newTestRouter mounts Handler with the given authorizer (allowAllAuthorizer
// by default via the wrapper below) so tests can also exercise a denying one.
func newTestRouterWithAuth(store recording.Store, authorize recording.Authorizer) http.Handler {
	r := chi.NewRouter()
	// Mirror the mount point the assignment prescribes.
	r.Mount("/api/v1/runs/{id}/recording", recording.Handler(store, authorize))
	return r
}

func newTestRouter(store recording.Store) http.Handler {
	return newTestRouterWithAuth(store, allowAllAuthorizer)
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

// TestHandler_OuterInnerMismatch pins item 4: the recording exists (an
// allow-all authorizer would happily serve it), but the OUTER mount {id}
// (naming which run this request claims to be about) does not match the
// INNER {runID}'s run-id PREFIX (the cast key actually being opened) — e.g. a
// caller authorized for run A's recordings requesting
// .../runs/A/recording/B. Handler must refuse this BEFORE ever calling
// authorize or the store, with the same 404 a missing cast gets.
func TestHandler_OuterInnerMismatch(t *testing.T) {
	store, _ := recording.NewFSStore(t.TempDir())
	ctx := context.Background()
	const cast = `{"version":2}` + "\n"
	const runB = "run-b"
	_ = store.SaveCast(ctx, runB, strings.NewReader(cast))

	var authorizeCalled bool
	authorize := func(*http.Request, string) bool { authorizeCalled = true; return true }
	srv := httptest.NewServer(newTestRouterWithAuth(store, authorize))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/runs/run-a/recording/" + runB)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("outer/inner mismatch: status = %d, want 404 (run-b's cast exists and an allow-all authorizer is wired)", resp.StatusCode)
	}
	if authorizeCalled {
		t.Error("authorize was called despite the outer/inner id mismatch — the cheap string check must short-circuit first")
	}
}

// TestHandler_AuthorizerDenies pins that a denying Authorizer produces the
// SAME 404 an absent recording gets — no distinguishable status/body between
// "not yours" and "never recorded" (no existence oracle).
func TestHandler_AuthorizerDenies(t *testing.T) {
	store, _ := recording.NewFSStore(t.TempDir())
	ctx := context.Background()
	const runID = "owned-by-someone-else"
	_ = store.SaveCast(ctx, runID, strings.NewReader(`{"version":2}`+"\n"))

	deny := func(*http.Request, string) bool { return false }
	srv := httptest.NewServer(newTestRouterWithAuth(store, deny))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/runs/" + runID + "/recording/" + runID)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("denied authorizer: status = %d, want 404", resp.StatusCode)
	}
}
