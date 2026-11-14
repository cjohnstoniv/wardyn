// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The note on a held governance change: PUT /governance/changes/{id}/note. The person who proposed a
// change writes why it is wanted and, optionally, a ticket reference; the deciders read both beside
// the diff. One route serves every held kind, so no covered write grows a field of its own. The note
// is optional here: a client that never sends one is unchanged. The columns and the validator are the
// ones a request for elevated access reuses, with the reason required.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// governanceApproverSecurity is the one value ?approver= takes: the changes only a security admin
// may decide.
const governanceApproverSecurity = "security"

// handlePutGovernanceChangeNote sets the note of a pending change. Only the proposer may (the owner test
// a withdrawal uses, ownsGovernanceChange), and only while the change is pending. A change the caller may
// not see answers the hidden 404, as a read does.
func (s *Server) handlePutGovernanceChangeNote(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "governance change")
	if !ok {
		return
	}
	ctx := r.Context()
	ch, err := s.cfg.Store.GetGovernanceChange(ctx, id)
	if err == nil && !s.canSeeGovernanceChange(r, ch) {
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
	if !s.ownsGovernanceChange(ch, principal, oidcEmailFromContext(ctx)) {
		writeErrorReason(w, http.StatusForbidden, reasonGovernanceChangeNoteProposerOnly,
			"only the person who proposed a change may write its note")
		return
	}
	body, ok := readCappedBody(w, r, 16<<10, "request body")
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
		Ticket string `json:"ticket"`
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "invalid JSON body: "+err.Error())
		return
	}
	// A request's note is never emptied of its reason, and an elevation's ticket still meets its unit's rule.
	_, isRequest := requestKindOf(ch.TargetKind)
	note, err := types.ValidateChangeNote(req.Reason, req.Ticket, isRequest)
	if err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonGovernanceChangeNoteInvalid, err.Error())
		return
	}
	if ch.TargetKind == types.GovernanceTargetElevation && !s.elevationNoteTicketOK(w, r, ch, note.Ticket) {
		return
	}
	saved, err := s.cfg.Store.SetGovernanceChangeNote(ctx, id, note)
	var notPending *store.ErrGovernanceChangeNotPending
	switch {
	case errors.As(err, &notPending):
		writeErrorReason(w, http.StatusConflict, reasonGovernanceChangeNotPending,
			"this change is no longer pending ("+notPending.State+")")
		return
	case notFoundIf(w, err, "governance change", reasonGovernanceChangeNotFound):
		return
	case err != nil:
		writeServerError(w, r, "write governance change note", err)
		return
	}
	data := map[string]any{"target_kind": saved.TargetKind, "target_key": saved.TargetKey, "reason": note.Reason, "ticket": note.Ticket}
	if units, _, scoped := unitChangeScope(saved); scoped {
		data["units"] = unitIDStrings(units)
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorType, principal, "governance.change.note", saved.ID.String(), "success", mustJSON(data)))
	writeJSON(w, http.StatusOK, s.changeForRead(r, saved))
}

// unitIDStrings is ids as their canonical strings, in order.
func unitIDStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// securityOnlyChange reports whether a security admin alone may decide ch: its approvers list is the
// security entry and nothing else.
func (s *Server) securityOnlyChange(r *http.Request, ch types.GovernanceChange) bool {
	a := s.changeApprovers(r.Context(), ch)
	return len(a) == 1 && a[0].Kind == "security"
}
