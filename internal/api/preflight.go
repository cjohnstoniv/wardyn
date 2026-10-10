// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/audit"
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
// too. The console shows an error as a danger alert beside Launch. A fresh
// 4xx, or a `missing` backend or llm_access row, holds Launch for that exact
// body for up to 60s (use-launch.ts preflightBlock); every other row is advisory.
type preflightResponse struct {
	SetupItems               []SetupItem            `json:"setup_items"`
	EnforcedConfinementClass types.ConfinementClass `json:"enforced_confinement_class"`
	// RiskAssessment/OverallRisk are composer.Grade/OverallLevel run on this run's
	// resolved spec — the IDENTICAL grader compose.go runs for the AI Run
	// Composer's Review (composeResponse carries the same two fields under the
	// same wire names). The manual wizard's Review renders the SAME RiskPanel +
	// HIGH-only acknowledgment gate from these, so the manual path can never show
	// a rosier picture than the AI Review would for the same spec — without
	// these fields, "Edit in wizard" would silently walk the operator around
	// the composer's HIGH-risk ack gate.
	RiskAssessment []composer.RiskItem `json:"risk_assessment"`
	OverallRisk    composer.RiskLevel  `json:"overall_risk"`
	// Warnings is resolveRunPolicy's clamp-warning list — non-empty only when a
	// MEMBER authored an inline_policy that composer.Clamp bounded or
	// filterUserGrants dropped a grant from. Surfaced here (never at launch, per
	// resolveRunPolicy's doc comment) so Review tells the member WHY their
	// inline_policy differs from what they typed, before they launch.
	Warnings []string `json:"warnings,omitempty"`
	// ModelCredential is WHERE this run's model credential will land and which
	// provider serves it, graded from the model provider the run chose
	// (runProviderChoice.modelCredential). The New Run rail states it verbatim,
	// replacing the unconditional "never written into the sandbox" claim.
	//
	// ABSENT for a run that makes no model call, for one no provider serves,
	// and on the 422 this handler answers for a person who has not connected
	// their credential, where there is no response body to carry it.
	ModelCredential *modelCredentialFacts `json:"model_credential,omitempty"`
	// Autonomy is what resolveRunAutonomy decided for this run — the same
	// object launch puts on its `run.create` audit row, from the same call, so
	// Review cannot show a level launch will not honour (0.8 #97).
	//
	// ABSENT rather than a zero value when nothing bound the run: no assigned
	// profile, no rubric on it, or a rubric that leaves this posture's three
	// fields unset. That is exactly the condition under which the audit row
	// omits its own field, which is what makes "Review returns what launch
	// audits" checkable instead of approximately true.
	Autonomy *types.AutonomyResolution `json:"autonomy,omitempty"`
	// GitCredential is THIS CALLER's Azure DevOps access state for THIS RUN's
	// OWN repositories only (review finding F2; gitCredentialFactForRepos,
	// scmaccess.go) — the per-user row (if any) that admits one of them, the
	// SAME row-selection gitCredentialRefusal itself uses. Never a refusal:
	// this handler answers 200 even when the state is not_configured, so the
	// rail can state it before Launch. Absent for a run whose repositories
	// touch no per-user Azure DevOps row at all (a GitHub-only run, a
	// deployment with no such row, or one whose only Azure DevOps row is
	// shared).
	GitCredential *SCMAccess `json:"git_credential,omitempty"`
	// Components is what the run is given access to (componentFacts), from the
	// spec and the gate's answers above — the preview's rows, with the
	// credential verdicts only this door reads. Absent for a run with no
	// repository on a Git provider and no component.
	Components []componentFact `json:"components,omitempty"`
	// Provenance says why each entry of the resolved spec is there; the policy
	// preview returns the same rows for the same request. Never null.
	Provenance []provenanceRow `json:"provenance"`
}

