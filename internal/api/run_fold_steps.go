// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The steps of runFoldSteps, in its order. Each writes its own refusal and
// returns false once it has answered. A mode test inside a step is either an
// `if f.mode == foldX` (or !=) branch or a flag argument;
// TestPreflightMirrorsLaunchGates refuses any other use, so every door
// difference stays readable to it. A mode test is a top-level statement of the
// step, never under a data condition. The check a step performs must be a
// (*Server) method call: the guard compares those by name, so a bare function's
// check, skipped at one door, is invisible to it.

// stepRunContract validates the New Run request fields (placement, resources,
// workspace targets, overrides, built-in components) and refuses, by name, any
// that changes the run's posture and that no lane applies yet (run_contract.go).
func (s *Server) stepRunContract(f *runFold, rec *foldRecorder) bool {
	return !s.runContractRefusal(*f.req).write(s, f.w, f.r)
}

// stepPolicy resolves the run policy through the one chokepoint: inline_policy,
// policy_id or the default, member-clamped. Its clamp and capability warnings
// reach every door, because a member whose host, grant or repo was narrowed
// away must hear it from the thing they called.
func (s *Server) stepPolicy(f *runFold, rec *foldRecorder) bool {
	ctx := f.r.Context()
	if f.mode == foldPreview {
		// credentials=false: the preview never reads a secret value.
		var refusal *runRefusal
		f.spec, f.policyID, f.policyWarns, f.source, refusal = s.resolveRunPolicyFacts(ctx, f.r, f.req, true, false)
		if refusal.write(s, f.w, f.r) {
			return false
		}
		recordPolicySource(rec, f.source, f.spec)
		return true
	}
	var ok bool
	// A promoted run's start replays the fold its create already ran, which wrote the policy audit rows
	// once: the replay writes none, as a dry door writes none.
	dry := f.promoted
	if f.mode == foldPreflight {
		ctx = context.WithValue(ctx, reviewDoorKey{}, true)
		dry = true
	}
	f.spec, f.policyID, f.policyWarns, f.source, ok = s.resolveRunPolicy(ctx, f.w, f.r, f.req, dry)
	if ok {
		recordPolicySource(rec, f.source, f.spec)
	}
	return ok
}

// stepSeedWorkspace folds the named workspace onto the resolved spec and
// re-runs every admission check seeding can invalidate (seedAndAdmitWorkspace).
// Only create gates on git_credential there (review finding F2); the preview
// reads no credential at all. ephemeralDirs is create's (WARDYN_EPHEMERAL_DIRS).
// Every door decides signinImageUngranted from the seeded image; it refuses
// nothing, and only create's dispatch reads it.
func (s *Server) stepSeedWorkspace(f *runFold, rec *foldRecorder) bool {
	ctx := f.r.Context()
	var seed workspaceSeed
	if f.mode == foldPreview {
		var refusal *runRefusal
		seed, refusal = s.seedAuthorizedWorkspace(ctx, f.r, &f.spec, f.req)
		f.ephemeralDirs, f.seed = seed.ephemeralDirs, seed
		f.signinImageUngranted = refusal == nil && s.signinVariantUngranted(ctx, *f.req, seed)
		return !refusal.write(s, f.w, f.r)
	}
	var ok bool
	seed, ok = s.seedAndAdmitWorkspace(ctx, f.w, f.r, &f.spec, f.req, f.mode == foldCreate)
	f.ephemeralDirs, f.seed = seed.ephemeralDirs, seed
	f.signinImageUngranted = ok && s.signinVariantUngranted(ctx, *f.req, seed)
	return ok
}

// stepDrive resolves the run's drives, after the onboarding gate: a drive is
// per-principal and never appears in the spec, so it does not pass through
// validateWorkspaceSources, but it must not answer before that gate either.
// The refusals are the point at every door; the mounts are create's and
// preflight's (seedRequestDrives). The preview checks the read-only narrowing
// and leaves runner and share readiness pending.
func (s *Server) stepDrive(f *runFold, rec *foldRecorder) bool {
	if f.mode == foldPreview {
		drives, refusal := s.authorizeRequestDrives(f.r, *f.req, f.ceiling)
		if refusal.write(s, f.w, f.r) {
			return false
		}
		for _, d := range drives {
			if driveReadOnlyRefusal(d.narrow, d.resolved).write(s, f.w, f.r) {
				return false
			}
		}
		f.drives = drives
		return true
	}
	var ok bool
	f.drives, f.driveMounts, ok = s.seedRequestDrives(f.w, f.r, *f.req, f.ceiling)
	return ok
}

