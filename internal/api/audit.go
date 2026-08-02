// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// handleQueryAudit returns audit events. The append-only audit log is the
// system of record and is never gated. With no run_id it returns the global
// SIEM-style feed (newest first across all runs) that the Audit view renders;
// with ?run_id= it returns that run's chronological trail. Either can be
// narrowed by ?since=&until=&action=&action_prefix=&actor_type=&outcome=
// (see parseAuditFilter) — the corp-operator questions ("what happened between
// 14:00 and 16:00", "every secret.write this quarter", "every failure") that
// more rows never answer, because the answer is 40 events inside 200k.
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
	filter, ok := parseAuditFilter(w, r)
	if !ok {
		return
	}
	raw := r.URL.Query().Get("run_id")
	if raw == "" {
		page, ok := parseListPage(w, r, auditGlobalDefaultLimit)
		if !ok {
			return
		}
		var pageFn func(store.Page) ([]types.AuditEvent, error)
		if pager != nil {
			pageFn = func(p store.Page) ([]types.AuditEvent, error) {
				if filter.IsZero() {
					return pager.QueryRecentAuditEventsPage(r.Context(), p)
				}
				return pager.QueryAuditEventsFilteredPage(r.Context(), nil, filter, p)
			}
		}
		// The fetch-all fallback MUST apply the same predicate: filtering only on
		// the pager path would answer a filtered request with unfiltered events.
		servePage(w, page, pageFn, func() ([]types.AuditEvent, error) {
			all, err := s.cfg.Store.QueryRecentAuditEvents(r.Context(), 0)
			return filter.Keep(all), err
		})
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
			if filter.IsZero() {
				return pager.QueryAuditEventsPage(r.Context(), runID, p)
			}
			return pager.QueryAuditEventsFilteredPage(r.Context(), &runID, filter, p)
		}
	}
	servePage(w, page, pageFn, func() ([]types.AuditEvent, error) {
		all, err := s.cfg.Store.QueryAuditEvents(r.Context(), runID, 0)
		return filter.Keep(all), err
	})
}

// parseAuditFilter reads the optional narrowing predicates off the query string
// (?since=&until=&action=&action_prefix=&actor_type=&outcome=), writing a 400 and
// returning ok=false on a malformed value. All are additive and optional; none
// set is the zero filter, which changes nothing about the query taken.
// Timestamps are RFC3339, the same encoding the audit rows are served in.
func parseAuditFilter(w http.ResponseWriter, r *http.Request) (store.AuditFilter, bool) {
	q := r.URL.Query()
	f := store.AuditFilter{
		Action:       q.Get("action"),
		ActionPrefix: q.Get("action_prefix"),
		ActorType:    types.ActorType(q.Get("actor_type")),
		Outcome:      q.Get("outcome"),
	}
	for _, p := range []struct {
		name string
		dst  *time.Time
	}{{"since", &f.Since}, {"until", &f.Until}} {
		v := q.Get(p.name)
		if v == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid "+p.name+" (want an RFC3339 timestamp)")
			return store.AuditFilter{}, false
		}
		*p.dst = t
	}
	switch f.ActorType {
	case "", types.ActorHuman, types.ActorAgent, types.ActorSystem:
	default:
		writeError(w, http.StatusBadRequest, "invalid actor_type (want human, agent, or system)")
		return store.AuditFilter{}, false
	}
	return f, true
}