// preflightBurst and preflightLimiterMaxPeople size the per-person preflight
// limiter: a burst of five, and a map cap for a deployment's people rather
// than the directory limiter's admins.
const (
	preflightBurst            = 5
	preflightLimiterMaxPeople = 16384
)

// refusePreflightRate answers 429 when this person is over the preflight rate, true when it has
// answered. A person only; the admin token is one shared actor name, so limiting it would pool
// every CI job into one bucket.
func (s *Server) refusePreflightRate(w http.ResponseWriter, r *http.Request) bool {
	if s.preflightLimiter == nil {
		return false
	}
	if t, who := actorFromRequest(r); t != types.ActorHuman || s.preflightLimiter.allow(who, s.cfg.Now()) {
		return false
	}
	writeErrorReason(w, http.StatusTooManyRequests, reasonPreflightRateLimited, "too many preflight checks; slow down")
	return true
}

// advertisedConfinement is the classes the runner advertises, for Review's
// default-class math, best-effort: no runner, or one that cannot answer, is
// nil — the default then stays at the policy minimum and Review never refuses
// on it (handlePreflightRun).
func (s *Server) advertisedConfinement(ctx context.Context) []types.ConfinementClass {
	if s.cfg.Runner == nil {
		return nil
	}
	caps, err := s.cfg.Runner.Capabilities(ctx)
	if err != nil {
		return nil
	}
	return caps.ConfinementClasses
}

