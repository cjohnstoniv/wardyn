// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// auditExportPageSize bounds each internal page handleExportAudit walks. The
// export itself has NO offset cap — it pages until the store is exhausted — but
// reads in bounded slices so a quarter of history is never buffered whole.
const auditExportPageSize = 1000

// handleQueryAudit returns audit events. The append-only audit log is the
// system of record and is never gated. With no run_id it returns the global
// SIEM-style feed (newest first across all runs) that the Audit view renders;
// with ?run_id= it returns that run's chronological trail. Either can be
// narrowed by ?since=&until=&action=&action_prefix=&actor_type=&outcome=
// (see parseAuditFilter) — the corp-operator questions ("what happened between
// 14:00 and 16:00", "every secret.write this quarter", "every failure") that
// more rows never answer, because the answer is 40 events inside 200k.
//
// Session history is v1 audit-feed-only — a client filters THIS response on
// `data.session_id` itself; there is no server-side query param for it. (The
// `data.compose_session_id` half went with the AI Run Composer in 0.5.) If that
// ever gets
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
	// Parsed once, up front, and shared by both the member gate below and the
	// per-run branch further down — a malformed run_id 400s regardless of role
	// (an input-shape error, never an authz one).
	var runID uuid.UUID
	if raw != "" {
		var err error
		if runID, err = uuid.Parse(raw); err != nil {
			writeError(w, http.StatusBadRequest, "invalid run_id")
			return
		}
	}
	if !s.isOperator(r.Context()) {
		// Members: audit is scoped to ?run_id= of a run THEY created (item 2). No
		// run_id, or a well-formed but unowned/unknown run_id, all collapse to
		// the SAME empty result — /audit is a collection endpoint, so the
		// no-existence-oracle property here is an empty 200 list, never the 404
		// the single-resource /runs/{id} routes use.
		owned := raw != ""
		if owned {
			run, gerr := s.cfg.Store.GetRun(r.Context(), runID)
			owned = gerr == nil && run.CreatedBy == principalFromRequest(r)
		}
		if !owned {
			writeJSON(w, http.StatusOK, []types.AuditEvent{})
			return
		}
	}
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
	// runID was already parsed (and validated) above.
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

// handleExportAudit streams the audit feed as newline-delimited JSON (one event
// per line, application/x-ndjson), applying the SAME filters (parseAuditFilter,
// incl. ?actor=) and member scoping as handleQueryAudit but with NO offset cap:
// it pages the store until exhausted, so a per-principal evidence pull —
// "everything alice@corp did in Q3", the vendor/compliance question the finding
// names (D6) — is ONE request no matter how many events it spans. handleQueryAudit
// stays the capped, paginated, UI-facing read; this is the bulk export beside it.
//
// A member is scoped exactly as in handleQueryAudit: only ?run_id= of a run they
// created, and an unowned/absent run_id yields an empty (200, zero-line) export —
// the same no-existence-oracle collapse a collection endpoint uses.
//
// Requires a Pager backend (store.PG is one); a non-Pager store (test fakes with
// no pager) gets 501 rather than a silently-capped read. A mid-stream store error
// after the header is sent can only stop and log — the NDJSON is then a truncated
// prefix, which a consumer detects by the request not ending cleanly.
func (s *Server) handleExportAudit(w http.ResponseWriter, r *http.Request) {
	pager, ok := s.cfg.Store.(store.Pager)
	if !ok {
		writeError(w, http.StatusNotImplemented, "audit export requires a paging store backend")
		return
	}
	filter, ok := parseAuditFilter(w, r)
	if !ok {
		return
	}
	raw := r.URL.Query().Get("run_id")
	var runID uuid.UUID
	if raw != "" {
		var err error
		if runID, err = uuid.Parse(raw); err != nil {
			writeError(w, http.StatusBadRequest, "invalid run_id")
			return
		}
	}
	if !s.isOperator(r.Context()) {
		owned := raw != ""
		if owned {
			run, gerr := s.cfg.Store.GetRun(r.Context(), runID)
			owned = gerr == nil && run.CreatedBy == principalFromRequest(r)
		}
		if !owned {
			// Empty export, no oracle: an unowned/absent run_id is a 200 with no
			// lines, exactly as handleQueryAudit returns an empty list.
			w.Header().Set("Content-Type", "application/x-ndjson")
			return
		}
	}
	var scope *uuid.UUID
	if raw != "" {
		scope = &runID
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	enc := json.NewEncoder(w)
	offset := 0
	for {
		page, err := pager.QueryAuditEventsFilteredPage(r.Context(), scope, filter,
			store.Page{Limit: auditExportPageSize, Offset: offset})
		if err != nil {
			// Header (200) is already committed; a truncated NDJSON prefix is all we
			// can leave. Log so the operator can tell a partial export from a whole one.
			slog.ErrorContext(r.Context(), "wardyn: audit export page failed mid-stream",
				slog.Int("offset", offset), slog.Any("err", err))
			return
		}
		for i := range page {
			if err := enc.Encode(page[i]); err != nil {
				return // client hung up
			}
		}
		if len(page) < auditExportPageSize {
			return
		}
		offset += len(page)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
}

// parseAuditFilter reads the optional narrowing predicates off the query string
// (?since=&until=&action=&action_prefix=&actor=&actor_type=&outcome=), writing a
// 400 and returning ok=false on a malformed value. ?actor= (D6) is the exact
// principal ("everything developer X did"). All are additive and optional; none
// set is the zero filter, which changes nothing about the query taken.
// Timestamps are RFC3339, the same encoding the audit rows are served in.
func parseAuditFilter(w http.ResponseWriter, r *http.Request) (store.AuditFilter, bool) {
	q := r.URL.Query()
	f := store.AuditFilter{
		Action:       q.Get("action"),
		ActionPrefix: q.Get("action_prefix"),
		Actor:        q.Get("actor"),
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