// stepWorkspaces reads the run's workspaces: the attached ones, then each other
// one the seeded spec references. Nothing after this step changes the spec's
// mounts or repos, so the list stays valid for the requirements fold, the
// egress unions and image resolution. Over every resolved target, the drive's
// included, it refuses an overlap, a second Azure DevOps organisation or (when
// the workspaces decide the image) a second base image, and resolves the one
// starting folder, before anything is granted. It runs after the onboarding
// gate, so every workspace a refusal names is one the caller may launch.
func (s *Server) stepWorkspaces(f *runFold, rec *foldRecorder) bool {
	// One read serves the referenced workspaces and the index the onboarding
	// gate held each policy source to; a failed read references none, as
	// referencedWorkspaces does.
	var all []types.Workspace
	if s.cfg.Store != nil {
		all, _ = s.cfg.Store.ListWorkspaces(f.r.Context())
	}
	carriers := s.launchCarrierRule(f.r, f.seed.attached).index(all)
	f.wsRefs = attachedWorkspaceRefs(f.seed.attached, referencedWorkspacesBy(carriers, f.spec))
	for _, ws := range f.wsRefs {
		recordWorkspaceSources(rec, f.spec, ws)
		_, dropped := runCloneEgress(ws, f.ceiling.Spec.AllowedDomains)
		for _, h := range dropped {
			f.policyWarns = append(f.policyWarns, cloneHostDropWarning(h, "workspace", ws.Name))
		}
	}
	var refusal *runRefusal
	f.attachments, f.startFolder, refusal = s.runAttachments(*f.req, f.spec, f.wsRefs, carriers, f.seed,
		driveTargets(f.drives))
	return !refusal.write(s, f.w, f.r)
}

// stepHarnessSet holds every harness a run-mode request includes to the agent
// capability, the org roster and the installed gate, and resolves the image
// that carries the set (run_harness_set.go). Older clients pass untouched.
func (s *Server) stepHarnessSet(f *runFold, rec *foldRecorder) bool {
	set, image, refusal := s.harnessSetRefusalFor(f.r, *f.req, f.wsRefs)
	if refusal.write(s, f.w, f.r) {
		return false
	}
	f.harnesses, f.image = set, image
	return true
}

// stepPushRuleSets builds the run's push rules per source-control entry from the
// resolved spec, at every door, so a remote whose entry cannot be keyed refuses
// at Review and the preview as it does at create.
func (s *Server) stepPushRuleSets(f *runFold, rec *foldRecorder) bool {
	// The site config is read here: f.scmSite is the autonomy step's snapshot and
	// is not there yet, and an Azure DevOps Server row's base path comes from it.
	site, err := s.scmLaneSiteConfig(f.r.Context(), f.spec, f.req.Repo)
	if err != nil {
		writeServerError(f.w, f.r, "get site config", err)
		return false
	}
	sets, applied, err := s.pushRuleSetsFor(f.spec, f.req, site)
	if err != nil {
		runError(http.StatusUnprocessableEntity, reasonInlinePolicyInvalid, "invalid push rules: "+err.Error()).write(s, f.w, f.r)
		return false
	}
	f.pushSets = sets
	f.source.recordPushRules(applied)
	return true
}

// stepPreviewEgress widens the dry doors' spec from onboarded-workspace
// registries and clone hosts the way create's post-mint unionRunEgress will,
// side-effect free, so Review grades the envelope the run is launched with.
// The SSH, site-config and ADO lanes need grant wiring that does not exist
// before the mint and stay create's.
func (s *Server) stepPreviewEgress(f *runFold, rec *foldRecorder) bool {
	for _, ws := range f.wsRefs {
		var contributed types.RunPolicySpec
		unionPreviewWorkspaceEgress(&contributed, []types.Workspace{ws}, f.ceiling.Spec.AllowedDomains)
		recordSpecAdds(rec, types.RunPolicySpec{}, contributed, workspaceProvSource(ws))
	}
	unionPreviewWorkspaceEgress(&f.spec, f.wsRefs, f.ceiling.Spec.AllowedDomains)
	return true
}

// stepDerivedEgress gives the dry doors the hosts create's unionRunEgress adds
// after the mint, from the same deriveRunEgress, so the preview's spec and
// Review's access facts name what dispatch will allow. Last of the fold: no
// gate or grade above it reads these hosts at create, so none reads them here.
//
// Only the lanes create would wire add hosts (lanesWiredFor). The site-config
// SCM hosts stay create's: they are the operator's configured host list, which
// no dry-door reader may see, so that lane is still pending.
func (s *Server) stepDerivedEgress(f *runFold, rec *foldRecorder) bool {
	policy := policyProvSource(f.source)
	wired := lanesWiredFor(f.scmSite, f.req.Agent, repoLocatorsOf(f.spec.WorkspaceRepos), f.spec.EligibleGrants)
	for _, d := range deriveRunEgress(&f.spec, f.wsRefs, f.req.Repo, types.SiteConfig{}, wired, f.ceiling.Spec.AllowedDomains) {
		for _, host := range d.added {
			rec.add(fieldAllowed, host, derivedEgressSource(rec, f.spec.EligibleGrants, d.kind, host, policy), provEffectAdded)
		}
	}
	return true
}

