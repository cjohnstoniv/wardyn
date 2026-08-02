// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// decisionRequest is the approve/deny body.
type decisionRequest struct {
	Reason string `json:"reason"`
}

// handleListApprovals returns approvals filtered by ?state= (empty = all) and
// ?run_id= (empty = every run), paginated by ?limit=&offset= (see parseListPage).
//
// When the lister also implements the OPTIONAL approvalPageLister (the wardynd
// adapter does, promoting store.PG's ListApprovalsPage), the un-filtered list
// applies LIMIT/OFFSET at the DB — the console polls four single-state lists
// every 10s and decided rows are never deleted, so the unpaged read grew with
// deployment age. A lister without it (test fakes) keeps servePage's fetch-all
// fallback.
//
// ?run_id= stays on the fetch-all path and filters INSIDE the closure, i.e.
// before servePage windows the result. That ordering is the whole point: the
// list is requested_at DESC and capped at maxListLimit, so filtering after the
// window would drop a run older than the newest 1000 approvals from its own
// detail page — silently, with a PENDING badge of 0.
func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	state := types.ApprovalState(r.URL.Query().Get("state"))
	switch state {
	case "", types.ApprovalPending, types.ApprovalApproved, types.ApprovalDenied, types.ApprovalExpired:
	default:
		writeError(w, http.StatusBadRequest, "invalid state filter")
		return
	}
	var runID uuid.UUID
	if raw := r.URL.Query().Get("run_id"); raw != "" {
		var err error
		if runID, err = uuid.Parse(raw); err != nil {
			writeError(w, http.StatusBadRequest, "invalid run_id")
			return
		}
	}
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}
	var pageFn func(store.Page) ([]types.ApprovalRequest, error)
	if pl, ok := s.cfg.Approvals.(approvalPageLister); ok && runID == uuid.Nil {
		pageFn = func(p store.Page) ([]types.ApprovalRequest, error) {
			return pl.ListApprovalsPage(r.Context(), state, p)
		}
	}
	servePage(w, page, pageFn, func() ([]types.ApprovalRequest, error) {
		all, err := s.cfg.Approvals.List(r.Context(), state)
		if err != nil || runID == uuid.Nil {
			return all, err
		}
		out := make([]types.ApprovalRequest, 0, len(all))
		for _, ap := range all {
			if ap.RunID == runID {
				out = append(out, ap)
			}
		}
		return out, nil
	})
}

// approvalPageLister is the optional DB-paged read surface an ApprovalService
// may additionally implement (see handleListApprovals). Deliberately NOT part
// of ApprovalService: test doubles embedding the interface keep compiling, the
// exact rationale store.Pager documents.
type approvalPageLister interface {
	ListApprovalsPage(ctx context.Context, state types.ApprovalState, p store.Page) ([]types.ApprovalRequest, error)
}

// handleApproveApproval transitions an approval to APPROVED. For credential
// approvals the broker mints inside the same transaction that observes the
// APPROVED state (handled by the broker on the next mint call); here we only
// record the human decision via the approval FSM.
func (s *Server) handleApproveApproval(w http.ResponseWriter, r *http.Request) {
	s.decide(w, r, true)
}

// handleDenyApproval transitions an approval to DENIED (fail closed).
func (s *Server) handleDenyApproval(w http.ResponseWriter, r *http.Request) {
	s.decide(w, r, false)
}

func (s *Server) decide(w http.ResponseWriter, r *http.Request, approve bool) {
	id, ok := parseIDParam(w, r, "id", "approval")
	if !ok {
		return
	}
	var body decisionRequest
	if r.Body != nil {
		// Reason is optional; ignore a decode error on an empty body.
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&body)
	}
	decidedByType, decidedBy := actorFromRequest(r)

	result, err := s.cfg.Approvals.Decide(r.Context(), id, approve, decidedByType, decidedBy, body.Reason)
	if err != nil {
		switch {
		// One sentinel: approval.ErrAlreadyDecided IS store.ErrAlreadyDecided.
		case errors.Is(err, approval.ErrAlreadyDecided):
			writeError(w, http.StatusConflict, "approval already decided")
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "approval not found")
		default:
			writeError(w, http.StatusInternalServerError, "decide approval: "+err.Error())
		}
		return
	}
	// Counted HERE, at the one service call both handlers (and every non-human
	// decision path) funnel through — never in handleApprove/handleDeny, which
	// would each need their own increment. The counter cannot live in
	// approval.Decide itself: internal/api imports internal/approval, so the
	// dependency only runs this way.
	s.metrics.approvalDecided(approve)
	writeJSON(w, http.StatusOK, result)
}
