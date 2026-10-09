// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// imageBuildTimeout bounds a per-run sandbox image build (BYOI wrap, devcontainer,
// workspace profile). The build is detached from the request ctx so a client
// disconnect cannot abort it — which leaves it needing a deadline of its own, or a
// wedged docker pull would hold the detached launch forever. Generous: a cold
// devcontainer build pulls a base image and runs the repo's full setup.
const imageBuildTimeout = 30 * time.Minute

// DRAFT (M2 canon pending)
const (
	// createRunGrantAbortHint is the failure_hint a run carries when POST /runs
	// answered 500 AFTER the run row existed: the row is persisted and the
	// identity already minted, so the compensator has to say WHY the badge reads
	// FAILED. The driver's own error text is in the 500 body and in the audit
	// row; this is the one line the console shows under the badge.
	createRunGrantAbortHint = "the run's credential grants could not be recorded, so it was never started"
	// createRunComponentsAbortHint is the same for the run's component snapshot.
	createRunComponentsAbortHint = "the run's components could not be recorded, so it was never started"
	// devcontainerNoBuilderWarning is the 201 warning for a devcontainer_repo run
	// on a deployment with no image builder wired: the build silently fell
	// through to the convention image, which is a different sandbox from the one
	// the caller asked for. Not a refusal — a hard fail would break every
	// no-builder deployment that has been launching this way.
	devcontainerNoBuilderWarning = "devcontainer_repo was ignored: no image builder is wired, so this run launches on the convention agent image instead of a devcontainer build"
)

// createRunRequest is the POST /api/v1/runs body. It is a TYPE ALIAS for the
// public SDK's request DTO — the SDK's declaration IS the server's declaration,
// so the wire contract is single-sourced and the compiler (not a parity test)
// enforces that the CLI, the SDK, and this handler all read/write the same
// fields. Field docs live on pkg/client.CreateRunRequest. The compile-time
// identity is pinned by TestCreateRunRequest_IsClientDTOAlias
// (internal/api/dto_alias_test.go); reverting this to a struct breaks that test.
type createRunRequest = client.CreateRunRequest

// parseConfinementClass validates a create-run request's confinement_class. An
// empty string is allowed (the caller inherits the policy minimum). A non-empty
// value must be one of the known classes (types.CC1/CC2/CC3); anything else is
// rejected (ok=false) so the handler can fail closed with HTTP 400.
func parseConfinementClass(s string) (types.ConfinementClass, bool) {
	if s == "" {
		return "", true
	}
	cc := types.ConfinementClass(s)
	if cc.Rank() == 0 { // unrecognised values rank 0 — see ConfinementClass.Rank
		return "", false
	}
	return cc, true
}

// warnWorkspaceCollision returns an advisory (never-blocking) warning when
// another non-terminal run already operates on workspacePath — two independent
// agents sharing a host directory could interfere — and records a collision
// audit event. Best-effort: an empty path, a store list error, or no collision
// yields no warning and the run still launches.
//
// The read is the question, not the whole table: the predicate is two
// columns, and Postgres can answer it with a WHERE (ActiveRunsAtPathReader)
// rather than a Seq Scan plus a full sort of agent_runs on EVERY run create,
// over a table nothing prunes and no retention policy bounds, to produce a
// sentence that is usually not printed. The in-Go fallback below exists for
// the test doubles that are not PG: the answer is identical either way, which
// is what makes an unconditional fallback safe here and not on the
// ownership-scoped list.
func (s *Server) warnWorkspaceCollision(r *http.Request, runID uuid.UUID, workspacePath string) []string {
	if workspacePath == "" {
		return nil
	}
	ctx := r.Context()
	// Two lists, and the split is the point. `others` is every colliding run and
	// belongs to the AUDIT row: an operator investigating two agents that fought
	// over a directory needs all of them, and that row is written by wardynd
	// (ActorSystem) for operators, not returned to the caller. `visible` is what
	// this CALLER may already see — the ownsRunOrAdmin rule handleObservedEgress
	// applies to the same class of cross-user run telemetry — and it is the only
	// half that reaches the response.
	//
	// This response must never enumerate every active run id at the path: any
	// member could otherwise learn another principal's run ids (and, by
	// repeating the create, watch them come and go) from an ADVISORY sentence.
	var others, visible []string
	note := func(e types.AgentRun) {
		others = append(others, e.ID.String())
		if s.ownsRunOrAdmin(r, e) {
			visible = append(visible, e.ID.String())
		}
	}
	if pr, ok := s.cfg.Store.(store.ActiveRunsAtPathReader); ok {
		active, lerr := pr.ActiveRunsAtWorkspacePath(ctx, workspacePath)
		if lerr != nil {
			return nil
		}
		for _, e := range active {
			// The state and the path are the STORE's now; only "not me" is left,
			// and it stays here because the run being created is this handler's
			// fact rather than a column the query could have filtered on.
			if e.ID != runID {
				note(e)
			}
		}
	} else {
		existing, lerr := s.cfg.Store.ListRuns(ctx)
		if lerr != nil {
			return nil
		}
		for _, e := range existing {
			if e.ID != runID && e.WorkspacePath == workspacePath && !isTerminalRunState(e.State) {
				note(e)
			}
		}
	}
	if len(others) == 0 {
		return nil
	}
	// Audited whenever it happens, not only when it is said out loud: the
	// operator's record of a collision must not shrink because the caller who
	// caused it owns none of the runs it collided with.
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.workspace.collide",
		workspacePath, "success", mustJSON(map[string]any{"other_runs": others})))
	if len(visible) == 0 {
		// Nothing to name that this caller may see. The advisory sentence is
		// canon and names its ids, so it is not re-worded here to carry a bare
		// count — a member-facing string change is the canon pass's, and it is
		// filed as one.
		return nil
	}
	return []string{fmt.Sprintf(
		"host workspace %q is already in use by %d active run(s) (%s); independent agents sharing a directory can interfere — proceeding anyway",
		workspacePath, len(visible), strings.Join(visible, ", "))}
}

