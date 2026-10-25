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
// A successful check of another person's run (an operator's) writes a recording.read row whose data names the
// operation chain_check; a check of one's own run writes none.
// A broken range answers 200 with ok=false, as the whole sweep does. One check walks at a time per
// replica; a second is a 503 recording_chain_busy.
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
	if key == "" {
		writeErrorReason(w, http.StatusBadRequest, reasonRecordingChainKeyRequired, "key (the recording's key) is required")
		return
	}
	from, err := time.Parse(time.RFC3339Nano, r.URL.Query().Get("from"))
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonAuditInvalidTimestampParam, "from (RFC 3339) is required")
		return
	}
	// The walk is the whole sweep's work over a narrower window, open to every run owner, so it is
	// single-flight per replica like the sweep (audit.go) and refused like a busy replay
	// (recording.Handler): 503 with Retry-After, never queued.
	if !s.recordingChainWalk.TryLock() {
		w.Header().Set("Retry-After", "5")
		writeErrorReason(w, http.StatusServiceUnavailable, reasonRecordingChainBusy, "another recording's chain check is running; retry shortly")
		return
	}
	defer s.recordingChainWalk.Unlock()
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
	// A check reads the recording's metadata, not its content; an operator's check of another person's run is
	// audited under the recording read's own action, marked as the check, as a replay of it is. An owner's
	// check of their own run is not.
	if run.CreatedBy != principalFromRequest(r) {
		actorType, actor := actorFromRequest(r)
		s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, actor, auditRecordingRead, key, "success", mustJSON(map[string]string{"operation": recordingChainOperation})))
	}
	writeJSON(w, http.StatusOK, rng)
}

// recordingChainOperation marks a recording.read row as the chain check, a metadata read, not a replay.
const recordingChainOperation = "chain_check"

// recordingNotFound is recording.Handler's refusal, the same status and body.
func recordingNotFound(w http.ResponseWriter) {
	http.Error(w, "recording not found", http.StatusNotFound)
}
