// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// decisionRequest is the approve/deny body.
type decisionRequest struct {
	Reason string `json:"reason"`
}

// handleListApprovals returns approvals filtered by ?state= (empty = all),
// paginated by ?limit=&offset= (see parseListPage).
//
// ponytail: the store's ListApprovalsPage applies LIMIT/OFFSET at the DB, but the
// approvals list here routes through the cfg.Approvals lister (an interface owned
// by the approval package, not a store.Pager), so this endpoint bounds the
// PAYLOAD in Go via servePage's fetch-all fallback rather than the query. The
// approvals_requested_at / approvals_state_requested_at indexes keep the
// underlying sort cheap; thread Page through the Approvals interface if the
// full-table read itself ever bites.
func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	state := types.ApprovalState(r.URL.Query().Get("state"))
	switch state {
	case "", types.ApprovalPending, types.ApprovalApproved, types.ApprovalDenied, types.ApprovalExpired:
	default:
		writeError(w, http.StatusBadRequest, "invalid state filter")
		return
	}
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}
	servePage(w, page, nil, func() ([]types.ApprovalRequest, error) { return s.cfg.Approvals.List(r.Context(), state) })
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
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	decidedByType, decidedBy := actorFromRequest(r)

	result, err := s.cfg.Approvals.Decide(r.Context(), id, approve, decidedByType, decidedBy, body.Reason)
	if err != nil {
		switch {
		case errors.Is(err, approval.ErrAlreadyDecided), errors.Is(err, store.ErrAlreadyDecided):
			writeError(w, http.StatusConflict, "approval already decided")
		case errors.Is(err, store.ErrNotFound):
			writeError(w, http.StatusNotFound, "approval not found")
		default:
			writeError(w, http.StatusInternalServerError, "decide approval: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, result)
}