// handleCreateRun validates policy, gates on confinement class against what the
// runner can actually enforce (fail closed), persists the run + its grants,
// mints the run identity, and (if a runner is wired) dispatches the sandbox.
// Without a runner the run stays PENDING with a clear status message (headless
// API-only operation is allowed for v0).
//
//nolint:funlen // Deliberate: one linear gate sequence whose ORDER is the contract (TestPreflightMirrorsLaunchGates reads it and its registered wrappers), so extracted gates remain visible to that guard. Each gate already lives in its own function; low branching, passes gocyclo/gocognit, just long.
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.refuseAdminViewLaunch(w, r) {
		return
	}
	req, ceiling, reqCC, taskWarning, ok := s.decodeAndValidateCreateRun(w, r)
	if !ok {
		return
	}

	// Resolve the policy: inline_policy (validated here), explicit policy_id, or
	// the configured default. resolveRunPolicy writes its own HTTP error and
	// returns ok=false when it has already responded (XOR violation, invalid
	// inline spec, missing/reserved inline secret ref, …) so we just stop.
	// Its clamp/capability warnings ride the 201 (see policyWarns below): a
	// member whose egress host, secret grant or workspace repo was narrowed
	// away has to hear about it from the thing they actually called.
	spec, policyID, policyWarns, policySource, ok := s.resolveRunPolicy(ctx, w, r, &req, false)
	if !ok {
		return
	}
	// Fold the named workspace onto the resolved spec, then re-run every check
	// that seeding can invalidate. Writes its own error and stops on false.
	ephemeralDirs, ok := s.seedAndAdmitWorkspace(ctx, w, r, &spec, &req, true) // true: launch, #386's gate applies
	if !ok {
		return
	}

	// Resolve the run's USER DRIVE, AFTER the onboarding gate above: the drive
	// is per-principal and never appears in the spec, so it deliberately does
	// not pass through validateWorkspaceSources — but it must not be able to
	// answer BEFORE the un-bypassable gate either. Writes its own 403/422 and
	// stops on false. nil for the overwhelming majority of runs (no `drive`
	// flag), which is a provable no-op: no store read at all.
	driveMount, ok := s.seedRequestDrive(w, r, req, ceiling)
	if !ok {
		return
	}

	// Fold each referenced workspace's requirements contract into the spec
	// BEFORE the confinement floor + risk grade read it. The deterministic CC3
	// blast-radius floor is computed from spec.EligibleGrants, so a workspace's
	// integration:<id> requirement — a third-party/production api_key grant —
	// must be present when the floor is computed, or invariant 5's "powerful
	// credentials run in the strongest sandbox" is silently bypassed. wsRefs is
	// derived from the seeded spec's mounts/repos, which nothing below mutates, so
	// it is equally valid here and is reused for the egress union + image
	// resolution. The fold is the AUDIT-FREE half (applyWorkspaceRequirements
	// returns its events) — the audit is recorded once the run id is minted, below.
	wsRefs := s.referencedWorkspaces(ctx, spec)
	// Caller-scoped secret namespace, resolved once: the presence map and the
	// requirement fold read the SAME one. Shared with the warning below.
	present := s.presentSecretNamesFor(ctx, s.secretOwnerFromRequest(r))
	reqEvents := s.applyWorkspaceRequirementsFor(ctx, present, &spec, req.Agent, wsRefs, resolveWorkspaceSelections(req))
	directGitHubAdded, refusal := s.unionDirectGitHubEgress(r, req, &spec, ceiling)
	if refusal.write(s, w, r) {
		return
	}
	// The run's components, bounded and expanded onto the spec here: after the
	// folds above, so a host an admin already credentialed is seen; before the
	// floor, the model-provider choice and the autonomy grade below, which read
	// what a component adds. See applyRunComponents.
	comps, refusal := s.applyRunComponents(r, req, &spec, ceiling, wsRefs, true)
	if refusal.write(s, w, r) {
		return
	}

	// The primary host workspace directory this run will operate in (if any), used
	// below to DISCOURAGE — warn, never block — sharing a directory with another
	// active run.
	workspacePath := primaryWorkspacePath(spec)

	// Resolve + gate the confinement class (request vs policy floor, the CC3
	// blast-radius floor now computed on the FOLDED spec, runner capability
	// membership, cloud_sts grant gating) — invariant 5, fail closed.
	baseline, ok := s.baselineOr500(w, r)
	if !ok {
		return
	}
	enforced, ok := s.resolveEnforcedConfinement(ctx, w, confinementFloorSpec(spec, comps), reqCC, baseline)
	if !ok {
		return
	}

	// The model-provider choice: the run's provider decides its lane, and one
	// whose credential its owner does not hold is refused here rather than at
	// dispatch. mpChoice is this run's ONLY source for ModelProviderID below and
	// for the run.create audit snapshot (#527) — mpChoice.chosen is false, with
	// a zero mpChoice.provider, when no provider serves this agent, so
	// ModelProviderID freezes "" there.
	//
	// Ahead of the autonomy gate because that gate grades THIS resolution: the
	// Bedrock model credential is handed to the run at dispatch, and a secrets
	// axis graded without it froze the level a rung too high (#504).
	mpChoice, ok := s.enforceRunModelProvider(w, r, req, spec, wsRefs, true)
	if !ok {
		return
	}
	// The chosen provider's arm, or nothing, credentials the run; a Bedrock
	// provider's run is graded with its owner's Bedrock credential.
	modelCred := mpChoice.modelCredential()

	// Posture-gated autonomy, resolved ONCE and enforced at both doors
	// (handlePreflightRun calls the SAME gate). Sited after the enforced class
	// because that class is the posture's third axis, after the model-credential
	// gate because its resolution is graded on the secrets axis — and before the
	// mint, so a refusal leaves no run row, and before
	// createRunAuditData and the dispatchParams literal below, which both read
	// the req.ToolApprovals this gate may derive to `hold`. Writes its own 403
	// and stops on false; its warnings join the 201 list further down.
	// scmSite is the one site-config snapshot the gate graded the SCM-host lane
	// from; unionRunEgress below dispatches from the same value.
	// adoGrade is what the gate RESOLVED about the per-person Azure DevOps lane;
	// it rides to dispatch on the ceiling so the credential dispatch authors can
	// only be the one this level was graded against (adoEntraGrade).
	// bedrockGrade is the same freeze for the Amazon Bedrock model credential
	// the gate graded from modelCred (bedrockCredGrade).
	autonomy, autonomyWarns, scmSite, adoGrade, bedrockGrade, ok := s.resolveRunAutonomy(w, r, &req, spec, wsRefs, enforced, ceiling, modelCred, comps)
	if !ok {
		return
	}
	// A member's bounded Azure DevOps list is said out loud on the 201 and the
	// run.create row (#1384); the none-permitted refusal is dispatch's, which
	// preflight mirrors (adoStandingAtDoor).
	if narrowed, _ := s.adoStandingAtDoor(r, spec, scmSite, ceiling); narrowed != "" {
		policyWarns = append(policyWarns, narrowed)
	}

	// Host capacity, the last refusal and before the mint, the same siting as
	// the autonomy gate: a refusal leaves no identity and no run row.
	if writeHostCapacityRefusal(w, r, s.admitHostCapacity(r.Context(), principalFromRequest(r), "runs", true)) {
		return
	}

	// The deployment run cap, refused before the mint for the same reason: no
	// identity.mint row and no live token for a run that gets no row.
	// CreateRunUnderCap (createRun) still decides a race at the cap.
	if s.refuseRunCapFull(w, r) {
		return
	}

	// The runs namespace's ResourceQuota, refused before the mint for the same reason: a run
	// the quota cannot hold leaves no identity, no run row and no sandbox. The advisories
	// join the 201's warnings. The quota's own admission stays the authority on a race.
	fitWarnings, refused := s.refuseRunFit(w, r, s.runFitSpec(ctx, spec, ceiling))
	if refused {
		return
	}

	createdByType, createdBy := actorFromRequest(r)
	runID := uuid.New()
	// Subject vs attribution: createdBy is the ATTRIBUTION — the run row's
	// CreatedBy, the sponsor claim, every audit actor — and in LocalMode it may be
	// the DEV-ONLY X-Wardyn-Principal header. The SUBJECT is what selects the
	// secret namespace at mint/inject time, so it comes from runIdentitySubject,
	// which no request header can move.
	operatorOwned := operatorOwnedRequest(ctx)
	id, err := s.cfg.Identity.MintRunIdentity(ctx, runID, runIdentitySubject(ctx, createdBy), createdBy, internalAudience, operatorOwned)
	if err != nil {
		writeServerError(w, r, "mint run identity", err)
		return
	}

	now := s.cfg.Now().UTC()
	run := types.AgentRun{
		ID:               runID,
		CreatedAt:        now,
		UpdatedAt:        now,
		CreatedBy:        createdBy,
		Agent:            req.Agent,
		Repo:             req.Repo,
		Task:             req.Task,
		Title:            strings.TrimSpace(req.Title),
		Description:      strings.TrimSpace(req.Description),
		PolicyID:         policyID,
		ConfinementClass: enforced,
		State:            types.RunPending,
		SPIFFEID:         id.SPIFFEID,
		RunnerTarget:     s.cfg.RunnerTarget,
		Interactive:      req.Interactive,
		WorkspacePath:    workspacePath,
		WorkspaceIDs:     workspaceIDsOf(wsRefs), // referencedWorkspaces above, same spec as WorkspacePath
		AutoStopAfterSec: spec.AutoStopAfterSec,
		// The LEVEL only. The posture and the bound field are provenance and
		// live on the create audit row (AgentRun.AutonomyLevel's own doc);
		// freezing them on the row would mirror a computation into a column
		// nothing reads back.
		AutonomyLevel: autonomy.Level,
		// The id alone — mpChoice.provider.Kind rides on the audit snapshot
		// below instead (AgentRun.ModelProviderID's own doc explains why).
		// mpChoice.provider.ID is "" when mpChoice.chosen is false.
		ModelProviderID: mpChoice.provider.ID,
		UserType:        runCreatorUserType(ctx),
		Preset:          req.Preset,
		PresetVersion:   req.PresetVersion,
		OperatorOwned:   operatorOwned,
		CreatedVia:      createdVia(ctx),
	}
	s.captureRunLimits(&run, ceiling)
	created, err := s.createRun(ctx, run)
	if err != nil {
		s.cfg.Identity.RevokeRun(context.WithoutCancel(ctx), runID) //nolint:errcheck // best-effort cleanup of the minted-but-unused token
		writeServerError(w, r, "create run", err)
		return
	}

	// From here on a run row exists, so no early return may simply answer and
	// walk away: the row is PENDING, the identity minted above is live, and
	// nothing downstream will notice — finalizeUndispatchedRuns only reaps it
	// after undispatchedGrace. Every post-CreateRun early return goes
	// through abort, which is the same compensation launchRecordRun's own
	// abort() has made (workspace_run_launch.go, pinned by
	// TestLaunchRecordRun_CreateGrantFailureFinalizesRun).
	//
	// WithoutCancel lives INSIDE the closure, not at the client-disconnect
	// detach further down: the compensator runs on the REQUEST's context, and a
	// 500 is very often being answered to a client that has already gone — its
	// cancelled ctx cannot write the FAILED state the compensator exists to
	// write, so the run would strand PENDING with un-revoked credentials on
	// exactly the path this closure exists for. Detaching here and again below
	// is harmless (WithoutCancel of a detached ctx is a no-op in effect) and
	// keeps the two concerns independent.
	abort := s.abortHalfBuiltRun(ctx, runID)

	// The components this run launched with, recorded before anything else is
	// built on the row: a revive re-checks the doors they needed from this
	// snapshot, so a run that could not record it must not start.
	if err := s.persistRunComponents(ctx, runID, comps); err != nil {
		abort(createRunComponentsAbortHint)
		writeServerError(w, r, "record run components", err)
		return
	}

	// resolveRunPolicy's own notes come FIRST: they are the ones that say the
	// run is narrower than what the caller asked for, and launch is the ONLY
	// place a member sees that — preflight, which carries the same notes, is
	// never called by the console. The strings are
	// narrowUserInlinePolicy's/filterUserGrants' own: they name the kind
	// and the dropped VALUE (a host, a secret NAME), never a secret value.
	warnings := withUnpublishedImageWarning(append(policyWarns, s.warnWorkspaceCollision(r, runID, workspacePath)...), req.Agent, s.cfg.AgentImages)
	if taskWarning != "" {
		warnings = append(warnings, taskWarning)
	}
	// The autonomy gate's own sentences (a derived hold; an agent with no
	// agent-side tool-approval lane), raised before the run id existed and
	// carried here — this is the only channel that reaches the caller.
	warnings = append(warnings, autonomyWarns...)
	// The two things provider admission ADMITTED rather than refused; see
	// repoSourceWarnings.
	warnings = append(warnings, s.repoSourceWarnings(ctx, runID, spec, req)...)
	warnings = append(warnings, fitWarnings...)

	// The requirements fold that ran ABOVE the confinement floor, audited now
	// that the run id exists. See recordCreateFolds.
	s.recordCreateFolds(ctx, runID, reqEvents)

	// Persist the eligibility records + derive the non-secret sandbox wiring
	// (github/git_pat/ssh grant ids, api_key proxy injections, SCM egress) —
	// see persistRunGrants. A grant write failure has already answered 500.
	gw, ok := s.persistRunGrants(ctx, w, r, runID, now, spec)
	if !ok {
		abort(createRunGrantAbortHint)
		return
	}
	// Provider-lane honesty, the same shape one line earlier in the pipeline: a
	// credential lane the deployment's provider row does not permit was not wired,
	// and the 201 says so (persistRunGrants collected the sentences as it declined
	// each one).
	warnings = append(warnings, gw.warnings...)
	// SSH-lane honesty: drop the wiring for agents with no SSH clone lane
	// (codex-cli) and advise BYOI images about the required tools.
	warnings = append(warnings, s.applySSHLaneWarnings(ctx, req, runID, &gw)...)

	// Git-broker: map the run's declared GitHub clone set to its github grant so the
	// proxy's /wardyn/gh/ route serves exactly these repos (github.com is dropped
	// from egress; an un-granted github repo is denied).
	gw.augmentGitBrokerGrants(req.Repo, spec.WorkspaceRepos)

	// credentialConfinementAdvisory (#150): a run whose model credential just
	// graded as the captured-AWS-SSO lane (modelCred.Kind, above) but
	// whose enforced confinement is weaker than CC3 gets that said on every
	// surface a person or an incident review reads — the 201, and (below) the
	// audit row's closed-vocabulary credential_confinement field. WARN, never
	// refuse: RequiredConfinementFloor above is untouched, on purpose.
	warnings, belowFloor := appendCredentialConfinementAdvisory(warnings, spec, enforced, modelCred.Kind)

	createData := createRunAuditData(req, policyID, enforced, reqCC, id.JTI, policyWarns, autonomy, belowFloor, mpChoice)
	createData["policy_source"] = policySource
	comps.stampRunCreate(createData)
	s.markGovernanceExempt(ctx, createData)
	s.recordAudit(ctx, s.auditEvent(&runID, createdByType, createdBy, "run.create",
		runID.String(), "success", mustJSON(withRunUserType(ctx, run.UserType, createData))))
	s.auditRunComponents(ctx, runID, createdByType, createdBy, comps)

	// Model-resolution fail-fast, as a warning; see noModelAccessWarning.
	warnings = append(warnings, noModelAccessWarning(req, mpChoice)...)

	// Widen the RESOLVED spec's egress from the deterministic operator-trusted
	// sources (onboarded-workspace registries, site-config SCM hosts, the SSH and
	// ADO SCM lanes) — never the LLM; see unionRunEgress.
	s.unionRunEgress(ctx, runID, &spec, gw, wsRefs, req.Repo, scmSite, directGitHubAdded, comps)

	// …and say so when one of those operator-approved workspace hosts is walled
	// off by the caller's own governance profile. The union above still happened
	// and the deny still wins at the proxy: this is the sentence that keeps the
	// mid-run refusal from arriving as a support ticket.
	warnings = append(warnings, warnCeilingDeniedWorkspaceEgress(ceiling, wsRefs)...)

	// …and the LATE drop nothing else says: a STORED policy is handed to dispatch
	// verbatim (resolvePolicy), so a workspace_repos target the write door now
	// refuses — /home/agent/drive, a reserved path — still reaches
	// buildRepoRecords, which drops the repo. Without this, that would produce
	// a 201, an empty WARDYN_REPOS and an agent hunting for a repo that was
	// never cloned.
	// Same inputs dispatch will use (run.Repo is req.Repo), so the sentence and
	// the drop cannot disagree. Inline policies are unaffected: they still 400.
	if _, repoDrops := buildRepoRecords(req.Repo, spec.WorkspaceRepos); len(repoDrops) > 0 {
		warnings = append(warnings, repoDrops...)
	}

	// The no-builder devcontainer fall-through is otherwise visible only as an
	// INFO setup row no CLI or API caller ever reads, so it rides the 201 — the
	// only channel this door has. See appendDevcontainerNoBuilderWarning.
	warnings = s.appendDevcontainerNoBuilderWarning(warnings, req)

	// The split point: the run row exists and every refusal above has answered.
	// Nothing below can become a 4xx, so the caller gets its run now and the
	// image build + dispatch continue server-side (runs_create_launch.go).
	w.Header().Set("Location", s.cfg.BasePath+"/api/v1/runs/"+runID.String())
	writeJSON(w, http.StatusCreated, createRunResponse{AgentRun: created, Warnings: warnings})
	launch := createRunLaunch{
		req: req, spec: spec, ceiling: ceilingForDispatch(ceiling, adoGrade, bedrockGrade), gw: gw,
		wsRefs: wsRefs, driveMount: driveMount, ephemeralDirs: ephemeralDirs,
		runToken: id.Token, created: created, comps: comps,
	}
	launchCtx := context.WithoutCancel(ctx)
	s.goBackground(func() { s.finishCreateRunLaunch(launchCtx, launch) })
}

