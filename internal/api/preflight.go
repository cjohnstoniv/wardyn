// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// preflightResponse is the POST /api/v1/runs/preflight body: the deterministic
// setup checklist deriveSetupItems produces (the SAME rows the compose Review
// panel shows), the confinement class this run will ACTUALLY enforce after the
// policy floor + blast-radius raise, and the deterministic risk grade. The
// manual wizard fires this when the operator enters Review so the checklist
// (secrets/workspaces/backend/egress), the silent-CC3 raise, and the risk
// assessment the composer already surfaces are all visible on the manual path
// too. Advisory only — the UI renders any error as a quiet "preflight
// unavailable" and never blocks Review.
type preflightResponse struct {
	SetupItems               []SetupItem            `json:"setup_items"`
	EnforcedConfinementClass types.ConfinementClass `json:"enforced_confinement_class"`
	// RiskAssessment/OverallRisk are composer.Grade/OverallLevel run on this run's
	// resolved spec — the IDENTICAL grader compose.go runs for the AI Run
	// Composer's Review (composeResponse carries the same two fields under the
	// same wire names). The manual wizard's Review renders the SAME RiskPanel +
	// HIGH-only acknowledgment gate from these, so the manual path can never show
	// a rosier picture than the AI Review would for the same spec — before this,
	// it showed no risk data at all, and "Edit in wizard" silently walked the
	// operator around the composer's HIGH-risk ack gate (pass2-coherence-report
	// finding N1).
	RiskAssessment []composer.RiskItem `json:"risk_assessment"`
	OverallRisk    composer.RiskLevel  `json:"overall_risk"`
	// Warnings is resolveRunPolicy's clamp-warning list — non-empty only when a
	// MEMBER authored an inline_policy that composer.Clamp bounded or
	// filterMemberGrants dropped a grant from. Surfaced here (never at launch, per
	// resolveRunPolicy's doc comment) so Review tells the member WHY their
	// inline_policy differs from what they typed, before they launch.
	Warnings []string `json:"warnings,omitempty"`
}

