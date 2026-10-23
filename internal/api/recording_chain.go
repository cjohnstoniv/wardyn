// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// handleRecordingChain answers GET /api/v1/runs/{id}/recording-chain?key=&from=: whether the audit
// chain verifies over a desktop recording's stretch of the log, from the run's first row at or
// after from (the recording's started_at) to the recording's desktop.recording.write row for key
// (#2968, board M7-Replay's "Tamper-evident log"). Whoever may read the run's recording may ask
// (recordingReader). Anyone else, a run that does not exist and a key with no recording all get the
// recording read's own 404 (recording.Handler), byte for byte, so the check is no existence oracle;
// a refused caller on a run that exists leaves the not_owner row recordingAuthorizer leaves.
// A broken range answers 200 with ok=false, as the whole sweep does.
func (s *Server) handleRecordingChain(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		recordingNotFound(w)
		return
	}
	run, err := s.cfg.Store.GetRun(r.Context(), id)
	if err != nil {
		recordingNotFound(w)
		return
	}
	if !s.recordingReader(r, run) {
		s.recordRefusal(r.Context(), r, authz.Deny(authz.ReasonNotOwner, id.String(), "").OnRun(id))
		recordingNotFound(w)
		return
	}
	v, ok := s.cfg.Store.(store.AuditChainRangeVerifier)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonAuditChainVerifyStoreUnavailable, "audit chain verification requires the Postgres store backend")
		return
	}
	key := r.URL.Query().Get("key")
	from, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("from"))
	if err != nil || key == "" {
		writeErrorReason(w, http.StatusBadRequest, reasonAuditInvalidTimestampParam, "key and from (RFC 3339) are required")
		return
	}
	rng, found, err := v.VerifyRecordingAuditRange(r.Context(), run.ID, key, from)
	if err != nil {
		slog.ErrorContext(r.Context(), "wardyn: recording audit range check failed", slog.Any("err", err))
		writeErrorReason(w, http.StatusInternalServerError, reasonAuditChainSweepFailed, "audit chain check failed")
		return
	}
	if !found {
		recordingNotFound(w)
		return
	}
	if !rng.OK {
		slog.ErrorContext(r.Context(), "wardyn: AUDIT CHAIN BROKEN in a recording's range",
			slog.Int64("broken_seq", rng.BrokenSeq), slog.String("reason", rng.Reason))
	}
	writeJSON(w, http.StatusOK, rng)
}

// recordingNotFound is recording.Handler's refusal, the same status and body.
func recordingNotFound(w http.ResponseWriter) {
	http.Error(w, "recording not found", http.StatusNotFound)
}