// stepRequirements folds each referenced workspace's requirements contract
// into the spec before the confinement floor and the grade read it: a
// workspace's integration:<id> requirement is a credential grant the CC3
// blast-radius floor must see, or invariant 5 is silently bypassed. This is
// the audit-free half; create records reqEvents once its run id exists. The
// fold's own refusal (a write: row past the member writable roots) answers at
// every door.
func (s *Server) stepRequirements(f *runFold, rec *foldRecorder) bool {
	ctx := f.r.Context()
	// Caller-scoped secret names, resolved once: the requirement fold and
	// Review's checklist read the same map.
	f.present = s.presentSecretNamesFor(ctx, s.secretOwnerFromRequest(f.r))
	selections := resolveWorkspaceSelections(*f.req)
	// One workspace at a time, so each one's additions are its own.
	for _, ws := range f.wsRefs {
		before := f.spec.Clone()
		bound := &egressBound{trusted: f.ceiling.Spec.AllowedDomains}
		events, refusal := s.applyWorkspaceRequirementsFor(ctx, f.present, &f.spec, f.req.Agent, []types.Workspace{ws}, f.seed.attached, selections, bound)
		if refusal != nil {
			return !runError(refusal.status, refusal.reason, refusal.Error()).write(s, f.w, f.r)
		}
		f.policyWarns = append(f.policyWarns, bound.dropped...)
		f.reqEvents = append(f.reqEvents, events...)
		recordRequirementFold(rec, before, f.spec, ws, selections[ws.ID.String()])
	}
	// The mount map was read before this fold, which can narrow a mount to read-only: show the final one.
	for i, a := range f.attachments {
		if j := slices.IndexFunc(f.spec.WorkspaceMounts, func(m types.WorkspaceMount) bool { return a.Kind == attachmentFolder && m.Target == a.Target }); j >= 0 {
			f.attachments[i].ReadOnly = f.spec.WorkspaceMounts[j].ReadOnlyOrDefault()
		}
	}
	return true
}

// stepGitHubEgress runs after the requirement fold, which decides whether a
// github_token grant survives: a dropped grant leaves a direct clone, which
// needs GitHub egress the floor below must see. create audits the hosts it adds
// after the mint (unionRunEgress).
func (s *Server) stepGitHubEgress(f *runFold, rec *foldRecorder) bool {
	var refusal *runRefusal
	f.directGitHubAdded, refusal = s.unionDirectGitHubEgress(f.r, *f.req, &f.spec, f.ceiling)
	if refusal.write(s, f.w, f.r) {
		return false
	}
	src := directGitHubSource(*f.req, f.spec, f.wsRefs)
	for _, h := range f.directGitHubAdded {
		rec.add(fieldAllowed, h, src, provEffectAdded)
	}
	return true
}

// stepComponents bounds the run's components and expands them onto the spec:
// after the folds above, so a host an admin already credentialed is seen;
// before the floor, the model-provider choice and the autonomy grade, which
// read what a component adds. The preview reports a secret that is not stored
// yet rather than refusing it.
func (s *Server) stepComponents(f *runFold, rec *foldRecorder) bool {
	var refusal *runRefusal
	f.comps, refusal = s.applyRunComponents(f.r, *f.req, &f.spec, f.ceiling, f.wsRefs, f.mode != foldPreview)
	if refusal.write(s, f.w, f.r) {
		return false
	}
	recordComponents(rec, f.comps)
	return true
}

// stepBaseline reads the operator's egress baseline once, for every step and
// for the door's own grading and facts. A site config nobody could read is a
// 500, never an empty baseline: a grade on a guess lowers nothing silently.
func (s *Server) stepBaseline(f *runFold, rec *foldRecorder) bool {
	var ok bool
	f.baseline, ok = s.baselineOr500(f.w, f.r)
	return ok
}

// stepConfinement resolves the enforced class on the folded spec (invariant 5,
// fail closed). Create also gates on runner capability membership and the
// cloud_sts identity provider (resolveEnforcedConfinement); the dry doors run
// the same floor math and leave those tail gates to Review's backend row.
// Review reads the advertised classes best-effort for the default class; the
// preview probes no runner.
func (s *Server) stepConfinement(f *runFold, rec *foldRecorder) bool {
	floor := confinementFloorSpec(f.spec, f.comps)
	if f.mode == foldCreate {
		var ok bool
		f.enforced, f.confinementDefault, ok = s.resolveEnforcedConfinement(f.r.Context(), f.w, requestPlacement(*f.req), requestRunnerID(*f.req), floor, f.reqCC, f.baseline, f.place)
		return ok && !f.place.barrierRefusal(f.enforced).write(s, f.w, f.r)
	}
	var advertised []types.ConfinementClass
	if f.mode == foldPreflight {
		reqCC, ok := parseConfinementClass(f.req.ConfinementClass)
		if !ok {
			writeErrorReason(f.w, http.StatusBadRequest, reasonConfinementClassUnknown, fmt.Sprintf("unknown confinement_class %q", f.req.ConfinementClass))
			return false
		}
		f.reqCC = reqCC
		advertised = s.advertisedConfinement(f.r.Context(), *f.req)
	}
	// A pooled run's default class comes from its candidates at every door, the preview included, so the
	// three doors promise one barrier.
	if offered := f.place.offeredClasses(); offered != nil {
		advertised = offered
	}
	enforced, source, err := enforcedConfinement(floor, f.reqCC, advertised, f.place.orgBarrier, f.baseline)
	if err != nil {
		writeErrorReason(f.w, http.StatusUnprocessableEntity, reasonConfinementClassConflict, err.Error())
		return false
	}
	f.enforced, f.confinementDefault = enforced, source
	if f.mode == foldPreflight {
		f.barrierOptions = draftBarrierOptions(f.place, f.ceiling, floor, f.baseline)
	}
	return !f.place.barrierRefusal(enforced).write(s, f.w, f.r)
}

