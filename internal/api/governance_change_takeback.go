// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ── take back ─────────────────────────────────────────────────────────────────

// handleTakeBackGovernanceApproval is DELETE /governance/changes/{id}/approval: the caller takes back their
// approval of a pending change. It works whatever the minimum is now, on any pending change where the caller
// holds an approval that counts, and answers 404 when they hold none.
func (s *Server) handleTakeBackGovernanceApproval(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "governance change")
	if !ok {
		return
	}
	ctx := r.Context()
	head, err := s.cfg.Store.GetGovernanceChange(ctx, id)
	if err == nil && !s.canSeeGovernanceChange(r, head) {
		err = store.ErrNotFound
	}
	if notFoundIf(w, err, "governance change", reasonGovernanceChangeNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "read governance change", err)
		return
	}
	actorType, principal := actorFromRequest(r)
	email := oidcEmailFromContext(ctx)
	var taken takenBack
	out, err := s.cfg.Store.DecideGovernanceChange(ctx, id, store.GovernanceDecision{}, s.takeBack(ctx, principal, email, &taken))
	switch {
	case errors.Is(err, store.ErrGovernanceApprovalRecorded):
	case err != nil:
		s.answerFailedTakeBack(w, r, head, actorType, principal, err)
		return
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorType, principal, "governance.change.retract", out.ID.String(), "success", mustJSON(map[string]any{
		"target_kind": out.TargetKind, "target_key": out.TargetKey, "proposed_by": out.ProposedBy,
		"approver": taken.approver, "have": taken.have, "required": s.effective(out),
	})))
	s.writeChangeView(w, r, out)
}

// takenBack is what a take-back leaves for its audit row: whose approval it deleted and how many still count.
type takenBack struct {
	approver string
	have     int
}

// takeBack is the take-back's body in the transaction that locked the change: the caller's token is read
// again, their approval that counts is deleted, and what is left is put in *taken. The approval is the
// caller's own principal's; the same email counts only when sign-in vouches for every caller's email, so an
// email it does not vouch for never takes back another person's approval. The sentinel commits the delete
// and leaves the change as it is.
func (s *Server) takeBack(ctx context.Context, principal, email string, taken *takenBack) store.GovernanceDecideFunc {
	return func(q store.Querier, ch types.GovernanceChange) error {
		if _, err := s.recheckLiveToken(ctx, q); err != nil {
			return err
		}
		rows, err := store.ListGovernanceChangeApprovalsQ(ctx, q, ch.ID)
		if err != nil {
			return err
		}
		// The caller's own principal's approval first; the same email only when they hold none of their own.
		own := slices.IndexFunc(rows, func(row store.GovernanceChangeApproval) bool {
			return row.Counts() && principal != "" && row.Approver == principal
		})
		if own < 0 && s.emailTrusted() {
			own = slices.IndexFunc(rows, func(row store.GovernanceChangeApproval) bool {
				return row.Counts() && sameHuman(principal, email, row.Approver, row.ApproverEmail)
			})
		}
		if principal == "" || own < 0 {
			return &govRefusal{why: "no approval to take back", write: func(w http.ResponseWriter, r *http.Request) {
				writeErrorReason(w, http.StatusNotFound, reasonGovernanceChangeApprovalNotFound, "you have no approval of this change to take back")
			}}
		}
		if _, err := store.DeleteGovernanceChangeApprovalQ(ctx, q, ch.ID, rows[own].Approver); err != nil {
			return err
		}
		taken.approver, taken.have = rows[own].Approver, len(rows)-countDropped(rows)-1
		return store.ErrGovernanceApprovalRecorded
	}
}

func countDropped(rows []store.GovernanceChangeApproval) int {
	n := 0
	for _, row := range rows {
		if !row.Counts() {
			n++
		}
	}
	return n
}

// answerFailedTakeBack writes the response for a take-back that did not commit: a refusal the decision
// carried, a change no longer pending (a lapsed one is moved to expired and audited as a decision would),
// a change that is gone, or an error.
func (s *Server) answerFailedTakeBack(w http.ResponseWriter, r *http.Request, head types.GovernanceChange, actorType types.ActorType, principal string, err error) {
	var refusal *govRefusal
	var notPending *store.ErrGovernanceChangeNotPending
	switch {
	case errors.As(err, &refusal):
		refusal.write(w, r)
	case errors.As(err, &notPending):
		if notPending.Lapsed {
			s.recordAudit(r.Context(), s.auditEvent(nil, actorType, principal, "governance.change.expire", head.ID.String(), "success",
				mustJSON(map[string]any{"target_kind": head.TargetKind, "target_key": head.TargetKey})))
		}
		writeErrorReason(w, http.StatusConflict, reasonGovernanceChangeNotPending, "this change is no longer pending ("+notPending.State+")")
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonGovernanceChangeNotFound, "governance change not found")
	default:
		writeServerError(w, r, "take back a governance approval", err)
	}
}
