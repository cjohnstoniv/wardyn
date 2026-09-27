// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The opt-in GET /runs query surface beyond ?limit=&offset= (#1197 L1a):
// view/owner/status/ended_within/include_killed/workspace/q. Split out of
// runs_policy.go so handleListRuns's own branch (any of these present -> the
// filtered path; none present -> byte-identical pre-#1197 behaviour) stays a
// short read.
package api

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// hasRunsListFilterParams reports whether the caller sent any opt-in
// landing-page param. handleListRuns takes the filtered branch when so —
// never on the mere presence of ?limit=/?offset=, which parseListPage already
// owns and every existing caller (the Recordings pager, the CLI, the SDK)
// already sends. Spelled as literal .Has() calls, not a loop over a name
// list: authz_query_id_test.go's query-parameter guard resolves a parameter
// name statically and cannot see one read through a loop variable.
func hasRunsListFilterParams(q url.Values) bool {
	return q.Has("view") || q.Has("owner") || q.Has("status") ||
		q.Has("ended_within") || q.Has("include_killed") || q.Has("workspace") || q.Has("q")
}

// maxRunsListQueryLen bounds ?q=: it is parameterised (store_runs_filtered.go
// escapes it before binding), so it is not injectable, but an unbounded value
// is still an ILIKE scan across four columns over a caller-controlled length.
const maxRunsListQueryLen = 400

// endedWithinDurations maps ?ended_within= to a window; "all" and the zero
// value both mean unbounded (store.RunFilter's <= 0 convention).
var endedWithinDurations = map[string]time.Duration{
	"":    0,
	"all": 0,
	"24h": 24 * time.Hour,
	"7d":  7 * 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

// runsListStatuses is the run-list status enum. "needs" (#1197) is active
// runs whose attention.by=="you" — the ONE status that reads the attention
// projection rather than a bare SQL predicate, and therefore the one that
// must fetch the live set unpaged and filter/window in Go — see
// handleListRunsFiltered's own "needs" branch.
var runsListStatuses = map[string]bool{"active": true, "ended": true, "failed": true, "killed": true, "needs": true}

// isNeedsStatus reports whether the caller asked for status=needs — always
// alone (never combined with another status value, which "needs" already
// narrows past), and always with a view (attention.by has no meaning
// without one).
func isNeedsStatus(statuses []string) bool {
	return len(statuses) == 1 && statuses[0] == "needs"
}

// parsedRunsListParams is handleListRuns's filtered-branch input: view only
// resolves the owner default/force at this lane — attention/ordering
// is unconditional on the filtered path (see handleListRuns's own doc) — the
// rest is store.RunFilter plus the resolved owner.
type parsedRunsListParams struct {
	view   string // "" | "user" | "admin", already coerced fail-closed
	filter store.RunFilter
}

// parseRunsListParams reads and validates the opt-in params, resolving
// ?owner= (and the view=user owner force) against the caller's own tier. principal
// is the caller's own principal (principalFromRequest), used when the
// resolved owner is "me". Writes a 400 and returns ok=false on any malformed
// value; callers must return immediately when ok is false.
func (s *Server) parseRunsListParams(w http.ResponseWriter, r *http.Request, principal string) (parsedRunsListParams, bool) {
	q := r.URL.Query()

	view := q.Get("view")
	switch view {
	case "", "user", "admin":
	default:
		writeError(w, http.StatusBadRequest, "invalid view")
		return parsedRunsListParams{}, false
	}
	isOperator := s.isSecurityOperator(r.Context())
	if view == "admin" && !isOperator {
		view = "user" // fail closed: a non-operator cannot escalate scope via view=admin
	}

	owner := q.Get("owner")
	switch owner {
	case "", "me", "all":
	default:
		writeError(w, http.StatusBadRequest, "invalid owner")
		return parsedRunsListParams{}, false
	}
	// #1197: view=user forces owner=me for EVERY caller, admins and security
	// operators included — the bug this lane fixes (a non-operator was
	// already forced regardless of view, below). Absent a view, an explicit
	// ?owner= is honoured as asked; absent both, an operator defaults to
	// "all" (today's admin scope) and a non-operator still lands on "me".
	switch {
	case view == "user":
		owner = "me"
	case owner == "":
		if isOperator {
			owner = "all"
		} else {
			owner = "me"
		}
	}
	if !isOperator {
		owner = "me" // the existing fail-closed branch (runs_policy.go), restated under the new param
	}
	resolvedOwner := ""
	if owner == "me" {
		resolvedOwner = principal
	}

	var statuses []string
	seenStatus := map[string]bool{}
	for _, st := range q["status"] {
		if !runsListStatuses[st] {
			writeError(w, http.StatusBadRequest, "invalid status filter")
			return parsedRunsListParams{}, false
		}
		if seenStatus[st] {
			continue // a repeated value adds nothing (the arms are OR'd); dedupe rather than build a longer, equivalent OR clause
		}
		seenStatus[st] = true
		statuses = append(statuses, st)
	}
	if slices.Contains(statuses, "needs") {
		if !isNeedsStatus(statuses) {
			writeError(w, http.StatusBadRequest, "status=needs cannot be combined with another status value")
			return parsedRunsListParams{}, false
		}
		if view == "" {
			writeError(w, http.StatusBadRequest, "status=needs requires view=user or view=admin")
			return parsedRunsListParams{}, false
		}
	}

	endedWithin, ok := endedWithinDurations[q.Get("ended_within")]
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid ended_within")
		return parsedRunsListParams{}, false
	}

	includeKilled := false
	switch q.Get("include_killed") {
	case "", "0":
	case "1":
		includeKilled = true
	default:
		writeError(w, http.StatusBadRequest, "invalid include_killed")
		return parsedRunsListParams{}, false
	}

	searchQuery := q.Get("q")
	if len(searchQuery) > maxRunsListQueryLen {
		writeError(w, http.StatusBadRequest, "q is too long")
		return parsedRunsListParams{}, false
	}

	return parsedRunsListParams{
		view: view,
		filter: store.RunFilter{
			Owner:         resolvedOwner,
			Statuses:      statuses,
			EndedWithin:   endedWithin,
			IncludeKilled: includeKilled,
			Workspace:     q.Get("workspace"),
			Query:         searchQuery,
		},
	}, true
}

