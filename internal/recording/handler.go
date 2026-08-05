// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Authorizer decides whether the caller of req may read the recording whose
// cast key names runIDPrefix — the run id up to an optional "~<suffix>"
// session marker (see CastKey). It is the ONLY authorization check Handler
// performs: the route's own auth middleware (wired by the caller, e.g.
// humanOrAdminAuth in internal/api) proves the caller is SOME authenticated
// human/admin, not WHICH run's recordings they may read. A false return is
// turned into the SAME 404 an absent recording gets (see Handler) — no
// existence oracle distinguishing "not yours" from "never recorded".
type Authorizer func(r *http.Request, runIDPrefix string) bool

// Handler returns an http.Handler that serves GET /{runID} as an asciicast
// stream (Content-Type: application/x-asciicast). Mount it under
// /api/v1/runs/{id}/recording in the wardynd router.
//
// The {runID} URL parameter is the CAST KEY (extracted via chi) — a bare run
// id (a batch run's cast) or a "<runID>~<suffix>" composite (an interactive
// attach session, or a future SSH session — see CastKey). SECURITY: this
// sub-route's own {runID} is NOT the same path segment as the PARENT mount's
// {id} (the run this recording is being fetched THROUGH) — the two are
// enforced equal (case-insensitively, on the run-id PREFIX) before authorize
// ever runs, so a caller cannot request .../runs/A/recording/B to read run
// B's cast under an authorization check scoped to run A. Both checks collapse
// to the SAME 404 "recording not found" a missing cast gets (no existence
// oracle). Errors from the store produce 500.
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
		rc, err := store.OpenCast(req.Context(), key)
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
