// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A helper the run's owner accepted decides no hold of that run raised from their request on, whatever the
// hold's kind and whatever basis they would decide it on, and never counts as its second human. There is no
// kind list: a helper can already reach everything the run can, so a hold the run raised afterwards could be
// theirs own doing. The rule reads the grant record, not the support switch, so an ended grant, and a switch
// turned off since, change nothing.

// SupportHelperDecisionBody answers a helper's decision on a hold raised after their request.
const SupportHelperDecisionBody = "You are helping in this run, so someone else approves or denies this request."

// SupportHelperSecondHumanWhy is the sentence a helper meets when the hold needs a second human.
const SupportHelperSecondHumanWhy = "you are helping in this run, so someone else approves or denies it"

// supportHelperOf reports whether principal was accepted as a helper on the run by a grant requested at or
// before at; a zero at is any accepted grant. A store without the grant record has no helper.
func (s *Server) supportHelperOf(ctx context.Context, runID uuid.UUID, principal string, at time.Time) (bool, error) {
	st, ok := s.cfg.Store.(store.SupportGrantStore)
	if !ok {
		return false, nil
	}
	return st.SupportHelperSince(ctx, runID, principal, at)
}

// helperBlocksDecision is true when the caller is, or cannot be shown not to be, a helper on the hold's run
// for a request at or before at, as isRunsOwnPerson reads a failure as yes.
func (s *Server) helperBlocksDecision(r *http.Request, runID uuid.UUID, at time.Time) bool {
	_, principal := actorFromRequest(r)
	helper, err := s.supportHelperOf(r.Context(), runID, principal, at)
	return helper || err != nil
}

// refuseHelperDecision answers 403 when the caller helped in the hold's run before the hold was raised. It
// is decide()'s rule for a hold raised from the helper's request on.
func (s *Server) refuseHelperDecision(w http.ResponseWriter, r *http.Request, ap types.ApprovalRequest) bool {
	if !s.helperBlocksDecision(r, ap.RunID, ap.RequestedAt) {
		return false
	}
	return s.refuse(w, r, authz.Deny(authz.ReasonSupportHelperDecision, ap.ID.String(), SupportHelperDecisionBody).OnRun(ap.RunID))
}
