// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const policyPreviewBurst = 15

func (s *Server) refusePolicyPreviewRate(w http.ResponseWriter, r *http.Request) bool {
	if s.policyPreviewLimiter == nil {
		return false
	}
	if who, limited := rateLimitedActor(r); !limited || s.policyPreviewLimiter.allow(who, s.cfg.Now()) {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(60/float64(s.cfg.PolicyPreviewRatePerMin))))))
	writeErrorReason(w, http.StatusTooManyRequests, reasonPolicyPreviewRateLimited, "too many policy preview checks; slow down")
	return true
}

func (s *Server) handlePolicyPreview(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(audit.WithDryRun(r.Context()))
	if s.refusePolicyPreviewRate(w, r) {
		return
	}
	if s.refuseAdminViewLaunch(w, r) {
		return
	}
	r, ok := s.admitBreakGlass(w, r)
	if !ok {
		return
	}
	var req createRunRequest
	if !s.decodeRunRequest(w, r, &req) {
		return
	}
	canonicalizeRunRepos(&req, s.adoHostsLoader(r.Context()))
	ceiling, reqCC, refusal := s.authorizePreviewRequest(r, req)
	if refusal.write(s, w, r) {
		return
	}
	// The derivation create applies last, so the managed-settings fact describes
	// the tool_approvals value launch dispatches with.
	req.ToolApprovals = effectiveToolApprovals(req, ceiling)
	f, ok := s.foldRunRequest(w, r, foldPreview, &req, ceiling, reqCC)
	if !ok {
		return
	}
	response := policyPreviewFacts(req, f.spec, f.source, f.policyWarns, f.scmSite, f.mpChoice)
	response.Provenance = f.prov
	response.Attachments = f.attachments
	response.RunnerPool, response.RunnerPools = f.place.facts()
	response.Image, response.HarnessSet = f.image, harnessSetFact(f.harnesses, f.harnessChoices)
	response.Limits, response.ResourcesApplied = f.place.sizeFacts()
	response.RunnerPoolSlots = s.runnerPoolSlots(r.Context(), f.place, *f.req)
	response.MainDrive = f.mainDrive.applied()
	runRepos := append([]string{f.req.Repo, f.req.DevcontainerRepo}, repoLocatorsOf(f.spec.WorkspaceRepos)...)
	var signIn *SCMAccess
	if !adoServiceCaller(r.Context()) {
		signIn = s.adoSignInPresenceFact(r.Context(), f.scmSite, oidcHumanFromContext(r.Context()), runRepos)
	}
	response.Components = componentFacts(componentFactInputs{
		req: f.req, spec: f.spec, harnesses: f.harnesses, comps: f.comps, baseline: f.baseline, scmSite: f.scmSite,
		ceiling: f.ceiling, mpChoice: f.mpChoice, pushSets: f.pushSets,
	}, signIn)
	// After the facts' own warnings, which previewSafeWarnings has already filtered.
	if f.adoNarrowed != "" {
		response.Warnings = append(response.Warnings, f.adoNarrowed)
	}
	// Run-level, never per repository group: the groups include devcontainer_repo,
	// which the server clones and the lane never serves.
	response.ADOLaneDeclined = string(f.adoDecline)
	if f.adoDecline != "" {
		response.Warnings = append(response.Warnings, f.adoDecline.sentence())
	}
	var err error
	if response.Access, err = s.accessFor(r, accessInputs{req: f.req, spec: f.spec, prov: f.prov, source: f.source,
		scmSite: f.scmSite, mpChoice: f.mpChoice, comps: f.comps, facts: response.Components, level: f.autonomy.Level}); err != nil {
		writeServerError(w, r, "resolve capability", err)
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) authorizePreviewRequest(r *http.Request, req createRunRequest) (governanceCeiling, types.ConfinementClass, *runRefusal) {
	if msg := agentRequirementError(req); msg != "" {
		return governanceCeiling{}, "", runError(http.StatusBadRequest, reasonAgentRequired, msg)
	}
	if msg, err := s.agentRosterRefusal(r.Context(), req.Agent); err != nil {
		return governanceCeiling{}, "", runServerError("get site config", err)
	} else if msg != "" {
		return governanceCeiling{}, "", runError(http.StatusUnprocessableEntity, reasonAgentNotEnabled, msg).forTool(harnessRefusalTool(req))
	}
	if reservedRunTasks[req.Task] {
		return governanceCeiling{}, "", runError(http.StatusBadRequest, reasonRunTaskReserved, fmt.Sprintf("task %q is set by the server and cannot be requested directly", req.Task))
	}
	ceiling, refusal := s.runRequestGovernance(r, req)
	if refusal != nil {
		return ceiling, "", refusal
	}
	if refusal = s.runGovernancePosture(r, req, ceiling, false); refusal != nil {
		return ceiling, "", refusal
	}
	if refusal = s.runGovernanceSupervision(r, req, ceiling); refusal != nil {
		return ceiling, "", refusal
	}
	if refusal = s.repoAdmissionRefusal(r, req.Repo, req.DevcontainerRepo); refusal != nil {
		return ceiling, "", refusal
	}
	if req.Repo != "" {
		if refusal = s.workspaceProvidersRefusal(r, "runs.workspace_provider", req.Repo); refusal != nil {
			return ceiling, "", refusal
		}
	}
	if msg, reason := s.validateImageBuildRequest(req); msg != "" {
		return ceiling, "", runError(http.StatusBadRequest, reason, msg)
	}
	cc, refusal := runRequestEnums(req)
	if refusal != nil {
		return ceiling, "", refusal
	}
	if refusal = s.localFolderRefusal(r.Context(), &req); refusal != nil {
		return ceiling, "", refusal
	}
	if refusal = s.runTextFieldsRefusal(req); refusal != nil {
		return ceiling, "", refusal
	}
	if req.IntegrationID != "" {
		return ceiling, "", runError(http.StatusUnprocessableEntity, reasonIntegrationIDRetired, mpRunNoIntegration)
	}
	if msg := interactiveToolApprovalsError(req); msg != "" {
		return ceiling, "", runError(http.StatusBadRequest, reasonToolApprovalsHoldInteractiveConflict, msg)
	}
	return ceiling, cc, nil
}

// unionPreviewWorkspaceEgress is the workspace half of deriveRunEgress, unioned
// before the requirements fold so Review grades it; stepDerivedEgress adds the
// grant lanes after every gate. The site-config, ADO and model-provider host
// unions remain pending.
func unionPreviewWorkspaceEgress(spec *types.RunPolicySpec, workspaces []types.Workspace, trusted []string) {
	unionWorkspaceEgress(spec, workspaces)
	for _, ws := range workspaces {
		kept, _ := runCloneEgress(ws, trusted)
		unionAllowedDomains(spec, kept)
	}
}

func previewPending(req createRunRequest, choice runProviderChoice) []policyPreviewPending {
	pending := []policyPreviewPending{previewCredential, previewAutonomy, previewToolApprovals, previewConfinement, previewEgress}
	if !req.Interactive && strings.TrimSpace(req.Task) == "" {
		pending = append(pending, previewTask)
	}
	if _, model := agentLLMProvider(req.Agent); model && createDoorIsModelRun(req) && !choice.chosen {
		pending = append(pending, previewProvider)
	}
	if req.Drive != nil && req.Drive.Enabled || len(req.Drives) > 0 {
		pending = append(pending, previewDrive)
	}
	return pending
}