// seedAndAdmitWorkspace folds a named workspace onto the resolved spec and then
// re-runs every admission check that seeding can invalidate. It writes its own
// HTTP error and returns ok=false once it has responded.
//
// The ORDER is the whole point, and it is why these four live together rather
// than inline among a dozen unrelated steps. Seeding can set req.Image from a
// workspace's base_image, so the capability answer and the
// image/devcontainer XOR must both run AFTER it — an explicit --image was
// already checked before workspace_id was even resolved, which is exactly the
// gap a member-owned workspace could otherwise walk through. The onboarding gate runs
// LAST because it is the single chokepoint on the RESOLVED spec, which is what
// makes it un-bypassable by a hand-authored stored policy.
//
// gate is #386's launch door (review finding F5): LAUNCH (runs.go's own
// caller) passes true, so gitCredentialRefusal runs here TOO — over
// spec.WorkspaceRepos, the resolved set a workspace_id, a SECOND workspace,
// a non-first repo, a stored policy or a hand-authored inline policy all
// fold into, which requestRepoProviderRefusals' two free-text fields alone
// never see. Review (preflight.go) passes false: see that gate's own doc
// comment.
func (s *Server) seedAndAdmitWorkspace(ctx context.Context, w http.ResponseWriter, r *http.Request, spec *types.RunPolicySpec, req *createRunRequest, gate bool) ([]string, bool) {
	dirs, refusal := s.seedAuthorizedWorkspace(ctx, r, spec, req)
	if refusal.write(s, w, r) {
		return nil, false
	}
	if gate && s.gitCredentialRefusal(w, r, repoLocatorsOf(spec.WorkspaceRepos)...) {
		return nil, false
	}
	return dirs, true
}

