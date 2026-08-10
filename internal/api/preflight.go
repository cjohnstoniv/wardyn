// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

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
	spec, _, ok := s.resolveRunPolicy(ctx, w, r, &req, true)
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

	// Enforced confinement class — the SAME math launch runs, shared rather than
	// hand-copied (enforcedConfinement, called by resolveEnforcedConfinement in
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

	// Fold the run's model-access binding into the spec exactly as launch does
	// AFTER enforcement (runs.go) — the SAME foldRunIntegration, so the
	// model-access and egress checklist rows below see the creds the run will
	// really hold across the WHOLE precedence chain (explicit integration_id,
	// workspace binding, operator default), not just the workspace tier. No
	// audit event — preflight persists nothing (that is
	// applyPrimaryWorkspaceCreds's launch-only half).
	wsRefs := s.referencedWorkspaces(ctx, spec)
	_, _, _ = s.foldRunIntegration(ctx, &spec, req, wsRefs)
	// Fold each referenced workspace's requirements contract exactly as launch
	// does (runs.go) — the SAME applyWorkspaceRequirements call, so Review can
	// never predict a rosier (or stricter) outcome than launch actually applies.
	// Discarded, not audited: preflight persists nothing, mirroring the
	// foldRunIntegration call just above.
	_ = s.applyWorkspaceRequirements(ctx, &spec, req.Agent, wsRefs, resolveWorkspaceSelections(req))

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

	// LLM-access verdict on a COPY: reconcileLLMAccess drops orphaned grants in
	// place, but the manual-wizard launch (handleCreateRun) persists every grant on
	// the resolved spec, so the checklist must see the FULL spec — mutating a copy
	// keeps deriveSetupItems' view faithful to what launch stores. The grants
	// slice is CLONED because a struct copy shares the backing array, and
	// slices.DeleteFunc's drop zeroes the vacated tail in place — a shallow copy
	// would clobber the very spec the copy exists to protect. managed mirrors
	// dispatch's precedence (runs.go): a compose-mode managed token credentials a
	// claude run that has no resident subscription mount and no anthropic api-key
	// grant, so reflect that instead of a false "no model access".
	llmSpec := spec
	llmSpec.EligibleGrants = slices.Clone(spec.EligibleGrants)
	_, hasAnthropicKey := apiKeyGrantForHost(&llmSpec, "api.anthropic.com")
	subscriptionActive := specHasMountTarget(&llmSpec, claudeCredTarget)
	managed := req.Agent == "claude-code" && !subscriptionActive &&
		!hasAnthropicKey && s.managedInjectReady(req.Agent) &&
		(llmSpec.AllowAllEgress || len(llmSpec.AllowedDomains) > 0)
	var llmAccess *composeLLMAccess
	if note, provisioned := reconcileLLMAccess(&llmSpec, req.Agent, presentSecrets, s.subscriptionInjectEnabled(), managed); note != "" {
		llmAccess = &composeLLMAccess{Provisioned: provisioned, Note: note}
	}
	// Operator-configured Bedrock credentials the run automatically: dispatch's
	// resolveBedrockAuth OVERRIDES the per-run api-key selection at launch, so a
	// run that picked no api_key still authenticates. Ask the same resolver here
	// (ws=nil — the global config; a workspace can only narrow region/model, not
	// supply credentials) so the checklist and the wizard's no-model-access banner
	// stop telling an operator with working Bedrock access that they have none.
	if llmAccess == nil || !llmAccess.Provisioned {
		if ba := s.resolveBedrockAuth(ctx, req.Agent, subscriptionActive, true, nil); ba.ready {
			llmAccess = &composeLLMAccess{
				Provisioned: true,
				Note:        "Amazon Bedrock is configured by the operator (region " + ba.region + ", model " + ba.model + "); this run uses it automatically — no per-run API key is needed.",
			}
		}
	}

	items := s.deriveSetupItems(ctx, runInput, spec, presentSecrets, llmAccess, nil, composeSubscriptionState{})
	writeJSON(w, http.StatusOK, preflightResponse{
		SetupItems:               items,
		EnforcedConfinementClass: enforced,
		RiskAssessment:           riskItems,
		OverallRisk:              overallRisk,
	})
}