// handleListRunsFiltered is handleListRuns' branch for any opt-in param.
// Ordering (live-first, then end time) and the two X-Wardyn-Hidden-* headers
// are unconditional here regardless of which param triggered the branch —
// `view`'s own job at this lane is only the owner default/force; a design
// that ties ordering/headers to `view` alone is a later refinement, and no
// legacy caller reaches this branch with SOME filter but no view, so there
// is no compatibility promise to keep narrower.
//
// Fail CLOSED, same posture as the existing member branch above: a store
// that cannot answer this scoped/filtered read must never fall back to the
// unscoped admin listing.
func (s *Server) handleListRunsFiltered(w http.ResponseWriter, r *http.Request, page store.Page) {
	principal := principalFromRequest(r)
	params, ok := s.parseRunsListParams(w, r, principal)
	if !ok {
		return
	}
	pager, capable := s.cfg.Store.(store.RunsFilteredPager)
	if !capable {
		writeError(w, http.StatusInternalServerError, "run listing is not scoped for this request on this store backend")
		return
	}
	adminView := params.view == "admin"

	// #1197: status=needs reads the attention projection, which no SQL
	// predicate can express — the live set (bounded by state,
	// agent_runs_state_idx) is fetched UNPAGED, projected, filtered to
	// attention.by=="you", and THEN windowed to the caller's own page.
	// "needs" implies live-only by construction, so neither hidden-count
	// header ever hides anything under it — both are always 0.
	if isNeedsStatus(params.filter.Statuses) {
		liveFilter := params.filter
		liveFilter.Statuses = []string{"active"}
		live, err := pager.ListRunsFiltered(r.Context(), liveFilter, store.Page{})
		if err != nil {
			writeServerError(w, r, "list", err)
			return
		}
		if err := s.projectAttention(r, live, adminView); err != nil {
			writeServerError(w, r, "list", err)
			return
		}
		needs := make([]types.AgentRun, 0, len(live))
		for _, run := range live {
			if run.Attention != nil && run.Attention.By == types.AttentionYou {
				needs = append(needs, run)
			}
		}
		s.projectRecordingMeta(r, needs)
		projectStatusDetail(needs)
		w.Header().Set("X-Wardyn-Hidden-Older", "0")
		w.Header().Set("X-Wardyn-Hidden-Killed", "0")
		windowed, truncated := pageWindow(needs, page.Offset, page.Limit)
		if truncated {
			w.Header().Set("X-Wardyn-Truncated", "true")
		}
		writeJSON(w, http.StatusOK, windowed)
		return
	}

	olderHidden, killedHidden, err := pager.CountHiddenRuns(r.Context(), params.filter)
	if err != nil {
		writeServerError(w, r, "list", err)
		return
	}
	w.Header().Set("X-Wardyn-Hidden-Older", strconv.Itoa(olderHidden))
	w.Header().Set("X-Wardyn-Hidden-Killed", strconv.Itoa(killedHidden))
	servePage(w, r, page, func(p store.Page) ([]types.AgentRun, error) {
		runs, err := pager.ListRunsFiltered(r.Context(), params.filter, p)
		if err != nil {
			return nil, err
		}
		// #1197: attention is projected only when the caller opted into
		// the `view` contract — see this function's own doc for why
		// order/headers are unconditional on this branch but attention is
		// not: no legacy caller reaches this branch with a filter but no
		// view, yet `view` is specifically what the API shape documents as
		// opting into attention.
		if params.view != "" {
			if err := s.projectAttention(r, runs, adminView); err != nil {
				return nil, err
			}
		}
		s.projectRecordingMeta(r, runs)
		projectStatusDetail(runs)
		return runs, nil
	}, nil)
}
