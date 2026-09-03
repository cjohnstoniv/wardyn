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

// auditScope resolves the run scope BOTH audit reads share and applies the
// member gate identically to each. It returns (scope, ok): ok==false means the
// response is already decided and the handler must simply return — either
// auditScope wrote the 400 for a malformed run_id, or the caller's own
// writeEmpty ran for the member collapse. scope is nil for the global feed and
// &runID for one run's trail.
//
// The ORDER here is load-bearing and is the one both handlers spelled: the
// run_id is parsed BEFORE the role is consulted, so a malformed run_id is a 400
// regardless of who asks (an input-shape error, never an authz one).
//
// Members are then scoped to ?run_id= of a run THEY created (item 2). No
// run_id, a well-formed but unowned run_id, and an unknown run_id ALL collapse
// to the SAME empty success — /audit is a collection endpoint, so the
// no-existence-oracle property here is an empty 200, never the 404 the
// single-resource /runs/{id} routes use. writeEmpty is what "empty" looks like
// in each caller's media type (a JSON [] for the query, a zero-line NDJSON body
// for the export) and is the ONLY thing the two gates ever differed on.
//
// isSecurityOperator, not isOperator: reading the org-wide audit log (and
// verifying its hash chain) is the security admin's job description — a tier
// that governs approvals and permissions without being able to read what
// happened is not a security tier at all.
func (s *Server) auditScope(w http.ResponseWriter, r *http.Request, writeEmpty func()) (*uuid.UUID, bool) {
	raw := r.URL.Query().Get("run_id")
	var runID uuid.UUID
	if raw != "" {
		var err error
		if runID, err = uuid.Parse(raw); err != nil {
			writeError(w, http.StatusBadRequest, "invalid run_id")
			return nil, false
		}
	}
	if !s.isSecurityOperator(r.Context()) {
		owned := raw != ""
		if owned {
			run, gerr := s.cfg.Store.GetRun(r.Context(), runID)
			owned = gerr == nil && run.CreatedBy == principalFromRequest(r)
		}
		if !owned {
			writeEmpty()
			return nil, false
		}
	}
	if raw == "" {
		return nil, true
	}
	return &runID, true
}

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
	scope, ok := s.auditScope(w, r, func() { writeJSON(w, http.StatusOK, []types.AuditEvent{}) })
	if !ok {
		return
	}
	// The historical caps differ by shape: a run's whole trail is worth more
	// rows than the org-wide feed's first screen.
	limit := auditGlobalDefaultLimit
	if scope != nil {
		limit = auditPerRunDefaultLimit
	}
	page, ok := parseListPage(w, r, limit)
	if !ok {
		return
	}
	var pageFn func(store.Page) ([]types.AuditEvent, error)
	if pager != nil {
		pageFn = func(p store.Page) ([]types.AuditEvent, error) {
			switch {
			case !filter.IsZero():
				// One filtered query serves both shapes: scope is exactly the
				// nil/&runID this argument already took.
				return pager.QueryAuditEventsFilteredPage(r.Context(), scope, filter, p)
			case scope == nil:
				return pager.QueryRecentAuditEventsPage(r.Context(), p)
			default:
				return pager.QueryAuditEventsPage(r.Context(), *scope, p)
			}
		}
	}
	// The fetch-all fallback MUST apply the same predicate: filtering only on
	// the pager path would answer a filtered request with unfiltered events.
	servePage(w, page, pageFn, func() ([]types.AuditEvent, error) {
		if scope == nil {
			all, err := s.cfg.Store.QueryRecentAuditEvents(r.Context(), 0)
			return filter.Keep(all), err
		}
		all, err := s.cfg.Store.QueryAuditEvents(r.Context(), *scope, 0)
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
	// Same gate as handleQueryAudit by construction, not by copy — see the
	// member paragraph above, which auditScope is now the single home of.
	scope, ok := s.auditScope(w, r, func() { w.Header().Set("Content-Type", "application/x-ndjson") })
	if !ok {
		return
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

// handleVerifyAuditChain runs the audit hash-chain sweep (migration 0047) and
// reports what it found. Operator-only (registered on routes.go's operatorOnly
// group): a member reading it would learn the deployment's total audit volume
// across every other user's runs — the same disclosure that keeps /metrics
// admin-gated.
//
// This is the ONLY surface that verifies the chain, and it is deliberately
// PULL-based. wardynd does NOT verify at boot: the sweep re-hashes every chained
// row, so it costs O(whole audit log) on a check whose answer is "fine" on every
// start that is not an incident. An operator — or a cron hitting this route with
// the admin bearer — chooses when to pay it.
//
// A BROKEN chain answers 200 with ok=false, not 5xx: the sweep SUCCEEDED, it
// just found something. 5xx is reserved for "the sweep could not run", so an
// alert on ok=false never fires on a store hiccup and a store hiccup is never
// mistaken for tampering.
func (s *Server) handleVerifyAuditChain(w http.ResponseWriter, r *http.Request) {
	v, ok := s.cfg.Store.(store.AuditChainVerifier)
	if !ok {
		writeError(w, http.StatusNotImplemented, "audit chain verification requires the Postgres store backend")
		return
	}
	// ONE SWEEP AT A TIME. The sweep re-hashes every row of a table that can
	// never be pruned, so N concurrent GETs are N full passes, each pinning a
	// pool connection for the duration — and a cron or a retrying client stacks
	// them without anyone deciding to. Refusing the second is better than
	// serving it slowly: the answer it would return is the answer the in-flight
	// sweep is already computing, and 429 tells a retrying client exactly what
	// to do. Deliberately NOT a deadline as well: this is the endpoint an
	// operator reaches for during a suspected tamper incident, and a fixed
	// timeout would put a ceiling on how large a log can be verified AT ALL,
	// which is the one thing it must not do. The paged sweep checks the request
	// context between pages instead, so a client that goes away stops the work —
	// which the single materializing statement it replaced could not.
	if !s.auditChainSweep.TryLock() {
		w.Header().Set("Retry-After", "30")
		writeError(w, http.StatusTooManyRequests, "an audit chain verification is already running; retry when it finishes")
		return
	}
	defer s.auditChainSweep.Unlock()

	st, err := v.VerifyAuditChain(r.Context())
	if err != nil {
		slog.ErrorContext(r.Context(), "wardyn: audit chain sweep failed", slog.Any("err", err))
		writeError(w, http.StatusInternalServerError, "audit chain sweep failed")
		return
	}
	if !st.OK {
		// Loud on the daemon's own logger too: whoever is paged is not
		// necessarily whoever ran the sweep.
		slog.ErrorContext(r.Context(), "wardyn: AUDIT CHAIN BROKEN",
			slog.Int64("broken_seq", st.BrokenSeq),
			slog.String("reason", st.Reason))
	}
	writeJSON(w, http.StatusOK, st)
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