// handlePreflightRun is a DRY-RUN of handleCreateRun's resolution + gating: it
// resolves the run policy through the EXACT same resolveRunPolicy chokepoint (so
// an XOR violation, an unknown-secret 422, or an invalid inline spec surface as
// the real launch errors), computes the enforced confinement class the same way
// runs.go does (requested-vs-floor + blast-radius CC3 raise), and returns the
// deterministic setup checklist. It mints nothing, persists nothing, dispatches
// nothing.
//
// The runner-capability 422 launch hard-gates on is deliberately NOT duplicated
// here: deriveSetupItems' backend row reports that honestly instead, so a host
// that can't yet enforce the class shows a fixable checklist row on Review
// rather than a fatal error that blanks the panel. Reproduced launch gates:
// the run-explicit integration_id ai_provider check below, resolveRunPolicy's
// 4xx set, the workspace_id seed's 400/422s (unknown workspace, an
// image/devcontainer_repo XOR violation surfaced by a workspace's base_image,
// target collision), the onboarded-workspace gate, the workspace
// credential-binding fold, and the confinement floor check below. Not
// reproduced (unreachable via the wizard body this endpoint serves): the
// agent-required 400, the BYOI image/devcontainer 400s, and the cloud_sts
// identity-provider 422 — launch still enforces all of them.
func (s *Server) handlePreflightRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req createRunRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Same member custom-image denial launch runs (runs_create.go's
	// decodeAndValidateCreateRun): a preflight dry-run must refuse a member's
	// BYOI/devcontainer_repo request with the SAME 403 create would, not preview
	// a rosier checklist for a request that would be denied at launch.
	if s.denyMemberCustomImage(w, r, req) {
		return
	}
	// Same eager integration_id check launch runs (decodeAndValidateCreateRun,
	// runs_create.go): a typo or a non-ai_provider id 400s here exactly as it
	// would at launch, instead of silently resolving to nothing at
	// foldRunIntegration time below (previewing as "no model access" on
	// Review) and only failing for real once the operator clicks launch.
	if req.IntegrationID != "" {
		if in, ok := s.resolveIntegrationRef(ctx, req.IntegrationID); !ok || in.Category != types.IntegrationAIProvider {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("integration_id %q does not name an ai_provider integration", req.IntegrationID))
			return
		}
	}

	// Resolve the policy through the SAME chokepoint launch uses. resolveRunPolicy
	// writes its own 4xx (XOR violation, invalid inline spec, missing/reserved
	// secret 422) and returns ok=false when it has already responded, so Review
	// sees the real launch error, never a rosier one.
	spec, _, clampWarnings, ok := s.resolveRunPolicy(ctx, w, r, &req, true)
	if !ok {
		return
	}

	// Same workspace_id seeding launch runs (runs.go): an unknown workspace or a
	// base_image-triggered XOR violation fails here exactly as it would at
	// create, and the checklist below sees the attached workspace
	// (seedRequestWorkspace prepends it, so deriveSetupItems' workspace rows
	// match launch). ephemeralDirs is launch-only (WARDYN_EPHEMERAL_DIRS at
	// dispatch) — preflight dispatches nothing, so it's discarded here.
	if _, code, err := s.seedRequestWorkspace(ctx, &spec, &req); err != nil {
		writeError(w, code, "workspace_id: "+err.Error())
		return
	}
	// Same base_image XOR + builder-wired re-check launch runs (runs.go).
	if msg := s.validateImageBuildRequest(req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}

	// Same un-bypassable onboarding gate launch runs (runs.go): a non-onboarded
	// mount source or repo 422s here exactly as it would at create.
	if code, err := s.validateWorkspaceSources(ctx, spec); err != nil {
		writeError(w, code, "workspace: "+err.Error())
		return
	}

	// Which secrets actually exist (names only) — the SAME map compose builds.
	presentSecrets := s.presentSecretNames(ctx)

	// Fold the run's model-access binding AND each referenced workspace's
	// requirements contract into the spec BEFORE computing the enforced confinement
	// class and grading — the SAME order launch now uses (SPINE-2/SPINE-6), so a
	// workspace's integration:<id> requirement floors the run to CC3 here exactly
	// as it will at launch, and the risk grade below sees the fold's write
	// narrowing + grants rather than a pre-fold snapshot. foldRunIntegration folds
	// the WHOLE precedence chain (explicit integration_id, workspace binding,
	// operator default), not just the workspace tier. bedrockRef is KEPT (SPINE-5):
	// a bedrock integration that supplies the region/model must reach
	// resolveBedrockAuth below, or the checklist previews "no model access" for a
	// run launch credentials fine. No audit event — preflight persists nothing
	// (the run.workspace.creds audit is the create path's launch-only half).
	wsRefs := s.referencedWorkspaces(ctx, spec)
	_, _, bedrockRef := s.foldRunIntegration(ctx, &spec, req, wsRefs)
	_ = s.applyWorkspaceRequirements(ctx, &spec, req.Agent, wsRefs, resolveWorkspaceSelections(req))

	// Enforced confinement class — the SAME math launch runs, now on the FOLDED
	// spec (enforcedConfinement, called by resolveEnforcedConfinement in
	// runs_create.go), so preflight cannot drift from the launch gate. The tail
	// gates resolveEnforcedConfinement adds — the runner-capability check and the
	// cloud_sts grantChecker — are deliberately NOT repeated (see the doc comment);
	// the backend checklist row covers the first.
	reqCC, ccOK := parseConfinementClass(req.ConfinementClass)
	if !ccOK {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown confinement_class %q", req.ConfinementClass))
		return
	}
	enforced, err := enforcedConfinement(spec, reqCC)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// The RunInput deriveSetupItems keys off — the scalar create-run fields, with
	// the ENFORCED class so the backend row probes the class this run will really
	// run at (post-floor/raise), matching launch.
	runInput := composer.RunInput{
		Agent:            req.Agent,
		Repo:             req.Repo,
		Task:             req.Task,
		ConfinementClass: string(enforced),
		Interactive:      req.Interactive,
		DevcontainerRepo: req.DevcontainerRepo,
	}

	// Deterministic risk grade (N1 fix): the SAME composer.Grade/OverallLevel
	// call compose.go runs for the AI Run Composer's Review, on the SAME
	// resolved spec deriveSetupItems sees below — NOT the llmSpec copy just
	// below (that copy exists only so reconcileLLMAccess sees a droppable clone
	// of EligibleGrants; grading it would silently diverge from what the
	// checklist below is judging). Computed here, after both folds above, so a
	// workspace's requirements contract (egress/mounts) is graded exactly like
	// launch will enforce it — never before, never off a rosier snapshot.
	//
	// Graded on a copy whose MinConfinementClass is the ENFORCED class — the
	// grader reads spec.MinConfinementClass, not RunInput.ConfinementClass, and
	// compose.go achieves the same by mutating the clamped spec before grading
	// (its blast-radius raise). Grading the pre-raise floor printed "HIGH:
	// Fence, the weakest tier" on the same screen whose raise banner says the
	// run launches at Vault.
	gspec := spec
	gspec.MinConfinementClass = enforced
	riskItems := composer.Grade(runInput, gspec)
	overallRisk := composer.OverallLevel(riskItems)

	// LLM-access verdict on the resolved spec — the SAME computation the create path
	// warns from (resolveRunLLMAccess), so this checklist row and the launch-time
	// warning can never disagree. The helper clones internally: reconcileLLMAccess
	// drops orphaned grants in place, but launch persists every grant on the resolved
	// spec, so the checklist must keep seeing the FULL spec.
	llmAccess := s.resolveRunLLMAccess(ctx, req, spec, presentSecrets, bedrockRef)

	items := s.deriveSetupItems(ctx, runInput, spec, presentSecrets, llmAccess, nil, composeSubscriptionState{})
	writeJSON(w, http.StatusOK, preflightResponse{
		SetupItems:               items,
		EnforcedConfinementClass: enforced,
		RiskAssessment:           riskItems,
		OverallRisk:              overallRisk,
		Warnings:                 clampWarnings,
	})
}
