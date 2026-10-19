// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// deleteRunContract is what a person's erasure relies on, for every store that
// can delete: it removes the run's bare cast and every composite of it, none of
// another run's (a prefix of the id is another run), and may be repeated.
func deleteRunContract(t *testing.T, s recording.Store) {
	t.Helper()
	ctx := t.Context()
	run, other := uuid.NewString(), uuid.NewString()
	longer := run + "9" // shares the run id as a prefix, but is another key
	for _, key := range []string{run, recording.CastKey(run, "sess"), recording.CastKey(run, recording.PartSuffix(1)), other, recording.CastKey(other, "sess"), longer} {
		if err := s.SaveCast(ctx, key, strings.NewReader("cast "+key)); err != nil {
			t.Fatal(err)
		}
	}
	d, ok := s.(recording.RunDeleter)
	if !ok {
		t.Fatalf("%T is not a RunDeleter", s)
	}
	n, err := d.DeleteRun(ctx, run)
	if err != nil || n != 3 {
		t.Fatalf("DeleteRun = %d, %v, want the 3 casts of the run", n, err)
	}
	for _, key := range []string{run, recording.CastKey(run, "sess"), recording.CastKey(run, recording.PartSuffix(1))} {
		if _, err := s.OpenCast(ctx, key); !errors.Is(err, recording.ErrErased) {
			t.Errorf("cast %s after DeleteRun: %v, want erased", key, err)
		}
	}
	for _, key := range []string{other, recording.CastKey(other, "sess"), longer} {
		rc, err := s.OpenCast(ctx, key)
		if err != nil {
			t.Errorf("cast %s of another run was deleted: %v", key, err)
			continue
		}
		_ = rc.Close()
	}
	if n, err := d.DeleteRun(ctx, run); err != nil || n != 0 {
		t.Errorf("a second DeleteRun = %d, %v, want nothing and no error", n, err)
	}
	if _, err := d.DeleteRun(ctx, "../escape"); err == nil {
		t.Error("DeleteRun accepted a path-traversal id")
	}
}

func TestFSStoreDeleteRun(t *testing.T) {
	s, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deleteRunContract(t, s)
}

func TestHandler_ErasedRecordingRemainsHidden(t *testing.T) {
	const run = "0a000000-0000-4000-8000-000000000010"
	s, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	for _, authorized := range []bool{false, true} {
		for _, key := range []string{run, run + "~attach", run + "~part-2"} {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/"+run+"/recording/"+key, nil)
			newTestRouterWithAuth(s, func(*http.Request, string) bool { return authorized }).ServeHTTP(w, req)
			if w.Code != http.StatusNotFound || w.Body.String() != "recording not found\n" {
				t.Fatalf("erased replay response = %d %s", w.Code, w.Body)
			}
		}
	}
}

// A save that was killed before its rename leaves its staging file holding the run's bytes; erasing
// the run removes it (#2786), and erasing a composite key leaves other keys' staging files alone.
func TestFSStoreDeleteRun_RemovesTheRunsStagingFiles(t *testing.T) {
	root := t.TempDir()
	s, err := recording.NewFSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	run, other := uuid.NewString(), uuid.NewString()
	stage := func(run string) string {
		name := filepath.Join(root, ".tmp-cast-"+run+"~"+uuid.NewString())
		if err := os.WriteFile(name, []byte("partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		return name
	}
	mine, theirs := stage(run), stage(other)
	if _, err := s.DeleteRun(t.Context(), recording.CastKey(run, "sess")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatalf("a composite erase removed the run's staging file: %v", err)
	}
	if _, err := s.DeleteRun(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the erased run's staging file survives: %v", err)
	}
	if _, err := os.Stat(theirs); err != nil {
		t.Errorf("another run's staging file was removed: %v", err)
	}
}

// DeleteRuns is the many-run DeleteRun: same fence, same removals, one scan (#2788).
func TestFSStoreDeleteRuns(t *testing.T) {
	root := t.TempDir()
	s, err := recording.NewFSStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	a, b, keep := uuid.NewString(), uuid.NewString(), uuid.NewString()
	for _, key := range []string{a, recording.CastKey(a, "sess"), b, keep, a + "9"} {
		if err := s.SaveCast(ctx, key, strings.NewReader("cast "+key)); err != nil {
			t.Fatal(err)
		}
	}
	staged := filepath.Join(root, ".tmp-cast-"+b+"~"+uuid.NewString())
	if err := os.WriteFile(staged, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteRuns(ctx, []string{a, b})
	if err != nil || n != 3 {
		t.Fatalf("DeleteRuns = %d, %v, want the 3 casts of the two runs", n, err)
	}
	for _, key := range []string{a, recording.CastKey(a, "sess"), b} {
		if _, err := s.OpenCast(ctx, key); !errors.Is(err, recording.ErrErased) {
			t.Errorf("cast %s after DeleteRuns: %v, want erased", key, err)
		}
	}
	for _, key := range []string{keep, a + "9"} {
		rc, err := s.OpenCast(ctx, key)
		if err != nil {
			t.Errorf("cast %s of an unlisted run was deleted: %v", key, err)
			continue
		}
		_ = rc.Close()
	}
	if _, err := os.Stat(staged); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a listed run's staging file survives: %v", err)
	}
	if _, err := s.DeleteRuns(ctx, []string{recording.CastKey(a, "sess")}); err == nil {
		t.Error("DeleteRuns accepted a composite key")
	}
	if n, err := s.DeleteRuns(ctx, nil); err != nil || n != 0 {
		t.Errorf("DeleteRuns(nil) = %d, %v", n, err)
	}
}
