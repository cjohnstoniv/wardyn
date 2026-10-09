// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The steps of runFoldSteps, in its order. Each writes its own refusal and
// returns false once it has answered. A mode test inside a step is either an
// `if f.mode == foldX` (or !=) branch or a flag argument;
// TestPreflightMirrorsLaunchGates refuses any other use, so every door
// difference stays readable to it. A mode test is a top-level statement of the
// step, never under a data condition. The check a step performs must be a
// (*Server) method call: the guard compares those by name, so a bare function's
// check, skipped at one door, is invisible to it.

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
		return !refusal.write(s, f.w, f.r)
	}
	var ok bool
	f.spec, f.policyID, f.policyWarns, f.source, ok = s.resolveRunPolicy(ctx, f.w, f.r, f.req, f.mode == foldPreflight)
	return ok
}

// stepSeedWorkspace folds the named workspace onto the resolved spec and
// re-runs every admission check seeding can invalidate (seedAndAdmitWorkspace).
// Only create gates on git_credential there (review finding F2); the preview
// reads no credential at all. ephemeralDirs is create's (WARDYN_EPHEMERAL_DIRS).
func (s *Server) stepSeedWorkspace(f *runFold, rec *foldRecorder) bool {
	ctx := f.r.Context()
	if f.mode == foldPreview {
		var refusal *runRefusal
		f.ephemeralDirs, refusal = s.seedAuthorizedWorkspace(ctx, f.r, &f.spec, f.req)
		return !refusal.write(s, f.w, f.r)
	}
	var ok bool
	f.ephemeralDirs, ok = s.seedAndAdmitWorkspace(ctx, f.w, f.r, &f.spec, f.req, f.mode == foldCreate)
	return ok
}

// stepDrive resolves the run's user drive, after the onboarding gate: the
// drive is per-principal and never appears in the spec, so it does not pass
// through validateWorkspaceSources, but it must not answer before that gate
// either. The refusals are the point at every door; the mount is create's.
// The preview checks the read-only narrowing and leaves runner and share
// readiness pending. No drive flag is a provable no-op: no store read at all.
func (s *Server) stepDrive(f *runFold, rec *foldRecorder) bool {
	if f.mode == foldPreview {
		drive, refusal := s.authorizeRequestDrive(f.r, *f.req, f.ceiling)
		if refusal.write(s, f.w, f.r) {
			return false
		}
		return drive == nil || !driveReadOnlyRefusal(*f.req, *drive).write(s, f.w, f.r)
	}
	var ok bool
	f.driveMount, ok = s.seedRequestDrive(f.w, f.r, *f.req, f.ceiling)
	return ok
}

// stepWorkspaces reads the workspaces the seeded spec references. Nothing after
// this step changes the spec's mounts or repos, so the list stays valid for the
// requirements fold, the egress unions and image resolution.
func (s *Server) stepWorkspaces(f *runFold, rec *foldRecorder) bool {
	f.wsRefs = s.referencedWorkspaces(f.r.Context(), f.spec)
	return true
}

// stepPreviewEgress widens the dry doors' spec from onboarded-workspace
// registries and clone hosts the way create's post-mint unionRunEgress will,
// side-effect free, so Review grades the envelope the run is launched with.
// The SSH, site-config and ADO lanes need grant wiring that does not exist
// before the mint and stay create's.
func (s *Server) stepPreviewEgress(f *runFold, rec *foldRecorder) bool {
	unionPreviewWorkspaceEgress(&f.spec, f.wsRefs)
	return true
}

// stepRequirements folds each referenced workspace's requirements contract
// into the spec before the confinement floor and the grade read it: a
// workspace's integration:<id> requirement is a credential grant the CC3
// blast-radius floor must see, or invariant 5 is silently bypassed. This is
// the audit-free half; create records reqEvents once its run id exists.
func (s *Server) stepRequirements(f *runFold, rec *foldRecorder) bool {
	ctx := f.r.Context()
	// Caller-scoped secret names, resolved once: the requirement fold and
	// Review's checklist read the same map.
	f.present = s.presentSecretNamesFor(ctx, s.secretOwnerFromRequest(f.r))
	f.reqEvents = s.applyWorkspaceRequirementsFor(ctx, f.present, &f.spec, f.req.Agent, f.wsRefs, resolveWorkspaceSelections(*f.req))
	return true
}

// stepGitHubEgress runs after the requirement fold, which decides whether a
// github_token grant survives: a dropped grant leaves a direct clone, which
// needs GitHub egress the floor below must see. create audits the hosts it adds
// after the mint (unionRunEgress).
func (s *Server) stepGitHubEgress(f *runFold, rec *foldRecorder) bool {
	var refusal *runRefusal
	f.directGitHubAdded, refusal = s.unionDirectGitHubEgress(f.r, *f.req, &f.spec, f.ceiling)
	return !refusal.write(s, f.w, f.r)
}