// stepPlacementCapability refuses a request whose own shape demands an optional
// runner interface the placement it gets does not implement (design 3.4), so
// the demand is never silently dropped. A runner that cannot answer is refused
// runner_offline. The remote placement is the deployment's own executor and is
// not asked.
func (s *Server) stepPlacementCapability(f *runFold, rec *foldRecorder) bool {
	return !s.placementCapabilityRefusal(f.r.Context(), *f.req, f.place, f.ceiling).write(s, f.w, f.r)
}

// stepModelProvider makes the model-provider choice and refuses one whose
// credential the caller does not hold, here rather than at dispatch. It runs
// before autonomy because that gate grades this credential (#504). Only create
// renews an expired AWS sign-in; the preview authorizes the selection without
// reading any credential and leaves the choice pending when nothing pins it.
//
// Every other harness a run-mode request includes is judged the same way, as
// its own agent with its own requested provider and no workspace pin (only the
// primary inherits the pin), then the set is held to the one-lane invariant
// (harnessProvidersConflict). modelCred is the most resident choice, so
// autonomy grades the deepest credential the sandbox gets.
func (s *Server) stepModelProvider(f *runFold, rec *foldRecorder) bool {
	var ok bool
	if f.mode == foldPreview {
		choice, refusal := s.authorizeRunModelProvider(f.r, *f.req, f.wsRefs, true)
		if refusal.write(s, f.w, f.r) {
			return false
		}
		f.mpChoice = choice
		recordProviderHost(rec, choice, f.req.Agent)
		_, needsModel := agentLLMProvider(f.req.Agent)
		if needsModel && createDoorIsModelRun(*f.req) {
			if name, secretName, found := modelEnvSecretGrant(f.spec); found {
				s.writeProviderRefusal(f.w, f.r, choice.provider.ID, choice.provider.Kind, fmt.Sprintf(mpRunModelEnvSecret, secretName, name), false)
				return false
			}
		}
		f.harnessChoices, ok = s.judgeHarnessProviders(f.w, f.r, *f.req, f.spec, f.harnesses, rec, choice, true, false, false)
		grantHarnessProviders(f.harnesses, f.harnessChoices)
		return ok
	}
	if f.mode == foldPreflight {
		w := f.w
		// Every setup row but model access, which the refusal itself answers.
		f.w = credentialSetupWriter{w, func() []SetupItem {
			return s.deriveRunSetupItems(f.r.Context(), s.secretOwnerFromRequest(f.r), preflightRunInput(f.req, f.enforced, f.baseline), f.spec, f.present, nil, f.wsRefs)
		}}
		defer func() { f.w = w }()
	}
	var choice runProviderChoice
	if choice, ok = s.enforceRunModelProvider(f.w, f.r, *f.req, f.spec, f.wsRefs, f.mode == foldCreate, f.queued); !ok {
		return false
	}
	f.mpChoice = choice
	f.modelCred = choice.modelCredential()
	recordProviderHost(rec, choice, f.req.Agent)
	f.harnessChoices, ok = s.judgeHarnessProviders(f.w, f.r, *f.req, f.spec, f.harnesses, rec, choice, false, f.mode == foldCreate, f.queued)
	grantHarnessProviders(f.harnesses, f.harnessChoices)
	if len(f.harnessChoices) > 1 {
		f.modelCred = mostResident(f.harnessChoices).modelCredential()
	}
	return ok
}

// stepAWSCLIProfile refuses an AWS CLI component on a run whose model, or any
// included harness's, comes through an AWS sign-in: a sandbox holds one AWS
// profile. Dispatch refuses it too (authorAWSCLI); this is the doors' answer.
func (s *Server) stepAWSCLIProfile(f *runFold, _ *foldRecorder) bool {
	if f.comps.awsCLI() == nil || !runUsesAWSSignIn(f.mpChoice, f.harnessChoices) {
		return true
	}
	writeErrorReason(f.w, http.StatusUnprocessableEntity, reasonComponentCLIAWSWithBedrockSignIn, cliAWSWithBedrockSSORefusal)
	return false
}

