// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/google/uuid"
)

// handleQueryAudit returns audit events. The append-only audit log is the
// system of record and is never gated. With no run_id it returns the global
// SIEM-style feed (newest first across all runs) that the Audit view renders;
// with ?run_id= it returns that run's chronological trail.
//
// ponytail: compose-session history (Decision 7) is v1 audit-feed-only — the UI
// filters THIS response on `data.session_id`/`data.compose_session_id`
// client-side; there is no server-side query param for it. If that ever gets
// slow, the upgrade path is a store method (e.g. QueryAuditEventsBySession)
// backed by a `(data->>'session_id')` expression index, not a new table — the
// session id already lives in Data (JSONB), no migration to add the column.
func (s *Server) handleQueryAudit(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("run_id")
	if raw == "" {
		events, err := s.cfg.Store.QueryRecentAuditEvents(r.Context(), 0)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "query audit: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, events)
		return
	}
	runID, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid run_id")
		return
	}
	events, err := s.cfg.Store.QueryAuditEvents(r.Context(), runID, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "query audit: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, events)
}