// repoSourceWarnings is the pair of 201 sentences provider admission cannot
// state in a refusal, because in both cases it ADMITTED the request.
//
// The first: a host the legacy `scm_hosts` list still names and no provider row
// claims. Said ONCE here — the only run-create response with a warnings channel
// — over all three free-text inputs rather than at each door, so one host earns
// one sentence. The AUDIT half is not here: admitRepoSources records it at every
// one of the ten doors, so the eight with no warnings channel are not silent.
//
// The second: an SSH clone URL carries no path, so a provider row scoped to one
// org admitted it for the WHOLE host. Wider than the policy reads, and
// therefore never silent.
//
// Extracted from handleCreateRun for the function-size gate; the locator list
// is built once and read by both (neither retains it).
func (s *Server) repoSourceWarnings(ctx context.Context, runID uuid.UUID, spec types.RunPolicySpec, req createRunRequest) []string {
	locators := append(repoLocatorsOf(spec.WorkspaceRepos), req.Repo, req.DevcontainerRepo)
	return append(s.legacyHostAdmissionWarnings(ctx, locators...), s.sshHostLevelWarnings(ctx, runID, locators...)...)
}

// noModelAccessWarning is the model-resolution fail-fast, as a sentence on the
// 201: a non-interactive harness run whose agent needs a model but that no
// model provider serves boots and 404s on its FIRST model call.
//
// WARN, never hard-reject (edge cases); the CLI already prints warnings, so the
// operator sees it before the run wastes a sandbox. Computed through the SAME
// helper preflight's checklist uses (runLLMAccess), so the two agree.
func noModelAccessWarning(req createRunRequest, mp runProviderChoice) []string {
	if !runNeedsModelWarning(req) {
		return nil
	}
	if la := runLLMAccess(req, mp); la != nil && !la.Provisioned {
		return []string{la.Note}
	}
	return nil
}

