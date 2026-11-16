// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// templatePolicyCheckNoCandidateMsg answers a check whose body names no candidate. Read as an empty
// policy it would be within nearly anything, so a gate would pass on a body that never held one.
const templatePolicyCheckNoCandidateMsg = "A candidate is required: the policy to check against the template"

// handleTemplatePolicyCheck answers POST /api/v1/templates/{id}/policy-check: is the candidate
// within the template's policy as it resolves for the caller? A template carries some policy keys
// and takes the rest from the caller's source policy, so no client can measure against it alone.
// The template is read with the caller's read authority (one they cannot read is the missing 404),
// its policy is resolved as a launch of it would be (templateEffectivePolicy), and the verdict is
// composer.Leq, the rule `wardyn policy check` applies to a stored policy or a file. The answer is
// the verdict and the fields that keep it false, never the resolved policy. It works like the policy
// preview, so it shares the preview's rate, refuses the Admin view and asks an admin token for its
// break-glass reason. It stores nothing and writes no audit row.
func (s *Server) handleTemplatePolicyCheck(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(audit.WithDryRun(r.Context()))
	if s.refusePolicyPreviewRate(w, r) {
		return
	}
	ts, c, id, ok := s.templateRoute(w, r, true)
	if !ok {
		return
	}
	t, ok := readableTemplate(w, r, ts, c, id, 0, false)
	if !ok {
		return
	}
	var body client.TemplatePolicyCheckRequest
	if !decodeStrict(w, r, &body) {
		return
	}
	if body.Candidate == nil {
		writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, templatePolicyCheckNoCandidateMsg)
		return
	}
	if s.refuseAdminViewLaunch(w, r) {
		return
	}
	if r, ok = s.admitBreakGlass(w, r); !ok {
		return
	}
	effective, ok := s.templateEffectivePolicy(w, r, t)
	if !ok {
		return
	}
	// Judged on the policy this caller's own reads show, never the operator's copy: a verdict on a
	// mount's host path or a secret's name would confirm a guess of what those reads withhold.
	effective = redactSpecForRead(effective, s.specReadTierFor(r.Context()))
	var none types.GovernanceLimits
	out := client.PolicyCheckResult{Within: composer.Leq(*body.Candidate, effective, none, none), Widenings: []client.PolicyWidening{}}
	if !out.Within {
		for _, wd := range composer.LeqWidenings(*body.Candidate, effective) {
			out.Widenings = append(out.Widenings, client.PolicyWidening(wd))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// templateEffectivePolicy is the policy a launch of t by this caller starts from. The template's
// request is the one a workload launch makes of an intent (client.RunDocumentRequest: inline_policy
// laid on as the overlay), held to the run door's decode checks (applyRunMode) with its context
// choice stamped, then resolved by the policy step the preview runs: resolveRunPolicyFacts, dry and
// reading no credential. So the overlay, the source policy, the caller's ceiling, their walls and the
// boundary have one definition, the run door's. It writes its own refusal and returns false once it
// has answered.
func (s *Server) templateEffectivePolicy(w http.ResponseWriter, r *http.Request, t types.RunTemplate) (types.RunPolicySpec, bool) {
	var doc client.TemplateDocument
	if err := json.Unmarshal(t.Document, &doc); err != nil {
		writeServerError(w, r, "read template", err)
		return types.RunPolicySpec{}, false
	}
	req, err := client.RunDocumentRequest(doc.Intent)
	if err != nil {
		writeServerError(w, r, "read template", err)
		return types.RunPolicySpec{}, false
	}
	if applyRunMode(&req).write(s, w, r) {
		return types.RunPolicySpec{}, false
	}
	canonicalizeRunRepos(&req, s.adoHostsLoader(r.Context()))
	r, ok := s.stampContextChoice(w, r, req)
	if !ok {
		return types.RunPolicySpec{}, false
	}
	// The launch's own capability gates on what the template names: its stored policy, workspaces,
	// agent and image. Without them a template naming a policy withheld from the caller would be
	// measured against, and the answer would tell them what that policy holds.
	if _, refusal := s.runRequestGovernance(r, req); refusal.write(s, w, r) {
		return types.RunPolicySpec{}, false
	}
	spec, _, _, _, refusal := s.resolveRunPolicyFacts(r.Context(), r, &req, true, false)
	if refusal.write(s, w, r) {
		return types.RunPolicySpec{}, false
	}
	return spec, true
}