// judgeHarnessProviders is stepModelProvider's half for the harnesses of set
// beside the primary (req.Agent), whose choice is primary: preview authorizes
// the selection alone, the other doors also check the caller's own credential
// (renew at create only, and never for a run that waits: queued), each as its own agent with its own requested
// provider and no workspace pin; an AWS sign-in serves the primary alone
// (harnessProviderNeedsPrimary). Then the set is held to the one-lane
// invariant (harnessProvidersConflict). It writes its own refusal.
func (s *Server) judgeHarnessProviders(w http.ResponseWriter, r *http.Request, req createRunRequest, spec types.RunPolicySpec,
	set []types.RunHarness, rec *foldRecorder, primary runProviderChoice, preview, renew, queued bool,
) ([]harnessChoice, bool) {
	choices := make([]harnessChoice, 0, len(set))
	for _, h := range set {
		choice := primary
		if h.Harness != req.Agent {
			hreq := req
			hreq.Agent, hreq.ModelProvider = h.Harness, h.ModelProviderID
			var refusal *runRefusal
			if choice, refusal = s.authorizeRunModelProvider(r, hreq, nil, preview); refusal.write(s, w, r) {
				return nil, false
			}
			// Before the credential read, so the reason is this and never a sign-in.
			if choice.chosen && harnessProviderNeedsPrimary(choice.provider) {
				runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable,
					harnessSignInNotPrimaryMsg(harnessName(h.Harness), choice.provider.ID)).forTool(h.Harness).write(s, w, r)
				return nil, false
			}
			if !preview {
				var ok bool
				if choice, ok = s.enforceChosenModelProvider(w, r, hreq, spec, choice, renew, queued); !ok {
					return nil, false
				}
			}
			recordProviderHost(rec, choice, h.Harness)
		}
		choices = append(choices, harnessChoice{harness: h.Harness, choice: choice})
	}
	if a, b, conflict := harnessProvidersConflict(choices); conflict {
		runError(http.StatusUnprocessableEntity, reasonHarnessProvidersConflict, harnessProvidersConflictMsg(harnessName(a), harnessName(b))).forTool(b).write(s, w, r)
		return nil, false
	}
	return choices, true
}

// stepAutonomy is the posture-gated autonomy, resolved once and enforced at
// create and Review alike: after the enforced class (the posture's third axis)
// and the model credential (graded on the secrets axis), before the mint. It
// may derive req.ToolApprovals to hold, which create's audit and dispatch read.
// scmSite is the one site-config snapshot the SCM-host lane was graded from;
// adoGrade and bedrockGrade freeze what dispatch may author. The preview grades
// no autonomy and reads the snapshot alone.
func (s *Server) stepAutonomy(f *runFold, rec *foldRecorder) bool {
	if f.mode == foldPreview {
		site, err := s.scmLaneSiteConfig(f.r.Context(), f.spec, f.req.Repo)
		if err != nil {
			writeServerError(f.w, f.r, "get site config", err)
			return false
		}
		f.scmSite = site
		return true
	}
	var ok bool
	f.autonomy, f.autonomyWarns, f.scmSite, f.adoGrade, f.bedrockGrade, ok = s.resolveRunAutonomy(f.w, f.r, f.req, f.spec, f.wsRefs, f.enforced, f.ceiling, f.modelCred, f.comps, f.baseline)
	return ok
}

// stepToolPolicy settles the tool lane and holds a person's tool rules to the
// policy, after autonomy so every step that can put the run on the gated lane
// has: a person's rule or default, or a policy rule that holds or blocks, puts
// a background run of a gated harness there with "*: allow" under the person's
// layer (applyToolPosture), and each
// allow they send stands only where the policy, with the lane's default under
// it, already allows the tool. An interactive Claude Code run's default, and
// every tool name it carries, is held to what its managed settings can say
// first, as step 5 greys it. Last, the policy's harness_inner_walls: approvals
// in Wardyn put a background Claude Code run on the gated lane, and walls the
// run cannot meet refuse it (innerWallsFor).
func (s *Server) stepToolPolicy(f *runFold, rec *foldRecorder) bool {
	before := f.spec
	if warn := applyInnerWallsHold(f.req, f.spec); warn != "" {
		f.policyWarns = append(f.policyWarns, warn)
	}
	s.applyToolPosture(f.req, &f.spec)
	recordSpecAdds(rec, before, f.spec, provSource{Kind: provKindPosture})
	if attendedDefaultRefusal(*f.req, f.autonomy.Level).write(s, f.w, f.r) || attendedToolNamesRefusal(*f.req, f.spec).write(s, f.w, f.r) ||
		inheritedMCPHoldRefusal(*f.req, f.spec, f.comps).write(s, f.w, f.r) {
		return false
	}
	if s.toolRulesWithinPolicyRefusal(*f.req, f.spec).write(s, f.w, f.r) {
		return false
	}
	var refusal *runRefusal
	f.innerWalls, refusal = innerWallsFor(*f.req, f.spec, f.harnesses)
	return !refusal.write(s, f.w, f.r)
}

// stepGitHubCLIRepo is githubCLIRepoRefusal (runs_cli_github.go) at the doors that launch and
// review. The preview shows the same fact as the card's reason instead.
func (s *Server) stepGitHubCLIRepo(f *runFold, _ *foldRecorder) bool {
	return !githubCLIRepoRefusal(f.comps, *f.req, f.spec, f.scmSite).write(s, f.w, f.r)
}

// stepRunEnd decides the run's end and its lifetime cap against the pool,
// governance and the deployment (run_end_create.go); a refusal is the same at
// every door and leaves no row.
func (s *Server) stepRunEnd(f *runFold, rec *foldRecorder) bool {
	var pool *types.RunnerPool
	if f.place.pooled {
		pool = &f.place.row
	}
	var refusal *runRefusal
	f.end, refusal = s.resolveCreateEnd(f.r, *f.req, f.ceiling, pool)
	return !refusal.write(s, f.w, f.r)
}