// stepComponents bounds the run's components and expands them onto the spec:
// after the folds above, so a host an admin already credentialed is seen;
// before the floor, the model-provider choice and the autonomy grade, which
// read what a component adds. The preview reports a secret that is not stored
// yet rather than refusing it.
func (s *Server) stepComponents(f *runFold, rec *foldRecorder) bool {
	var refusal *runRefusal
	f.comps, refusal = s.applyRunComponents(f.r, *f.req, &f.spec, f.ceiling, f.wsRefs, f.mode != foldPreview)
	return !refusal.write(s, f.w, f.r)
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
		f.enforced, ok = s.resolveEnforcedConfinement(f.r.Context(), f.w, floor, f.reqCC, f.baseline)
		return ok
	}
	var advertised []types.ConfinementClass
	if f.mode == foldPreflight {
		reqCC, ok := parseConfinementClass(f.req.ConfinementClass)
		if !ok {
			writeErrorReason(f.w, http.StatusBadRequest, reasonConfinementClassUnknown, fmt.Sprintf("unknown confinement_class %q", f.req.ConfinementClass))
			return false
		}
		f.reqCC = reqCC
		advertised = s.advertisedConfinement(f.r.Context())
	}
	enforced, err := enforcedConfinement(floor, f.reqCC, advertised, f.baseline)
	if err != nil {
		writeErrorReason(f.w, http.StatusUnprocessableEntity, reasonConfinementClassConflict, err.Error())
		return false
	}
	f.enforced = enforced
	return true
}

// stepModelProvider makes the model-provider choice and refuses one whose
// credential the caller does not hold, here rather than at dispatch. It runs
// before autonomy because that gate grades this credential (#504). Only create
// renews an expired AWS sign-in; the preview authorizes the selection without
// reading any credential and leaves the choice pending when nothing pins it.
func (s *Server) stepModelProvider(f *runFold, rec *foldRecorder) bool {
	if f.mode == foldPreview {
		choice, refusal := s.authorizeRunModelProvider(f.r, *f.req, f.wsRefs, true)
		if refusal.write(s, f.w, f.r) {
			return false
		}
		f.mpChoice = choice
		_, needsModel := agentLLMProvider(f.req.Agent)
		if needsModel && createDoorIsModelRun(*f.req) {
			if name, secretName, found := modelEnvSecretGrant(f.spec); found {
				s.writeProviderRefusal(f.w, f.r, choice.provider.ID, choice.provider.Kind, fmt.Sprintf(mpRunModelEnvSecret, secretName, name), false)
				return false
			}
		}
		return true
	}
	choice, ok := s.enforceRunModelProvider(f.w, f.r, *f.req, f.spec, f.wsRefs, f.mode == foldCreate)
	if !ok {
		return false
	}
	f.mpChoice = choice
	f.modelCred = choice.modelCredential()
	return true
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

// stepADOStanding bounds a member's Azure DevOps list (#1384); the narrowing is
// said out loud at every door. A list that leaves nothing standing is
// dispatch's refusal at create, which the dry doors answer before the click.
func (s *Server) stepADOStanding(f *runFold, rec *foldRecorder) bool {
	narrowed, none := s.adoStandingAtDoor(f.r, f.spec, f.scmSite, f.ceiling)
	if f.mode != foldCreate {
		if none {
			writeErrorReason(f.w, http.StatusUnprocessableEntity, reasonADOCapabilitiesNonePermitted, adoNonePermitted(f.spec.AzureDevOpsCapabilities))
			return false
		}
	}
	f.adoNarrowed = narrowed
	return true
}

// stepPATNarrowing answers dispatch's git_pat narrowing refusals at the dry
// doors, the same reasons and sentences, so a narrowing the run could not
// enforce shows before the click. Create meets them at dispatch.
func (s *Server) stepPATNarrowing(f *runFold, rec *foldRecorder) bool {
	if reason, detail := s.patNarrowingAtDoor(f.r, f.spec, f.scmSite); reason != "" {
		writeErrorReason(f.w, http.StatusUnprocessableEntity, reason, detail)
		return false
	}
	return true
}

// stepHostCapacity is the same 503, reason and Retry-After at create and
// Review; only create writes the audit row.
func (s *Server) stepHostCapacity(f *runFold, rec *foldRecorder) bool {
	return !writeHostCapacityRefusal(f.w, f.r, s.admitHostCapacity(f.r.Context(), principalFromRequest(f.r), "runs", f.mode == foldCreate))
}

// stepRunCap refuses a full deployment before the mint: no identity.mint row
// and no live token for a run that gets no row. CreateRunUnderCap still
// decides a race at the cap.
func (s *Server) stepRunCap(f *runFold, rec *foldRecorder) bool {
	return !s.refuseRunCapFull(f.w, f.r)
}

// stepRunFit refuses a run the runs namespace's ResourceQuota cannot hold; its
// advisories join the door's warnings. The quota's own admission stays the
// authority on a race.
func (s *Server) stepRunFit(f *runFold, rec *foldRecorder) bool {
	var refused bool
	f.fitWarnings, refused = s.refuseRunFit(f.w, f.r, s.runFitSpec(f.r.Context(), f.spec, f.ceiling))
	return !refused
}
