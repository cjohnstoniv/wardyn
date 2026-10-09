// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
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
	s, err := recording.NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DeleteRun(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	for _, authorized := range []bool{false, true} {
		for _, key := range []string{"run", "run~attach", "run~part-2"} {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/runs/run/recording/"+key, nil)
			newTestRouterWithAuth(s, func(*http.Request, string) bool { return authorized }).ServeHTTP(w, req)
			if w.Code != http.StatusNotFound || w.Body.String() != "recording not found\n" {
				t.Fatalf("erased replay response = %d %s", w.Code, w.Body)
			}
		}
	}
}