// stepADOStanding resolves the run's Azure DevOps lane once, from the inputs
// create grades and dispatch resolves with, and bounds a member's Azure DevOps
// list (#1384); the narrowing and a decline (f.adoDecline) are said out loud at
// every door. A list that leaves nothing standing is dispatch's refusal at
// create, which the dry doors answer before the click.
func (s *Server) stepADOStanding(f *runFold, rec *foldRecorder) bool {
	var lane adoEntraRun
	lane, f.adoDecline = resolveADOEntraLane(f.scmSite, f.spec.WorkspaceRepos, f.req.Repo, adoLaneOwnerOf(f.r))
	narrowed, dropped, none := adoStandingAtDoor(f.spec, lane, f.ceiling)
	if f.mode != foldCreate {
		if none {
			writeErrorReason(f.w, http.StatusUnprocessableEntity, reasonADOCapabilitiesNonePermitted, adoNonePermitted(f.spec.AzureDevOpsCapabilities))
			return false
		}
	}
	f.adoNarrowed = narrowed
	recordADONarrowing(rec, dropped)
	return true
}

// stepPATNarrowing answers dispatch's git_pat narrowing refusals at the dry
// doors, the same reasons and sentences, so a narrowing the run could not
// enforce shows before the click. Create meets them at dispatch.
func (s *Server) stepPATNarrowing(f *runFold, rec *foldRecorder) bool {
	if reason, detail := s.patNarrowingAtDoor(f.r, *f.req, f.spec, f.scmSite, f.ceiling.Operator, f.place.pooled); reason != "" {
		writeErrorReason(f.w, http.StatusUnprocessableEntity, reason, detail)
		return false
	}
	return true
}

// stepHostCapacity is the same 503, reason and Retry-After at create and
// Review; only create writes the audit row.
func (s *Server) stepHostCapacity(f *runFold, rec *foldRecorder) bool {
	if f.queued || f.promoted { // the run waits for them, or promotion has applied them
		return true
	}
	return !writeHostCapacityRefusal(f.w, f.r, s.admitHostCapacity(f.r.Context(), principalFromRequest(f.r), "runs", f.mode == foldCreate))
}

// stepRunCap refuses a full deployment before the mint: no identity.mint row
// and no live token for a run that gets no row. CreateRunUnderCap still
// decides a race at the cap.
func (s *Server) stepRunCap(f *runFold, rec *foldRecorder) bool {
	if f.queued || f.promoted { // the run waits for them, or promotion has applied them
		return true
	}
	return !s.refuseRunCapFull(f.w, f.r)
}

// stepRunFit refuses a run the runs namespace's ResourceQuota cannot hold; its
// advisories join the door's warnings. The quota's own admission stays the
// authority on a race. A run that waits for a slot is refused only when no quota
// could ever hold it: it may fit once other runs finish, and promotion asks again.
func (s *Server) stepRunFit(f *runFold, rec *foldRecorder) bool {
	if f.promoted {
		return true
	}
	var refused bool
	f.fitWarnings, refused = s.refuseRunFit(f.w, f.r, s.runFitSpec(f.spec, f.mainDrive), time.Duration(f.end.capSec)*time.Second,
		runProxyAzure(f.mpChoice, f.harnessChoices), f.queued)
	return !refused
}

// stepPlacementDenied is the deny_placements door for an unpooled request: one
// whose placement the caller's governance profile denies is refused 403
// placement_denied, naming the profile, before any local-eligibility work. Where
// pools place runs, the request's placement is not the run's: resolvePlacement
// holds the list against each kind of runner in the pool and asks the same door
// when it leaves none (kindTargets). An unassigned member
// has no profile and an operator's runs stand outside governance, so for both
// the list is absent and every placement is permitted.
func (s *Server) stepPlacementDenied(f *runFold, rec *foldRecorder) bool {
	if s.poolsPlaceRuns() {
		return true
	}
	return !s.placementDeniedRefusal(f.r.Context(), requestPlacement(*f.req), f.ceiling).write(s, f.w, f.r)
}

// stepResources writes the size placement applied into the resolved spec, so everything after it
// (host capacity, the quota fit, the run row, dispatch) reads what the dry doors report. A cut is
// said once; it never refuses.
func (s *Server) stepResources(f *runFold, rec *foldRecorder) bool {
	a := f.place.applied
	rl := types.ResourceLimits{}
	if f.spec.Resources != nil {
		rl = *f.spec.Resources
	}
	rl.CPUMillis, rl.MemoryMiB = a.CPUMillis.Applied, a.MemoryMiB.Applied
	f.spec.Resources = &rl
	if idle := f.place.limits.Idle; idle != nil {
		f.spec.AutoStopAfterSec = poolIdleStop(f.spec.AutoStopAfterSec, *idle)
	}
	// The clamp already folded a PIDs cut into the shared warning; this is the signal that says which one.
	f.place.applied.PidsLimit = appliedPids(f.source, f.ceiling, rl.PidsLimit)
	if (a.CPUMillis.Capped || a.MemoryMiB.Capped) && !slices.Contains(f.policyWarns, composer.WarnResourcesCapped) {
		f.policyWarns = append(f.policyWarns, composer.WarnResourcesCapped)
	}
	return true
}

