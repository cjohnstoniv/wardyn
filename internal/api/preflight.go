// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
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
	// a rosier picture than the AI Review would for the same spec — without
	// these fields, "Edit in wizard" would silently walk the operator around
	// the composer's HIGH-risk ack gate.
	RiskAssessment []composer.RiskItem `json:"risk_assessment"`
	OverallRisk    composer.RiskLevel  `json:"overall_risk"`
	// Warnings is resolveRunPolicy's clamp-warning list — non-empty only when a
	// MEMBER authored an inline_policy that composer.Clamp bounded or
	// filterMemberGrants dropped a grant from. Surfaced here (never at launch, per
	// resolveRunPolicy's doc comment) so Review tells the member WHY their
	// inline_policy differs from what they typed, before they launch.
	Warnings []string `json:"warnings,omitempty"`
	// ModelCredential is WHERE this run's model credential will land, graded from
	// the lanes the mechanism gate just resolved (gradeModelCredential). The New
	// Run rail states it verbatim, replacing the unconditional "never written
	// into the sandbox" claim.
	//
	// It OVERRIDES the /setup/status harness row's own residency, which is the
	// default-path answer graded against the deployment default policy: this one
	// is graded against the body the caller is actually about to launch, and
	// preflightIsCurrent compares that whole body — so a verdict for a different
	// agent can never render.
	//
	// ABSENT for a run that makes no model call, for a caller whose roster read
	// failed, and — the case worth naming — on the 422 this handler answers for a
	// per_user member who has not signed in, where there is no response body to
	// carry it. That is exactly why the status row exists as the default path.
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
// authz.denied audit row — denyMemberField, from inside the shared code path.
// So a dry run that is refused (task_mode, the drive door, any other profile
// limit) leaves exactly one row per refused door per call, with run_id NULL
// because there is no run. A dry run that PASSES writes nothing at all.
//
// It stays that way deliberately rather than being suppressed: the row is the
// record that this principal was refused this capability, which is true whether
// or not they went on to launch, and the alternative — a gate that audits at
// one door and not at the identical door one handler over — is the drift the
// shared path exists to prevent. What it costs is that Review's re-resolve on
// every edit can write a row per keystroke for a member editing against a
// closed door; the run_id NULL is what tells those apart from the denials that
// actually bounded a run.
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
	ctx := r.Context()
	var req createRunRequest
	if !decodeStrict(w, r, &req) {
		return
	}
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
		writeError(w, http.StatusUnprocessableEntity, msg)
		return
	}
	// Same member request-field denial launch runs (runs_create.go's
	// decodeAndValidateCreateRun): a preflight dry-run must refuse a member's
	// BYOI/devcontainer_repo/ungranted-workspace request with the SAME 403
	// create would, not preview a rosier checklist for a request that would be
	// denied at launch.
	ceiling, denied := s.denyMemberRequest(w, r, req)
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
	// (workspace.provider.legacy_host, from admitRepoSources) — the same "a
	// refused dry run leaves the record of the refusal" rule this handler's doc
	// comment already states for denyMemberField. In legacy open mode (no
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
	// Same eager integration_id check launch runs (decodeAndValidateCreateRun,
	// runs_create.go): a typo or a non-AI-provider id 400s here exactly as it
	// would at launch, instead of silently resolving to nothing at
	// foldRunIntegration time below (previewing as "no model access" on
	// Review) and only failing for real once the operator clicks launch.
	if req.IntegrationID != "" {
		if in, ok := s.resolveIntegrationRef(ctx, s.secretOwnerFromRequest(r), req.IntegrationID); !ok || !types.AIProviderKind(in.Kind) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("integration_id %q does not name an AI provider integration", req.IntegrationID))
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

	// The SAME seed-and-admit block launch runs — called, not re-implemented.
	// seedAndAdmitWorkspace (runs.go) is the one place that owns the four gates
	// seeding can invalidate, in order: the workspace_id seed's 400/422s, the G3
	// post-seed capability re-check, the base_image XOR + builder-wired
	// re-check, and the un-bypassable onboarded-source gate. Inlining all four
	// verbatim would let a fifth gate added to launch's block reach Review only
	// if someone remembered to copy it — the exact drift
	// TestPreflightMirrorsLaunchGates now refuses. ephemeralDirs is launch-only
	// (WARDYN_EPHEMERAL_DIRS at dispatch) — preflight dispatches nothing, so it
	// is discarded here.
	if _, ok := s.seedAndAdmitWorkspace(ctx, w, r, &spec, &req, false); !ok { // false: F2, ditto
		return
	}

	// Same USER DRIVE resolution launch runs, in the same place in the order
	// (runs.go). The mount itself is launch-only — preflight dispatches nothing
	// — but the REFUSALS are the point: a member who ticked "mount my drive" and
	// has no allocation, or whose profile shuts the door, must read that on
	// Review rather than discover it at launch. Discarding the mount and keeping
	// the gate is exactly what this handler does with ephemeralDirs above.
	if _, ok := s.seedRequestDrive(w, r, req, ceiling); !ok {
		return
	}

	// Which secrets actually exist (names only) — the SAME map compose builds.
	presentSecrets := s.presentSecretNamesFor(ctx, s.secretOwnerFromRequest(r))

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
	// Widen the spec's egress from onboarded-workspace registries +
	// clone hosts the SAME way launch-time unionRunEgress does (runs.go),
	// side-effect-free (no audit — mirrors the fold's own "preflight persists
	// nothing" note above) — otherwise this preview graded/checklisted a
	// NARROWER envelope than the run will actually be launched with (launch
	// widens AFTER preflight would have graded), so Review understated what
	// the run gets. unionRunEgress's SSH/site-config/ADO SCM lanes are
	// deliberately NOT repeated here: those need grantWiring, which does not
	// exist yet at preflight time (no grants have been minted) — the
	// workspace + clone-host union below is the lane that can silently
	// diverge without any grant ever being involved.
	unionWorkspaceEgress(&spec, wsRefs)
	for _, ws := range wsRefs {
		unionAllowedDomains(&spec, workspaceCloneEgress(ws))
	}
	_, _, bedrockRef := s.foldRunIntegration(ctx, s.secretOwnerFromRequest(r), &spec, req, wsRefs)
	_ = s.applyWorkspaceRequirementsFor(ctx, presentSecrets, &spec, req.Agent, wsRefs, resolveWorkspaceSelections(req))

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
	// Best-effort capabilities read for the same reason enforcedConfinement now
	// needs them: an unspecified request's default is the strongest advertised
	// class, not the bare policy minimum. Never refused on here — a nil
	// Runner or a Capabilities error just leaves the default at the policy
	// minimum, matching this handler's "advisory only, never blocks Review"
	// contract; the runner-capability REFUSAL stays un-reproduced (doc comment
	// above), reported by the checklist's backend row instead.
	var advertised []types.ConfinementClass
	if s.cfg.Runner != nil {
		if caps, cerr := s.cfg.Runner.Capabilities(ctx); cerr == nil {
			advertised = caps.ConfinementClasses
		}
	}
	enforced, err := enforcedConfinement(spec, reqCC, advertised)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// The SAME autonomy gate launch runs, in the same place in the order
	// (runs.go) and on the same folded spec + enforced class — called, not
	// re-implemented, because a Review that previewed a level launch then
	// refused is the one lie this feature cannot afford. Its 403s are real
	// refusals with real authz.denied rows, the same way the drive door's are.
	// The derived tool_approvals write lands on this handler's own request
	// copy and is discarded with it (preflight dispatches nothing); the 201
	// warnings belong to the launch channel, and the site-config snapshot to
	// launch's egress union, so both are dropped here too.
	// The frozen Azure DevOps grade is dropped with the rest: preflight
	// dispatches nothing, so there is no dispatch for it to bind.
	autonomy, _, _, _, ok := s.resolveRunAutonomy(w, r, &req, spec, wsRefs, enforced, ceiling)
	if !ok {
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

	// Deterministic risk grade: the SAME composer.Grade/OverallLevel
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

	// Create/Review parity on the declared model-access mechanism: Review answers
	// the SAME 422 launch would, from the same predicate — a checklist that said
	// "ready" for a run create refuses is the worse of the two lies. Writes its
	// own 422; see enforceCreateLLMMechanism.
	// The caller's own run-identity subject, for the reason named at the create
	// door (runs.go): a per_user lane resolves against the principal's namespace,
	// and secretOwnerFromRequest's "" for an operator would preview "sign in
	// again" for an admin whose own capture is right there.
	//
	// The out-param is this handler's ONE resolution of the run's credential
	// lanes: the gate already resolves them to judge the declared mechanism, and
	// grading residency from a second resolution would both cost another
	// secret-store read and let the rail describe a lane the gate did not judge.
	ssoSubject := runIdentitySubject(ctx, principalFromRequest(r))
	// The model-provider choice, where launch makes it (runs.go).
	if !s.enforceRunModelProvider(w, r, req, wsRefs) {
		return
	}
	var modelCred modelCredentialFacts
	if !s.enforceCreateLLMMechanism(ctx, w, req, spec, bedrockRef, ssoSubject, &modelCred, false) {
		return
	}

	// LLM-access verdict on the resolved spec — the SAME computation the create path
	// warns from (resolveRunLLMAccess), so this checklist row and the launch-time
	// warning can never disagree. The helper clones internally: reconcileLLMAccess
	// drops orphaned grants in place, but launch persists every grant on the resolved
	// spec, so the checklist must keep seeing the FULL spec.
	//
	// Skipped for task_mode=exec, mirroring runNeedsModelWarning's own
	// exec gate at create time — an exec run runs a plain shell command, invokes
	// no model, and needs no credential, so resolving it unconditionally previewed
	// a false "missing model access" blocker on every CI exec job's --dry-run.
	var llmAccess *composeLLMAccess
	if req.TaskMode != "exec" {
		llmAccess = s.resolveRunLLMAccess(ctx, req, spec, presentSecrets, bedrockRef, ssoSubject)
	}

	items := s.deriveSetupItems(ctx, s.secretOwnerFromRequest(r), runInput, spec, presentSecrets, llmAccess)

	// credentialConfinementAdvisory (#150): the SAME shared helper the create
	// path calls (appendCredentialConfinementAdvisory, runs_create.go), off the
	// SAME modelCred.Mechanism the gate above just graded — so Review can never
	// show a rosier picture than the launch it previews. WARN, never refuse:
	// nothing above this line changed.
	warnings, _ := appendCredentialConfinementAdvisory(clampWarnings, spec, enforced, modelCred.Mechanism)

	// A zero residency means nothing was graded (no store, an unreadable roster,
	// a non-model run) — omitted rather than published as a guess, which leaves
	// the rail on the /setup/status row it already had.
	resp := preflightResponse{
		SetupItems:               items,
		EnforcedConfinementClass: enforced,
		RiskAssessment:           riskItems,
		OverallRisk:              overallRisk,
		Warnings:                 warnings,
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
	writeJSON(w, http.StatusOK, resp)
}