// handlePreflightRun is a DRY-RUN of handleCreateRun's resolution + gating: it
// resolves the run policy through the EXACT same resolveRunPolicy chokepoint (so
// an XOR violation, an unknown-secret 422, or an invalid inline spec surface as
// the real launch errors), computes the enforced confinement class the same way
// runs.go does (requested-vs-floor + blast-radius CC3 raise), and returns the
// deterministic setup checklist. It mints nothing and dispatches nothing.
//
// It does persist one thing. Every gate it
// reproduces is a real gate, and a gate that REFUSES a member writes its
// authz.denied audit row — refuse, from inside the shared code path.
// So a dry run that is refused (task_mode, the drive door, any other profile
// limit) writes a row per refused door, with run_id NULL because there is no
// run. A dry run that PASSES writes nothing at all.
//
// It stays that way deliberately rather than being suppressed: the row is the
// record that this principal was refused this capability, which is true whether
// or not they went on to launch, and the alternative — a gate that audits at
// one door and not at the identical door one handler over — is the drift the
// shared path exists to prevent. Every row written under this request carries
// data.dry_run: true, stamped by audit.DryRunRecorder from the context mark set
// below, because a NULL run_id does not tell a dry run apart: a launch refused
// before its run row exists carries one too. Review's re-resolve on every edit
// would write a row per keystroke for a member editing against a closed door,
// so audit.DenialCoalescer keeps the first identical refusal in full and
// appends one preflight.denial.coalesce row counting the repeats of the next
// ten minutes (per replica; docs/OPERATIONS.md).
//
// The runner-capability 422 launch hard-gates on is deliberately NOT duplicated
// here: deriveSetupItems' backend row reports that honestly instead, so a host
// that can't yet enforce the class shows a fixable checklist row on Review
// rather than a fatal error that blanks the panel. Reproduced launch gates:
// the run-explicit integration_id AI-provider check below, the provider
// admission + member capability over `repo`/`devcontainer_repo`
// (requestRepoProviderRefusals), the agent-roster 422 (agentRosterRefusal),
// resolveRunPolicy's 4xx set, the workspace_id seed's 400/422s (unknown workspace, an
// image/devcontainer_repo XOR violation surfaced by a workspace's base_image,
// target collision), the onboarded-workspace gate, the workspace
// credential-binding fold, and the confinement floor check below. Not
// reproduced (unreachable via the wizard body this endpoint serves): the
// agent-required 400, the BYOI image/devcontainer 400s, and the cloud_sts
// identity-provider 422 — launch still enforces all of them.
// TestPreflightMirrorsLaunchGates now scans THROUGH decodeAndValidateCreateRun
// rather than excepting the whole wrapper, so this inventory is executable gate
// by gate instead of wrapper by wrapper.
func (s *Server) handlePreflightRun(w http.ResponseWriter, r *http.Request) {
	// Marked before any gate runs, refuseAdminViewLaunch included: every audit row
	// written under this request carries dry_run (audit.DryRunRecorder).
	r = r.WithContext(audit.WithDryRun(r.Context()))
	ctx := r.Context()
	// First, before the decode and every gate: a limited call costs no work and
	// writes no row.
	if s.refusePreflightRate(w, r) {
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
	// The order of the five gates below is create's own order, and it is
	// load-bearing rather than tidy. The structural parity guard
	// (TestPreflightMirrorsLaunchGates) can see the gate SET but not the
	// sequence, so a body violating two gates would otherwise get a different
	// status from Review than from launch — Review's job is to answer the
	// refusal the caller is actually about to meet, not merely one of them.
	// decodeAndValidateCreateRun asks, in this sequence: the agent roster, the
	// member request denial, provider admission over the free-text repositories,
	// the free-text field caps, then the eager integration_id check.

	// The org roster: an agent this deployment does not offer 422s at launch, so
	// previewing it as a clean checklist is the same lie provider admission was.
	// Surfaced by narrowing this pair's parity exception rather than by a
	// second field report. With no AgentProviders block agentRosterRefusal
	// short-circuits to "" and Review is byte-for-byte what it was.
	if msg, rerr := s.agentRosterRefusal(ctx, req.Agent); rerr != nil {
		writeServerError(w, r, "get site config", rerr)
		return
	} else if msg != "" {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonAgentNotEnabled, msg)
		return
	}
	// Same member request-field denial launch runs (runs_create.go's
	// decodeAndValidateCreateRun): a preflight dry-run must refuse a member's
	// BYOI/devcontainer_repo/ungranted-workspace request with the SAME 403
	// create would, not preview a rosier checklist for a request that would be
	// denied at launch.
	ceiling, denied := s.denyUserRequest(w, r, req)
	if denied {
		return
	}
	// Same PROVIDER ADMISSION launch runs over the two FREE-TEXT repository
	// fields (decodeAndValidateCreateRun -> requestRepoProviderRefusals,
	// workspace_admission.go). Neither `repo` nor `devcontainer_repo` is a spec
	// entry, so the resolved-spec gate below never sees them — Review showed a
	// clean checklist for a repository POST /runs then refused, 422 for an
	// operator and 403 for a member.
	//
	// The SAME function, not a copy, so the two doors cannot answer different
	// refusals.
	//
	// It can write one audit row on the grace lane
	// (workspace.provider.admit, from admitRepoSources) — the same "a
	// refused dry run leaves the record of the refusal" rule this handler's doc
	// comment already states for refuse. In legacy open mode (no
	// provider rows) it reads the site config and returns having refused,
	// audited and warned nothing.
	if s.requestRepoProviderRefusals(w, r, req, false) { // false: Review never gates on git_credential (F2)
		return
	}
	// Same free-text field caps + control-character check launch runs over every
	// caller-supplied string on this body (runs_create_fields.go): one loop, one
	// 400 shape, shared so Review never previews a request the create door 400s.
	if !s.validateRunTextFields(w, req) {
		return
	}
	// Same integration_id refusal launch runs (decodeAndValidateCreateRun), so
	// Review never previews a request launch refuses.
	if req.IntegrationID != "" {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonIntegrationIDRetired, mpRunNoIntegration)
		return
	}

	// Every launch gate from policy resolution to the quota fit, through the
	// SAME fold launch runs (runFoldSteps) — called, not re-implemented, so a
	// gate added to launch reaches Review in the same commit and the same place
	// in the order. Its refusals are real ones with real authz.denied rows; what
	// it resolves for launch alone (the mount, ephemeral dirs, the frozen grades)
	// is discarded with this request. No reqCC: stepConfinement parses it in
	// launch's order.
	f, ok := s.foldRunRequest(w, r, foldPreflight, &req, ceiling, "")
	if !ok {
		return
	}
	spec, enforced, mpChoice, modelCred, autonomy := f.spec, f.enforced, f.mpChoice, f.modelCred, f.autonomy
	clampWarnings := f.policyWarns
	if mpChoice.renewAtLaunch {
		clampWarnings = append(clampWarnings, fmt.Sprintf(mpBRRenewAtLaunch, mpChoice.provider.ID))
	}
	// After renewAtLaunch: Review's warning order, so it stays at the door.
	if f.adoNarrowed != "" {
		clampWarnings = append(clampWarnings, f.adoNarrowed)
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
		Baseline:         f.baseline,
	}

	// Deterministic risk grade: the SAME composer.Grade/OverallLevel
	// call compose.go runs for the AI Run Composer's Review, on the SAME
	// resolved spec deriveSetupItems sees below — NOT the llmSpec copy just
	// below (that copy is a droppable clone of EligibleGrants; grading it would
	// silently diverge from what the checklist below is judging). Computed here, after both folds above, so a
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

	// LLM-access verdict — the SAME computation the create path warns from
	// (runLLMAccess), so this checklist row and the launch-time warning can
	// never disagree.
	//
	// Skipped for task_mode=exec, mirroring runNeedsModelWarning's own
	// exec gate at create time — an exec run runs a plain shell command, invokes
	// no model, and needs no credential, so resolving it unconditionally previewed
	// a false "missing model access" blocker on every CI exec job's --dry-run.
	var llmAccess *composeLLMAccess
	if req.TaskMode != "exec" {
		llmAccess = runLLMAccess(req, mpChoice)
	}

	items := s.deriveSetupItems(ctx, s.secretOwnerFromRequest(r), runInput, spec, f.present, llmAccess)

	// credentialConfinementAdvisory (#150): the SAME shared helper the create
	// path calls (appendCredentialConfinementAdvisory, runs_create.go), off the
	// SAME modelCred.Kind the choice above just graded — so Review can never
	// show a rosier picture than the launch it previews. WARN, never refuse:
	// nothing above this line changed.
	warnings, _ := appendCredentialConfinementAdvisory(clampWarnings, spec, enforced, modelCred.Kind)
	warnings = append(warnings, f.fitWarnings...)

	// A zero residency means no provider was chosen (none serves the agent, or a
	// non-model run) — omitted rather than published as a guess.
	resp := preflightResponse{
		SetupItems:               items,
		EnforcedConfinementClass: enforced,
		RiskAssessment:           riskItems,
		OverallRisk:              overallRisk,
		Warnings:                 warnings,
		Provenance:               f.prov,
	}
	if modelCred.Residency != "" {
		resp.ModelCredential = &modelCred
	}
	// Published on exactly the condition the audit row publishes on — a level
	// was actually resolved — so the two objects are comparable field for field.
	if autonomy.Level != "" {
		resp.Autonomy = &autonomy
	}
	// #386: the informational, PER-RUN git_credential fact (review finding
	// F2) — never blocking (this handler's own contract, doc comment above),
	// and never the gate: THIS run's own repos only, free-text and
	// workspace-resolved together, the same two sets requestRepoProviderRefusals
	// and seedAndAdmitWorkspace each gate at launch.
	runRepos := append([]string{req.Repo, req.DevcontainerRepo}, repoLocatorsOf(spec.WorkspaceRepos)...)
	resp.GitCredential = s.gitCredentialFactForRepos(ctx, oidcHumanFromContext(ctx), runRepos)
	resp.Components = componentFacts(req, spec, f.scmSite, f.comps, resp.GitCredential, f.baseline)
	writeJSON(w, http.StatusOK, resp)
}
