// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gradePolicyRequest is the POST /api/v1/policies/grade body: a bare spec to
// grade with no run attached — the policy panel's live safety meter.
// Interactive is an optional hint, default false (the conservative frame):
// composer.Grade's only read of its RunInput is Interactive (the never-reap
// item in risk.go), so without the hint an interactive-intended policy's
// never-reap grade would carry the non-interactive rationale instead of the
// "expected for an interactive run" one. The /policies panel instance sends no
// hint; the run wizard's instance sends the screen's mode.
type gradePolicyRequest struct {
	Spec        types.RunPolicySpec `json:"spec"`
	Interactive bool                `json:"interactive,omitempty"`
}

// gradePolicyResponse reuses preflightResponse's risk fields' wire names
// verbatim, so the UI's RiskItem/RiskLevel TS mirror needs nothing new for
// this endpoint.
type gradePolicyResponse struct {
	RiskAssessment []composer.RiskItem `json:"risk_assessment"`
	OverallRisk    composer.RiskLevel  `json:"overall_risk"`
}

// handleGradePolicy grades a policy spec on its own: no policy resolution, no
// store read/write, mints nothing. It exists so the policy panel can show a
// live safety meter while a spec is still being edited, before it is ever
// saved or attached to a run. Member-accessible (mirrors /runs/preflight): a
// member may see the risk of a spec they cannot necessarily save.
// validatePolicySpec runs FIRST, with the same 400-naming-the-field contract
// /policies' save path uses — grading a spec invalid enough to reject at write
// time would tell a lie about it.
func (s *Server) handleGradePolicy(w http.ResponseWriter, r *http.Request) {
	var req gradePolicyRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if err := validatePolicySpec(req.Spec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid policy spec: "+err.Error())
		return
	}
	runInput := composer.RunInput{Interactive: req.Interactive}
	items := composer.Grade(runInput, req.Spec)
	writeJSON(w, http.StatusOK, gradePolicyResponse{
		RiskAssessment: items,
		OverallRisk:    composer.OverallLevel(items),
	})
}
