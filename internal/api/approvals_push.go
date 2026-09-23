// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The control-plane half of a held git push (push_rules.require_review_paths).
//
// The proxy sidecar does the holding (internal/egress/proxy/push_hold.go) and
// raises a push_content approval through handleInternalRequestApproval. What
// lives here is what the control plane checks before that row exists:
//
//   - THE SHAPE. The scope is what the console renders on the push card, so a
//     row is only written when it is one (types.PushContentScope.Validate).
//   - THE RUN IS ATTENDED. An unattended run's sidecar refuses a review-path
//     push outright rather than raising (brokered:git:push-held-unattended);
//     refusing here too means a sidecar that did not, for whatever reason,
//     still leaves no question in front of a human that nobody's run is
//     waiting on.
//
// Who may DECIDE one is authorizeMemberDecision's, unchanged: members are kept
// to egress_domain (and their own Azure DevOps escalations), so push_content
// is admin-decidable only. A member approving their own run's workflow-file
// edit is the exfiltration the rule exists to stop. A decision carries no
// decision_scope (decide's rule 4 refuses one on this kind): the sidecar
// treats an approval as covering exactly the commits the scope names.

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pushContentUnattendedBody is the refusal a raise for an unattended run gets.
const pushContentUnattendedBody = "this run is unattended, so a push that needs review is refused rather than held"

// admitPushContentRaise is handleInternalRequestApproval's push_content arm.
// It writes its own 4xx and reports false when the raise must not proceed.
func (s *Server) admitPushContentRaise(w http.ResponseWriter, r *http.Request, runID uuid.UUID, raw json.RawMessage) bool {
	var scope types.PushContentScope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&scope); err != nil {
		writeError(w, http.StatusBadRequest, "invalid push_content requested_scope: "+err.Error())
		return false
	}
	if err := scope.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, "invalid push_content requested_scope: "+err.Error())
		return false
	}
	if s.cfg.Store == nil {
		writeError(w, http.StatusServiceUnavailable, "run store unavailable")
		return false
	}
	run, err := s.cfg.Store.GetRun(r.Context(), runID)
	if err != nil {
		writeServerError(w, r, "load run for push_content", err)
		return false
	}
	if !run.Interactive {
		writeError(w, http.StatusForbidden, pushContentUnattendedBody)
		return false
	}
	return true
}