// poolIdleStop is a run's auto_stop_after_sec under its pool's idle stop (an Interactive run's only;
// Idle is nil for a Background run): the pool's default when the policy sets none (0, or the -1 an
// interactive session uses for never), never above the pool's cap. The idle reaper stops the run on that
// value as on any other.
func poolIdleStop(sec int, idle types.EffectiveLimit) int {
	if sec <= 0 && !idle.Default.Unlimited {
		sec = idle.Default.Value
	}
	if !idle.Cap.Unlimited && (sec <= 0 || sec > idle.Cap.Value) {
		sec = idle.Cap.Value
	}
	return sec
}

// addRemoteInstead appends how to run a refused run on a remote-hosted runner, to a sent 4xx and to a
// denial decision (status 0), which is written from the decision's sentence (the reason's own when it has
// none). A decision that hides its cause or answers with another reason keeps its body, and a server error says nothing.
func (f *runRefusal) addRemoteInstead(pool string) {
	if f == nil {
		return
	}
	if f.status == 0 && f.decision != nil && f.err == nil && f.drive == nil {
		d := f.decision
		if d.Effect == authz.EffectHidden || d.WireReason != "" {
			return
		}
		sentence := d.Sentence
		if ref, ok := authz.Lookup(d.Reason); ok && sentence == "" {
			sentence = ref.Sentence
		}
		if sentence != "" {
			d.Sentence = sentence + " " + remoteInsteadMsg(pool)
		}
		return
	}
	if f.status >= http.StatusBadRequest && f.status < http.StatusInternalServerError {
		f.body.Error += " " + remoteInsteadMsg(pool)
	}
}

// stepLocalEligibility is before confinement/provider liveness: create may
// renew a one-use AWS credential there. No local refusal may reach that phase.
// The admitted plan supplies specific refusals; an unpooled local request has
// no runner to go to and is refused before mint/persistence. A pooled run is
// judged at the placement stepPlacement decided, and is never moved to the
// other kind when it is refused: where the pool chose the person's own runner
// and a remote-hosted one could also have taken it, the sentence says how.
func (s *Server) stepLocalEligibility(f *runFold, rec *foldRecorder) bool {
	req := *f.req
	if f.place.pooled {
		req.Placement = f.place.placement
	}
	var refusal *runRefusal
	f.localDelivery, refusal = s.localPlacementRefusal(f.r, req, f.place.pooled, candidateTrusts(f.place.eligible, promotedRunner(f.r.Context())), trustOwners(f.place.eligible), f.spec, f.ceiling, f.comps, f.driveMounts, f.wsRefs, f.baseline)
	if f.place.remoteOffered {
		refusal.addRemoteInstead(f.place.pool.Name)
	}
	if refusal.write(s, f.w, f.r) {
		return false
	}
	// The run is bound only to a runner of the trust class its plan was admitted at. A run that waits is
	// judged again at its start, the promotion's runner's class first (candidateTrusts).
	if req.Placement == types.PlacementLocal && f.place.pooled {
		f.place.narrowToTrust(f.localDelivery.trust)
		if f.localDelivery.personOwned {
			f.place.narrowToPersonOwned()
		}
	}
	// A launch that waits on the pool's remote-hosted runners records this step's answer for the person's
	// own: the queue then never offers it a runner of theirs that frees first, which the start would
	// refuse here, ending a run that was accepted to wait.
	if f.queued && f.place.pooled && req.Placement == types.PlacementRemote {
		req.Placement = types.PlacementLocal
		_, local := s.localPlacementRefusal(f.r, req, true, waitingLocalTrusts(), nil, f.spec, f.ceiling, f.comps, f.driveMounts, f.wsRefs, f.baseline)
		// A read that failed answers nothing (a server error carries its error, not a status): the launch
		// then behaves as one with nothing recorded.
		failed := local != nil && (local.err != nil || local.status >= http.StatusInternalServerError)
		f.place.noLocal = local != nil && !failed
		if failed {
			slog.WarnContext(f.r.Context(), "wardynd: read a waiting run's local eligibility; the queue may offer it a runner of the person's own", slog.Any("err", local.err))
		}
	}
	return true
}

// stepDriveEligibility holds the run's drives to the kind of runner it got: no
// drive mounts on a person's own runner, so a run stepPlacement left on one (its
// pool has no remote-hosted runner for it, or the request asked for its own
// runner) is refused, named, before anything is sized or minted. A person's own
// drive is held off such a runner permanently (memberDriveNeverOnPool).
func (s *Server) stepDriveEligibility(f *runFold, rec *foldRecorder) bool {
	if len(f.drives) == 0 || !f.place.pooled || f.place.placement != types.PlacementLocal {
		return true
	}
	msg := driveNotInPoolMsg(f.drives[0].resolved.Drive.Name)
	return !driveDenied(authz.Deny(authz.ReasonDriveNotInPool, "runs.drive", msg), msg).write(s, f.w, f.r)
}

