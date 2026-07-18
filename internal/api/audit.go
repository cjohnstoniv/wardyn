// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// handleQueryAudit returns audit events. The append-only audit log is the
// system of record and is never gated. With no run_id it returns the global
// SIEM-style feed (newest first across all runs) that the Audit view renders;
// with ?run_id= it returns that run's chronological trail.
//
// compose-session history (Decision 7) is v1 audit-feed-only — the UI
// filters THIS response on `data.session_id`/`data.compose_session_id`
// client-side; there is no server-side query param for it. If that ever gets
// slow, the upgrade path is a store method (e.g. QueryAuditEventsBySession)
// backed by a `(data->>'session_id')` expression index, not a new table — the
// session id already lives in Data (JSONB), no migration to add the column.
// Audit-feed default page sizes. Unlike the list endpoints (defaultListLimit),
// an unparameterised /audit keeps the historical caps the store applied (per-run
// 1000 / global 500) so the UI audit trail and the CLI exit-code lookup
// (docs/sdk.md: run.complete -> .data.exit_code) see the same window they did
// before pagination. A caller pages past the cap with ?limit=&offset=; a
// truncated page sets X-Wardyn-Truncated (per-run stays ASC, so ?offset= walks
// forward to the newest events).
const (
	auditPerRunDefaultLimit = 1000
	auditGlobalDefaultLimit = 500
)

func (s *Server) handleQueryAudit(w http.ResponseWriter, r *http.Request) {
	pager, _ := s.cfg.Store.(store.Pager)
	raw := r.URL.Query().Get("run_id")
	if raw == "" {
		page, ok := parseListPage(w, r, auditGlobalDefaultLimit)
		if !ok {
			return
		}
		var pageFn func(store.Page) ([]types.AuditEvent, error)
		if pager != nil {
			pageFn = func(p store.Page) ([]types.AuditEvent, error) {
				return pager.QueryRecentAuditEventsPage(r.Context(), p)
			}
		}
		servePage(w, page, pageFn, func() ([]types.AuditEvent, error) { return s.cfg.Store.QueryRecentAuditEvents(r.Context(), 0) })
		return
	}
	runID, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid run_id")
		return
	}
	page, ok := parseListPage(w, r, auditPerRunDefaultLimit)
	if !ok {
		return
	}
	var pageFn func(store.Page) ([]types.AuditEvent, error)
	if pager != nil {
		pageFn = func(p store.Page) ([]types.AuditEvent, error) {
			return pager.QueryAuditEventsPage(r.Context(), runID, p)
		}
	}
	servePage(w, page, pageFn, func() ([]types.AuditEvent, error) { return s.cfg.Store.QueryAuditEvents(r.Context(), runID, 0) })
}