// createRunAuditData assembles the run.create event's payload.
//
// Extracted from handleCreateRun rather than inlined, and the reason is the
// same for all four conditional fields below: NONE of them is stored on the run
// row, so this event is their ONLY provenance record — which makes the payload
// a contract worth reading in one scope instead of a block interleaved with the
// dispatch sequence.
//
// confinement_source ("requested"/"defaulted", from reqCC, the pre-resolution
// request value — never re-derive it from enforced, which cannot tell a
// defaulted class from one the caller happened to name explicitly) is what
// makes `enforced` legible: an unspecified request's default is
// live-probed (the strongest class the runner advertises at or above the
// floor), so the SAME enforced value can mean "the caller asked for this" one
// day and "this is what today's runner offered" the next if a runtime
// disappears — the row is the one place that distinction survives.
//
// belowFloor (#150): the caller's own answer to whether
// credentialConfinementAdvisory fired for this run — a stored AWS SSO
// credential is delivered to the sandbox at DISPATCH, after `enforced` is
// already resolved, so it is never an eligible grant and never on the run row
// either; this event is its only provenance record too.
//
// mp (#527): the run's own model-provider choice, {id, kind} — the run row
// freezes the id alone (AgentRun.ModelProviderID), so this event is the only
// provenance for the KIND at the moment of choice, since a provider's kind
// can change later (a kind change mints a fresh UID, #521) and the row would
// then read a kind the id no longer has. Omitted entirely when mp.chosen is
// false — no provider block, or a block serving no provider for this agent —
// same as every other conditional field above.
func createRunAuditData(req createRunRequest, policyID *uuid.UUID, enforced types.ConfinementClass, reqCC types.ConfinementClass, jti string,
	clampWarnings []string, autonomy types.AutonomyResolution, belowFloor bool, mp runProviderChoice,
) map[string]any {
	confinementSource := "defaulted"
	if reqCC != "" {
		confinementSource = "requested"
	}
	data := map[string]any{
		"agent": req.Agent, "repo": req.Repo, "policy_id": policyID,
		"confinement_class": enforced, "confinement_source": confinementSource, "jti": jti,
		"inline_policy": req.InlinePolicy != nil,
	}
	if req.Preset != "" {
		// The preset stamp is also on the run row, but the chained audit row
		// outlives it: this ties the run to the preset version it came from.
		data["preset"] = req.Preset
		data["preset_version"] = req.PresetVersion
	}
	if req.TaskMode == "exec" {
		// The run row doesn't store task_mode (request-scoped), so the audit
		// event is the provenance record that this run ran a plain command.
		data["task_mode"] = req.TaskMode
	}
	if req.Interactive && req.InteractiveStart != "" {
		// Same reason as task_mode above: interactive_start is request-scoped, so
		// the audit event is the only record that this sandbox opened straight
		// into the agent CLI rather than a bare shell.
		data["interactive_start"] = req.InteractiveStart
	}
	if req.Interactive && req.SeedAutoTools {
		// SeedAutoTools is request-scoped like interactive_start above; only
		// meaningful alongside a non-empty Task (the boot seed) — recorded
		// whenever requested regardless, since dispatch's own `interactive &&`
		// gate is what makes it structurally inert otherwise.
		data["seed_auto_tools"] = req.SeedAutoTools
	}
	if req.ToolApprovals != "" {
		// Request-scoped like task_mode: this is the only record that an
		// autonomous run's tool calls were routed to Wardyn approvals instead of
		// running unsupervised.
		data["tool_approvals"] = req.ToolApprovals
	}
	if len(clampWarnings) > 0 {
		// Every way launch NARROWED what the caller asked for (resolveRunPolicy's
		// clamp + capability notes). Request-scoped like the fields above, and this
		// RUN-BOUND row is the only place a member can read them back: an inline
		// policy's own policy.inline.apply row carries no run id, and run.policy.resolve
		// carries the merged policy, not the list of tightenings that produced it.
		// The run-detail "Effective policy" widget reads exactly this.
		data["clamp_warnings"] = clampWarnings
	}
	if autonomy.Level != "" {
		// The WHOLE resolution — level, the three-axis posture that produced
		// it, and EVERY rubric field that tied at it. The run row freezes the level
		// alone, so this event is the only record of WHY that level: a posture
		// is a function of a spec that is about to be widened (unionRunEgress
		// runs below) and of an enforced class that is live-probed, so it
		// cannot be re-derived from the row afterwards. Omitted entirely for a
		// run under no profile or no rubric, which keeps the payload
		// byte-for-byte what it was for every deployment that authors neither.
		data["autonomy"] = autonomy
	}
	if belowFloor {
		// Closed vocabulary (like confinement_source's requested/defaulted): an
		// incident review filtering "which runs carried an under-confined SSO
		// credential" needs to GROUP, which free text cannot do. Absent covers
		// everything else — no SSO-delivered credential, or one whose enforced
		// confinement already meets CC3.
		data["credential_confinement"] = credentialConfinementBelowFloor
	}
	if mp.chosen {
		snapshot := map[string]any{"id": mp.provider.ID, "kind": mp.provider.Kind}
		if mp.provider.Name != "" {
			// The name the run was launched under (#996): a provider deleted later
			// takes its name with it, and the row freezes the id alone.
			snapshot["name"] = mp.provider.Name
		}
		data["model_provider"] = snapshot
	}
	return data
}