// stepPlacement runs after the components and before the local-eligibility step: the confinement
// floor it reads is the policy's and the components', and the plan it gates is the one that step judges.
func (s *Server) stepPlacement(f *runFold, rec *foldRecorder) bool {
	in := placementInput{req: f.req, floor: confinementFloorSpec(f.spec, f.comps), baseline: f.baseline, reqCC: f.reqCC, reqCCOK: true,
		ceiling: f.ceiling, untouched: resourceBaseline(f.source, f.spec), ask: resourceAsk(f.req.Resources, sourceSize(f.source, f.ceiling)),
		promoted: f.promoted, drives: len(f.drives) > 0, noLocal: promotedNoLocal(f.r.Context())}
	if f.spec.Resources != nil {
		in.policyDiskMiB = f.spec.Resources.DiskMiB
	}
	if f.mode != foldCreate {
		in.dryDoor = true
	}
	if f.mode == foldPreflight { // preflight parses the class inside stepConfinement, after this step
		in.reqCC, in.reqCCOK = parseConfinementClass(f.req.ConfinementClass)
	}
	plan, refusal := s.resolvePlacement(f.r.Context(), in)
	if refusal.write(s, f.w, f.r) {
		return false
	}
	f.place = plan
	// A run bound for a capped pool waits for a slot instead of failing (the dry doors answer the same).
	// The queue's own start replays the fold as promoted, and must not queue again.
	if f.mode != foldPreview {
		f.queued = plan.queues() && !f.promoted
	}
	// A launch that waits records the size it gets before any runner bounds it, so the queue sizes it for
	// each runner that could take it (startSizeOn) instead of holding it to the runner it met first.
	if f.place.unbound, refusal = s.unboundSize(f.r.Context(), in, plan.row, f.queued); refusal.write(s, f.w, f.r) {
		return false
	}
	return true
}

// stepMainDrive sizes the main drive after placement, which resolved the effective limits, and
// refuses a size the run's policy writes on a runner that cannot cap disk. The dry doors write the
// number into their answered spec, so spec.resources.disk_mib keeps showing what the run gets;
// create writes nothing into the launch spec and hands dispatch the decision instead.
func (s *Server) stepMainDrive(f *runFold, rec *foldRecorder) bool {
	policyMiB, requestMiB := 0, 0
	if f.spec.Resources != nil {
		policyMiB = f.spec.Resources.DiskMiB
	}
	if f.req.Resources != nil {
		requestMiB = f.req.Resources.DiskMiB
	}
	md := mainDriveFor(policyMiB, requestMiB, f.place.limits.DiskMiB)
	if !md.Soft && md.MiB > 0 && s.runnerCannotCapDisk(f.r.Context(), *f.req, f.place) {
		msg := mainDriveUnenforceableMsg()
		if !f.place.pooled {
			msg = mainDriveUnenforceableUnpooledMsg()
		}
		runError(http.StatusUnprocessableEntity, reasonMainDriveUnenforceable, msg).write(s, f.w, f.r)
		return false
	}
	f.mainDrive = md
	warn := composer.WarnResourcesCapped
	if md.ClampedBy == client.MainDriveClampPolicy {
		warn = warnDiskPolicyCut
	}
	if md.ClampedBy != "" && !slices.Contains(f.policyWarns, warn) {
		f.policyWarns = append(f.policyWarns, warn)
	}
	if f.mode != foldCreate {
		(&mainDriveDecision{MiB: md.MiB}).apply(&f.spec)
	}
	return true
}

// stepADOOverride applies an Azure DevOps narrowing that waited for the workspace seeding, so a
// repository a selected workspace adds resolves its provider row's default profile as dispatch will,
// before any step grades the policy. The diff on the run's policy source and the provenance rows are
// what applying it with the other edits would have recorded.
func (s *Server) stepADOOverride(f *runFold, rec *foldRecorder) bool {
	e := f.source.edits
	if e == nil || e.adoPending == nil {
		return true
	}
	o := *e.adoPending
	e.adoPending = nil
	base, refusal := s.adoOverrideBase(f.r.Context(), f.r, f.spec, f.req)
	if refusal == nil {
		refusal = applyADOOverride(&f.spec, o, base, e)
	}
	if refusal.write(s, f.w, f.r) {
		return false
	}
	if e.applied.AzureDevOps == nil {
		return true // the person kept the whole set: no edit
	}
	e.folded.AzureDevOpsCapabilities = slices.Clone(f.spec.AzureDevOpsCapabilities)
	if f.source.OverridesDiff == nil {
		f.source.OverridesDiff = &client.RunOverridesDiff{}
	}
	f.source.OverridesDiff.AzureDevOps = e.applied.AzureDevOps
	all := &foldRecorder{}
	recordPolicySource(all, f.source, f.spec)
	for _, row := range all.rows {
		if row.Field == fieldADOCaps {
			rec.add(row.Field, row.Value, row.Source, row.Effect)
		}
	}
	return true
}
