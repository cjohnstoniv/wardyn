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
	if t, who := actorFromRequest(r); t != types.ActorHuman || s.policyPreviewLimiter.allow(who, s.cfg.Now()) {
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
	var req createRunRequest
	if !s.decodeRunRequest(w, r, &req) {
		return
	}
	canonicalizeRunRepos(&req, s.adoHostsLoader(r.Context()))
	ceiling, reqCC, refusal := s.authorizePreviewRequest(r, req)
	if refusal.write(s, w, r) {
		return
	}
	ctx := r.Context()
	spec, _, warnings, source, refusal := s.resolveRunPolicyFacts(ctx, r, &req, true, false)
	if refusal.write(s, w, r) {
		return
	}
	if _, refusal = s.seedAuthorizedWorkspace(ctx, r, &spec, &req); refusal.write(s, w, r) {
		return
	}
	drive, refusal := s.authorizeRequestDrive(r, req, ceiling)
	if refusal.write(s, w, r) {
		return
	}
	if drive != nil && driveReadOnlyRefusal(req, *drive).write(s, w, r) {
		return
	}
	wsRefs := s.referencedWorkspaces(ctx, spec)
	unionPreviewWorkspaceEgress(&spec, wsRefs)
	present := s.presentSecretNamesFor(ctx, s.secretOwnerFromRequest(r))
	_ = s.applyWorkspaceRequirementsFor(ctx, present, &spec, req.Agent, wsRefs, resolveWorkspaceSelections(req))
	if _, refusal = s.unionDirectGitHubEgress(r, req, &spec, ceiling); refusal.write(s, w, r) {
		return
	}
	if _, err := enforcedConfinement(spec, reqCC, nil); err != nil {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonConfinementClassConflict, err.Error())
		return
	}
	choice, refusal := s.authorizeRunModelProvider(r, req, wsRefs, true)
	if refusal.write(s, w, r) {
		return
	}
	_, needsModel := agentLLMProvider(req.Agent)
	if needsModel && createDoorIsModelRun(req) {
		if name, secretName, found := modelEnvSecretGrant(spec); found {
			s.writeProviderRefusal(w, r, choice.provider.ID, choice.provider.Kind, fmt.Sprintf(mpRunModelEnvSecret, secretName, name), false)
			return
		}
	}
	site, err := s.scmLaneSiteConfig(ctx, spec, req.Repo)
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	narrowed, none := s.adoStandingAtDoor(r, spec, site, ceiling)
	if none {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonADOCapabilitiesNonePermitted, adoNonePermitted(spec.AzureDevOpsCapabilities))
		return
	}
	if reason, detail := s.patNarrowingAtDoor(r, spec, site); reason != "" {
		writeErrorReason(w, http.StatusUnprocessableEntity, reason, detail)
		return
	}
	response := policyPreviewFacts(req, spec, source, warnings, site, choice)
	if narrowed != "" {
		response.Warnings = append(response.Warnings, narrowed)
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
		return governanceCeiling{}, "", runError(http.StatusUnprocessableEntity, reasonAgentNotEnabled, msg)
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

// Only workspace declarations and their clone hosts are safe before dispatch;
// site-config, SSH, PAT and ADO host unions remain pending.
func unionPreviewWorkspaceEgress(spec *types.RunPolicySpec, workspaces []types.Workspace) {
	unionWorkspaceEgress(spec, workspaces)
	for _, ws := range workspaces {
		unionAllowedDomains(spec, workspaceCloneEgress(ws))
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
	if req.Drive != nil && req.Drive.Enabled {
		pending = append(pending, previewDrive)
	}
	return pending
}