// grantChecker is the optional grant-gating surface implemented by the embedded
// identity provider (CheckGrants). The API uses it to refuse policies whose
// grants require a different provider, without importing the embedded package.
type grantChecker interface {
	CheckGrants(grants []types.GrantSpec) error
}

// createRunResponse is the create-run reply: the run's fields at the TOP LEVEL
// (so existing AgentRun decoders are unaffected) plus optional advisory warnings
// (e.g. a workspace-directory collision with another active run — discouraged,
// never blocked).
type createRunResponse struct {
	types.AgentRun
	Warnings []string `json:"warnings,omitempty"`
}

// runLLMAccess is the deterministic model-access verdict for a run request —
// the SAME computation preflight's checklist uses, shared so the create-path
// warning and the preflight row can never disagree: the chosen provider's
// verdict, or "no provider serves it". nil for an agent Wardyn wires no model
// credential for (nothing to resolve).
func runLLMAccess(req createRunRequest, mp runProviderChoice) *composeLLMAccess {
	if _, needsModel := agentLLMProvider(req.Agent); !needsModel {
		return nil
	}
	return providerLLMAccess(req.Agent, mp)
}

// runNeedsModelWarning gates the create-time model-access check: a NON-interactive
// HARNESS run (not task_mode=exec, not interactive, not the server-set harness-login
// box) whose agent needs a model. An exec run runs a plain command (no harness); an
// interactive run surfaces a model failure to the operator live; the login box mints
// nothing — none warrant the boot-then-404 warning.
func runNeedsModelWarning(req createRunRequest) bool {
	if req.TaskMode == "exec" || req.Interactive || req.Task == harnessLoginTask {
		return false
	}
	_, needsModel := agentLLMProvider(req.Agent)
	return needsModel
}
