// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/approval"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// decisionRequest is the approve/deny body.
type decisionRequest struct {
	Reason string `json:"reason"`
}

// handleListApprovals returns approvals filtered by ?state= (empty = all).
func (s *Server) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	state := types.ApprovalState(r.URL.Query().Get("state"))
	switch state {
	case "", types.ApprovalPending, types.ApprovalApproved, types.ApprovalDenied, types.ApprovalExpired:
	default:
		writeError(w, http.StatusBadRequest, "invalid state filter")
		return
	}
	out, err := s.cfg.Approvals.List(r.Context(), state)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list approvals: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
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

// compile-time reference so the broker import is meaningful even when the API
// only mints from internal handlers (keeps the dependency intentional).
var _ = broker.Minted{}
