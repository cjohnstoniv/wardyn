// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// GET /approvals?decidable_by=me: the PENDING approvals the caller may decide now. New file: approvals.go sits
// near the file-size cap, and the list is the resolver's verdict (mayDecide) over a candidate set, so it lives
// beside the resolver (deciders_resolve.go) and not beside the plain listing.
package api

import (
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxDecidableByMe is how many rows decidable_by=me returns; a further decidable row sets X-Wardyn-Truncated.
const maxDecidableByMe = 500

// listDecidableByMe answers GET /approvals?decidable_by=me and reports whether it did. The list is exactly the
// PENDING approvals mayDecide says the caller may decide, newest first: a security admin's candidates are every
// pending approval, anyone else's are those on runs they created plus those on runs launched in a unit a
// deciders rule governs. A hold the caller may not decide is left out, whether it exists or not, so the list
// (and ?run_id= on it) tells no one of a request they could not decide. A portal's delegated token never gets
// here: the route is not on its allow list. Limit and offset are not read; the cap is fixed.
func (s *Server) listDecidableByMe(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	if !q.Has("decidable_by") {
		return false
	}
	if q.Get("decidable_by") != "me" {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidDecidableBy, "invalid decidable_by")
		return true
	}
	if st := types.ApprovalState(q.Get("state")); st != "" && st != types.ApprovalPending {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidApprovalState, "decidable_by lists PENDING approvals only")
		return true
	}
	var runID uuid.UUID
	if raw := q.Get("run_id"); raw != "" {
		var err error
		if runID, err = uuid.Parse(raw); err != nil {
			writeErrorReason(w, http.StatusBadRequest, reasonInvalidRunIDParam, "invalid run_id")
			return true
		}
	}
	r = withDeciderMemo(r)
	cands, ok := s.decidableCandidates(w, r)
	if !ok {
		return true
	}
	slices.SortFunc(cands, func(a, b types.ApprovalRequest) int {
		if c := b.RequestedAt.Compare(a.RequestedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID.String(), b.ID.String()) // a total order, so the 500 cut is stable
	})
	runs := map[uuid.UUID]types.AgentRun{}
	rows := []types.ApprovalRequest{}
	truncated := false
	for _, ap := range cands {
		if ap.State != types.ApprovalPending || ap.Kind == types.ApprovalHumanTakeover || (runID != uuid.Nil && ap.RunID != runID) {
			continue // a takeover is answered by taking over or declining, never decided
		}
		run, seen := runs[ap.RunID]
		if !seen {
			var err error
			if run, err = s.cfg.Store.GetRun(r.Context(), ap.RunID); err != nil {
				continue // a run that cannot be read decides nothing
			}
			runs[ap.RunID] = run
		}
		if !s.mayDecide(r, ap, run) {
			continue
		}
		if len(rows) == maxDecidableByMe {
			truncated = true
			break
		}
		rows = append(rows, ap)
	}
	rows, _ = s.withHoldProjectionAll(r, func() ([]types.ApprovalRequest, error) { return rows, nil })()
	principal := principalFromRequest(r)
	for i := range rows {
		if by := runs[rows[i].RunID].CreatedBy; by != "" && by != principal {
			rows[i].RequestedBy = by
		}
	}
	if truncated {
		w.Header().Set("X-Wardyn-Truncated", "true")
	}
	writeJSON(w, http.StatusOK, rows)
	return true
}

// decidableCandidates is every PENDING approval the caller could possibly decide: all of them for a security
// admin, and for anyone else those on runs they created plus those on runs launched in a unit a deciders rule
// governs. mayDecide narrows it. A backend that cannot scope the read refuses (never an unscoped list), and a
// deciders read that fails leaves the caller's own runs, whose rows mayDecide then refuses for the same
// failure, as the decision does.
func (s *Server) decidableCandidates(w http.ResponseWriter, r *http.Request) ([]types.ApprovalRequest, bool) {
	ctx := r.Context()
	if s.isSecurityOperator(ctx) {
		all, err := s.cfg.Approvals.List(ctx, types.ApprovalPending)
		if err != nil {
			writeServerError(w, r, "list decidable approvals", err)
			return nil, false
		}
		return all, true
	}
	byCreator, capable := s.cfg.Approvals.(store.ApprovalsByRunCreatorPager)
	if !capable {
		writeErrorReason(w, http.StatusInternalServerError, reasonListingUnscopedBackend, "approval listing is not scoped for members on this backend")
		return nil, false
	}
	own, err := byCreator.ListApprovalsPageByRunCreator(ctx, principalFromRequest(r), types.ApprovalPending, store.Page{})
	if err != nil {
		writeServerError(w, r, "list decidable approvals", err)
		return nil, false
	}
	tree, err := s.deciderTree(ctx)
	if err != nil || tree == nil {
		return own, true
	}
	units := tree.governedUnits()
	if len(units) == 0 {
		return own, true
	}
	onUnits, capable := s.cfg.Approvals.(store.ApprovalsOnUnitsPager)
	if !capable {
		writeErrorReason(w, http.StatusInternalServerError, reasonListingUnscopedBackend, "approval listing is not scoped for deciders on this backend")
		return nil, false
	}
	more, err := onUnits.ListPendingApprovalsOnUnits(ctx, units)
	if err != nil {
		writeServerError(w, r, "list decidable approvals", err)
		return nil, false
	}
	seen := map[uuid.UUID]bool{}
	for _, ap := range own {
		seen[ap.ID] = true
	}
	for _, ap := range more {
		if !seen[ap.ID] {
			own = append(own, ap)
		}
	}
	return own, true
}

// governedUnits is every unit in the tree that a rule governs: one with a rule of its own or below a unit that
// has one. Runs launched in any other unit are decided as without deciders.
func (t *deciderTree) governedUnits() []uuid.UUID {
	var out []uuid.UUID
	for unit := range t.parents {
		cur := &unit
		for n := 0; cur != nil && n < store.MaxUnitDepthCap; n++ {
			parent, exists := t.parents[*cur]
			if !exists {
				break
			}
			if len(t.rules[*cur]) > 0 {
				out = append(out, unit)
				break
			}
			cur = parent
		}
	}
	return out
}

// decidesApproval reports whether a unit's deciders rule names the caller as a decider of ap on run, who then
// sees the request as its owner does. False with no rule, for a caller the rule does not name, and when the
// rule cannot be read.
func (s *Server) decidesApproval(r *http.Request, ap types.ApprovalRequest, run types.AgentRun) bool {
	v, err := s.resolveDecider(r, ap, run)
	return err == nil && v.decides()
}
