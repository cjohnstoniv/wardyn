// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Handler returns an http.Handler that serves GET /{runID} as an asciicast
// stream (Content-Type: application/x-asciicast). Mount it under
// /api/v1/runs/{id}/recording in the wardynd router.
//
// The {runID} URL parameter is extracted via chi. A 404 is returned when no
// recording exists for that run. Errors from the store produce 500.
func Handler(store Store) http.Handler {
	r := chi.NewRouter()
	r.Get("/{runID}", func(w http.ResponseWriter, req *http.Request) {
		runID := chi.URLParam(req, "runID")
		rc, err := store.OpenCast(req.Context(), runID)
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
