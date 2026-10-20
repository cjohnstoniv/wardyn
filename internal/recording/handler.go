// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
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
// to the same 404 (no existence oracle). Store errors produce 500, and a store with every read slot
// held (ErrReadBusy) a 503 with Retry-After. A bare run
// id is served joined across its tail-uploaded parts (OpenJoined).
//
// A key servableKey or canonicalRunID refuses gets that same 404 before
// authorize or the store is asked anything.
func Handler(store Store, authorize Authorizer) http.Handler {
	r := chi.NewRouter()
	r.Get("/{runID}", func(w http.ResponseWriter, req *http.Request) {
		key := chi.URLParam(req, "runID")
		outerID := chi.URLParam(req, "id")
		prefix, _, _ := strings.Cut(key, castSep)
		if !servableKey(key) || !canonicalRunID(prefix) || !strings.EqualFold(prefix, outerID) || !authorize(req, prefix) {
			http.Error(w, "recording not found", http.StatusNotFound)
			return
		}
		open := store.OpenCast
		if key == prefix {
			open = func(ctx context.Context, key string) (io.ReadCloser, error) { return OpenJoined(ctx, store, key) }
		}
		rc, err := open(req.Context(), key)
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrErased) {
			http.Error(w, "recording not found", http.StatusNotFound)
			return
		}
		if errors.Is(err, ErrReadBusy) {
			w.Header().Set("Retry-After", "5")
			http.Error(w, "too many recordings are being replayed; retry shortly", http.StatusServiceUnavailable)
			return
		}
		if err != nil {
			slog.ErrorContext(req.Context(), "recording: replay open failed", "run", prefix, "error", err)
			http.Error(w, "recording store error", http.StatusInternalServerError)
			return
		}
		defer rc.Close()

		w.Header().Set("Content-Type", "application/x-asciicast")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		// Stream: ignore the copy error — client may disconnect mid-stream. A STORE read
		// error part-way would end the body short of its tail after a 200, and a short cast is
		// still a well-formed one; log it and abort the response so the client sees a failed transfer.
		var readErr error
		rctl := http.NewResponseController(w)
		// With no server WriteTimeout nothing else resets the deadline, and a kept-alive connection would carry it into its next request.
		defer func() { _ = rctl.SetWriteDeadline(time.Time{}) }()
		_, _ = io.Copy(deadlineWriter{w: w, rc: rctl}, readerFunc(func(p []byte) (int, error) {
			n, err := rc.Read(p)
			if err != nil && !errors.Is(err, io.EOF) {
				readErr = err
			}
			return n, err
		}))
		if readErr != nil {
			slog.ErrorContext(req.Context(), "recording: replay read failed part-way", "run", prefix, "error", readErr)
			panic(http.ErrAbortHandler)
		}
	})
	return r
}

// replayWriteTimeout bounds each write of a replay to its client. The server sets no write timeout, and an
// open cast holds one of the few read slots until the reader is closed, so a client that stops reading
// (a zero TCP window) would pin the slot, and its buffered payload, indefinitely. A write that outlasts
// the bound fails, the copy ends, and the deferred Close frees the slot.
var replayWriteTimeout = 30 * time.Second

// deadlineWriter arms a fresh write deadline before every write. A writer that does not support
// deadlines (a recorder) is written to without one.
type deadlineWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (d deadlineWriter) Write(p []byte) (int, error) {
	_ = d.rc.SetWriteDeadline(time.Now().Add(replayWriteTimeout))
	return d.w.Write(p)
}

// servableKey reports whether key may be asked of the store at all. Handler
// checks it itself rather than leaning on the store's validKey: a store
// refusal is not a not-found, so it would reach the caller as a 500.
func servableKey(key string) bool {
	return validKey(key) == nil
}

// canonicalRunID reports whether prefix is a run id in its one canonical
// spelling. uuid.Parse also takes upper case, braces, "urn:uuid:" and bare
// hex, and the fs store takes a lock file named after each spelling it sees.
func canonicalRunID(prefix string) bool {
	id, err := uuid.Parse(prefix)
	return err == nil && id.String() == prefix
}

// readerFunc adapts a function to io.Reader.
type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
