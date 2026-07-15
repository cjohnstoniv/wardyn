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
// panel shows), plus the confinement class this run will ACTUALLY enforce after
// the policy floor + blast-radius raise. The manual wizard fires this when the
// operator enters Review so the checklist (secrets/workspaces/backend/egress)
// and the silent-CC3 raise the composer already surfaces are visible on the
// manual path too. Advisory only — the UI renders any error as a quiet
// "preflight unavailable" and never blocks Review.
type preflightResponse struct {
	SetupItems               []SetupItem            `json:"setup_items"`
	EnforcedConfinementClass types.ConfinementClass `json:"enforced_confinement_class"`
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
// resolveRunPolicy's 4xx set, the onboarded-workspace gate, and the confinement
// floor check below. Not reproduced (unreachable via the wizard body this
// endpoint serves): the agent-required 400, the BYOI image/devcontainer 400s,
// and the cloud_sts identity-provider 422 — launch still enforces all of them.
func (s *Server) handlePreflightRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req createRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Resolve the policy through the SAME chokepoint launch uses. resolveRunPolicy
	// writes its own 4xx (XOR violation, invalid inline spec, missing/reserved
	// secret 422) and returns ok=false when it has already responded, so Review
	// sees the real launch error, never a rosier one.
	spec, _, ok := s.resolveRunPolicy(ctx, w, r, &req, true)
	if !ok {
		return
	}

	// Same un-bypassable onboarding gate launch runs (runs.go): a non-onboarded
	// mount source or repo 422s here exactly as it would at create.
	if code, err := s.validateWorkspaceSources(ctx, spec); err != nil {
		writeError(w, code, "workspace: "+err.Error())
		return
	}

	// Which secrets actually exist (names only) — the SAME map compose builds
	// (compose.go), so the checklist's present/missing verdicts can never disagree
	// with the launch-time secret gate.
	presentSecrets := map[string]bool{}
	if s.cfg.Secrets != nil {
		if names, err := s.listUserSecretNames(ctx); err == nil {
			for _, n := range names {
				presentSecrets[n] = true
			}
		}
	}

	// Enforced confinement class — mirror runs.go's create path (runs.go:190-215):
	// the requested class when set (never weaker than the policy floor), else the
	// floor, then the deterministic blast-radius raise to CC3 for a write-capable /
	// third-party production credential. The runner-capability check is NOT repeated
	// (see the doc comment) — the backend checklist row covers it.
	reqCC, ccOK := parseConfinementClass(req.ConfinementClass)
	if !ccOK {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown confinement_class %q", req.ConfinementClass))
		return
	}
	enforced := spec.MinConfinementClass
	if reqCC != "" {
		if !confinementGE(reqCC, spec.MinConfinementClass) {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(
				"confinement_class %s is weaker than the policy minimum %s", reqCC, spec.MinConfinementClass))
			return
		}
		enforced = reqCC
	}
	if composer.RequiredConfinementFloor(spec) == types.CC3 && !confinementGE(enforced, types.CC3) {
		enforced = types.CC3
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

	// LLM-access verdict on a COPY: reconcileLLMAccess drops orphaned grants in
	// place, but the manual-wizard launch (handleCreateRun) persists every grant on
	// the resolved spec, so the checklist must see the FULL spec — mutating a copy
	// keeps deriveSetupItems' view faithful to what launch stores. managed mirrors
	// dispatch's precedence (runs.go): a compose-mode managed token credentials a
	// claude run that has no resident subscription mount and no anthropic api-key
	// grant, so reflect that instead of a false "no model access".
	llmSpec := spec
	_, hasAnthropicKey := apiKeyGrantForHost(&llmSpec, "api.anthropic.com")
	managed := req.Agent == "claude-code" && !specHasMountTarget(&llmSpec, claudeCredTarget) &&
		!hasAnthropicKey && s.managedInjectReady(req.Agent) &&
		(llmSpec.AllowAllEgress || len(llmSpec.AllowedDomains) > 0)
	var llmAccess *composeLLMAccess
	if note, provisioned := reconcileLLMAccess(&llmSpec, req.Agent, presentSecrets, s.subscriptionInjectEnabled(), managed); note != "" {
		llmAccess = &composeLLMAccess{Provisioned: provisioned, Note: note}
	}

	items := s.deriveSetupItems(ctx, runInput, spec, presentSecrets, llmAccess, nil, composeSubscriptionState{})
	writeJSON(w, http.StatusOK, preflightResponse{SetupItems: items, EnforcedConfinementClass: enforced})
}
