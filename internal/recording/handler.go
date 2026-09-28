// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Authorizer decides whether req's caller may read the recording whose cast
// key names runIDPrefix (the run id, up to an optional "~<suffix>" session
// marker; see CastKey). It is Handler's ONLY authorization check — route auth
// middleware proves SOME authenticated human/admin, not WHICH run's
// recordings they may read. False collapses to the SAME 404 an absent
// recording gets: no existence oracle for "not yours" vs "never recorded".
type Authorizer func(r *http.Request, runIDPrefix string) bool

// Handler returns an http.Handler serving GET /{runID} as an asciicast
// stream (Content-Type: application/x-asciicast); mount it under
// /api/v1/runs/{id}/recording.
//
// The {runID} param is the CAST KEY: a bare run id (a batch run's cast) or a
// "<runID>~<suffix>" composite (an interactive attach session, or a future
// SSH session; see CastKey). SECURITY: this sub-route's {runID} is NOT the
// parent mount's {id} (the run this recording is fetched THROUGH) — the two
// are enforced equal (case-insensitive, on the run-id prefix) before
// authorize runs, so a caller can't request .../runs/A/recording/B to read
// run B's cast under an authorization scoped to run A. Both checks collapse
// to the same 404 (no existence oracle). Store errors produce 500. A bare run
// id is served joined across its tail-uploaded parts (OpenJoined).
func Handler(store Store, authorize Authorizer) http.Handler {
	r := chi.NewRouter()
	r.Get("/{runID}", func(w http.ResponseWriter, req *http.Request) {
		key := chi.URLParam(req, "runID")
		outerID := chi.URLParam(req, "id")
		prefix, _, _ := strings.Cut(key, castSep)
		if !strings.EqualFold(prefix, outerID) || !authorize(req, prefix) {
			http.Error(w, "recording not found", http.StatusNotFound)
			return
		}
		open := store.OpenCast
		if key == prefix {
			open = func(ctx context.Context, key string) (io.ReadCloser, error) { return OpenJoined(ctx, store, key) }
		}
		rc, err := open(req.Context(), key)
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "recording not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "recording store error", http.StatusInternalServerError)
			return
		}
		defer rc.Close()

		w.Header().Set("Content-Type", "application/x-asciicast")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		// Stream: ignore the copy error — client may disconnect mid-stream.
		_, _ = io.Copy(w, rc)
	})
	return r
}
