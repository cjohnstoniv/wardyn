// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
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
// wedged docker pull would hold the create handler open forever. Generous: a cold
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
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.workspace.collision",
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
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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
	spec, policyID, policyWarns, ok := s.resolveRunPolicy(ctx, w, r, &req, false)
	if !ok {
		return
	}
	// Fold the named workspace onto the resolved spec, then re-run every check
	// that seeding can invalidate. Writes its own error and stops on false.
	ephemeralDirs, ok := s.seedAndAdmitWorkspace(ctx, w, r, &spec, &req)
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

	// Fold the run's model-access binding AND each referenced workspace's
	// requirements contract into the spec BEFORE the confinement floor + risk
	// grade read it. The deterministic CC3 blast-radius floor is
	// computed from spec.EligibleGrants, so a workspace's integration:<id>
	// requirement — a third-party/production api_key grant — must be present when
	// the floor is computed, or invariant 5's "powerful credentials run in the
	// strongest sandbox" is silently bypassed. wsRefs is derived from the seeded spec's
	// mounts/repos, which nothing below mutates, so it is equally valid here and is
	// reused for the egress union + image resolution. Both folds are the AUDIT-FREE
	// halves (foldRunIntegration; applyWorkspaceRequirements returns its events) —
	// the audit is recorded once the run id is minted, below.
	wsRefs := s.referencedWorkspaces(ctx, spec)
	// Caller-scoped secret namespace, resolved once: the presence map and the
	// integration fold must read the SAME one. Shared with the warning below.
	secretOwner := s.secretOwnerFromRequest(r)
	present := s.presentSecretNamesFor(ctx, secretOwner)
	foldInteg, foldKind, bedrockRef := s.foldRunIntegration(ctx, secretOwner, &spec, req, wsRefs)
	reqEvents := s.applyWorkspaceRequirementsFor(ctx, present, &spec, req.Agent, wsRefs, resolveWorkspaceSelections(req))

	// The primary host workspace directory this run will operate in (if any), used
	// below to DISCOURAGE — warn, never block — sharing a directory with another
	// active run.
	workspacePath := primaryWorkspacePath(spec)

	// Resolve + gate the confinement class (request vs policy floor, the CC3
	// blast-radius floor now computed on the FOLDED spec, runner capability
	// membership, cloud_sts grant gating) — invariant 5, fail closed.
	enforced, ok := s.resolveEnforcedConfinement(ctx, w, spec, reqCC)
	if !ok {
		return
	}

	// Posture-gated autonomy, resolved ONCE and enforced at both doors
	// (handlePreflightRun calls the SAME gate). Sited immediately after the
	// enforced class because that class is the posture's third axis — and
	// before the mint, so a refusal leaves no run row, and before
	// createRunAuditData and the dispatchParams literal below, which both read
	// the req.ToolApprovals this gate may derive to `hold`. Writes its own 403
	// and stops on false; its warnings join the 201 list further down.
	// scmSite is the one site-config snapshot the gate graded the SCM-host lane
	// from; unionRunEgress below dispatches from the same value.
	autonomy, autonomyWarns, scmSite, ok := s.resolveRunAutonomy(w, r, &req, spec, wsRefs, enforced, ceiling)
	if !ok {
		return
	}

	// No cross-mechanism fallback, at the door: when the org declared how this
	// agent reaches its model and the lane that would carry this run is not that
	// one, refuse HERE — before a run row, an identity or a grant exists — rather
	// than let the person watch a sandbox boot and die. With no declaration the
	// model-access finding stays the 201 advisory it has always been (below).
	// Writes its own 422; see enforceCreateLLMMechanism.
	// runIdentitySubject(principalFromRequest), NEVER secretOwnerFromRequest: the
	// subject is the secret NAMESPACE a run resolves against, and the adjacent
	// owner helper answers "" for every operator — which would refuse an ADMIN
	// their own per_user capture here while dispatch, reading run.CreatedBy,
	// resolved it fine.
	ssoSubject := runIdentitySubject(ctx, principalFromRequest(r))
	// refresh=true: the real launch redeems an expired-but-renewable session
	// here, so a spent one is refused before any run exists.
	if !s.enforceCreateLLMMechanism(ctx, w, req, spec, bedrockRef, ssoSubject, nil, true) {
		return
	}

	createdByType, createdBy := actorFromRequest(r)
	runID := uuid.New()
	// Subject vs attribution: createdBy is the ATTRIBUTION — the run row's
	// CreatedBy, the sponsor claim, every audit actor — and in LocalMode it may be
	// the DEV-ONLY X-Wardyn-Principal header. The SUBJECT is what selects the
	// secret namespace at mint/inject time, so it comes from runIdentitySubject,
	// which no request header can move.
	id, err := s.cfg.Identity.MintRunIdentity(ctx, runID, runIdentitySubject(ctx, createdBy), createdBy, internalAudience)
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
	}
	created, err := s.cfg.Store.CreateRun(ctx, run)
	if err != nil {
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

	// resolveRunPolicy's own notes come FIRST: they are the ones that say the
	// run is narrower than what the caller asked for, and launch is the ONLY
	// place a member sees that — preflight, which carries the same notes, is
	// never called by the console. The strings are
	// narrowMemberInlinePolicy's/filterMemberGrants' own: they name the kind
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

	// The model-access + requirements folds that ran ABOVE the confinement floor,
	// audited now that the run id exists. See recordCreateFolds.
	s.recordCreateFolds(ctx, runID, foldInteg, foldKind, reqEvents)

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

	s.recordAudit(ctx, s.auditEvent(&runID, createdByType, createdBy, "run.create",
		runID.String(), "success", mustJSON(createRunAuditData(req, policyID, enforced, reqCC, id.JTI, policyWarns, autonomy))))

	// Model-resolution fail-fast, as a warning; see noModelAccessWarning.
	warnings = append(warnings, s.noModelAccessWarning(ctx, req, spec, present, bedrockRef, ssoSubject)...)

	// Widen the RESOLVED spec's egress from the deterministic operator-trusted
	// sources (onboarded-workspace registries, site-config SCM hosts, the SSH and
	// ADO SCM lanes) — never the LLM; see unionRunEgress.
	s.unionRunEgress(ctx, runID, &spec, gw, wsRefs, req.Repo, scmSite)

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

	// Client-disconnect isolation, same rationale as dispatchRun's own
	// detach — which sits AFTER this block and so never covered it. From here on the
	// run row exists and MUST be driven to a terminal state or dispatched. An image
	// build is a multi-minute docker pull+build that honours cancellation, so a
	// client Ctrl-C, closed tab, or LB read timeout would abort an otherwise-healthy
	// build AND — far worse — take the FAILED-compensators below down with it: their
	// CAS runs on this same ctx and cannot write the state it exists to write, so the
	// run strands PENDING with no audit and no revoke until the next daemon boot.
	// Detach from cancellation (values preserved); the build gets its own explicit
	// deadline below instead of the client's connection being the de-facto one.
	ctx = context.WithoutCancel(ctx)

	// Resolve the sandbox image (BYOI wrap > devcontainer build > workspace
	// profile > convention image) and persist it for provenance. A failed
	// BYOI/devcontainer build has already marked the run FAILED; this frame
	// answers the 201 with the refreshed (FAILED) run, since
	// resolveCreateRunImage itself must stay callable off-request. The one
	// image lane that DEGRADES rather than refusing. With no ImageBuilder wired
	// a workspace base_image fails closed (inside resolveCreateRunImage) but a
	// devcontainer_repo silently falls through to the convention image —
	// otherwise visible only as an INFO setup row no CLI or API caller ever
	// reads. Said on the 201 instead, which is the only channel this door has.
	// Appended BEFORE the call so the warning is already on the list the
	// build-failed arm answers 201 with.
	warnings = s.appendDevcontainerNoBuilderWarning(warnings, req)
	image, failed := s.resolveCreateRunImage(ctx, req, runID, wsRefs)
	if failed {
		created = s.refreshRun(ctx, runID, created)
		writeJSON(w, http.StatusCreated, createRunResponse{AgentRun: created, Warnings: warnings})
		return
	}

	// Dispatch the sandbox if a runner is wired; otherwise stay PENDING.
	if s.cfg.Runner != nil {
		// The dispatch-time deny re-assertion's two inputs (runs_dispatch_ceiling.go).
		// A create-time deny alone is not enough — the artifact-redirect phase INSIDE
		// dispatch adds corporate hosts and authors token injections for them, AFTER
		// this handler's clamp ran — so the profile's walls have to be re-asserted
		// there. ceilingForDispatch owns the absent-row scoping (it answers a
		// RESOLVED empty ceiling for anyone with no assigned profile, operators
		// included), so the doctrine is decided once, in one function, and not
		// re-decided here.
		s.dispatchRun(ctx, created, ceilingForDispatch(ceiling), dispatchParams{
			RunToken:           id.Token,
			Image:              image,
			Policy:             spec,
			FirstGitHubGrantID: gw.firstGitHubGrantID,
			GitGrants:          gw.gitGrants,
			GitPATGrants:       gw.gitPATGrants,
			SSHGrants:          gw.sshGrants,
			Injections:         gw.injections,
			Interactive:        req.Interactive,
			TaskMode:           req.TaskMode,
			InteractiveStart:   req.InteractiveStart,
			SeedAutoTools:      req.SeedAutoTools,
			ToolApprovals:      req.ToolApprovals,
			BedrockRef:         bedrockRef,
			EphemeralDirs:      ephemeralDirs,
			Toolchains:         runToolchainNeeds(wsRefs),
			// The member's own persistent storage, already resolved and narrowed
			// at create (seedRequestDrive) — nil unless this run asked for it.
			// Carried here rather than re-resolved inside dispatch for the reason
			// the ceiling is: resolution keys on the caller's OIDC claims, which
			// the run row does not hold, so dispatch has no identity to resolve
			// FROM. See user_drives_run.go's own note.
			Drive: driveMount,
			// The zero posture unless this run attaches a MEMBER-OWNED workspace, in
			// which case the driver re-checks that member's own binds against these
			// roots immediately before ContainerCreate (memberMountPosture,
			// workspace_refs.go).
			MemberMounts: s.memberMountPosture(wsRefs),
		})
		// Re-read so the response reflects the post-dispatch state.
		created = s.refreshRun(ctx, runID, created)
	}

	writeJSON(w, http.StatusCreated, createRunResponse{AgentRun: created, Warnings: warnings})
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
func (s *Server) seedAndAdmitWorkspace(ctx context.Context, w http.ResponseWriter, r *http.Request, spec *types.RunPolicySpec, req *createRunRequest) ([]string, bool) {
	// First, before a single source is folded: authorize the SELECTION against
	// the CALLER. Everything below this line reasons about host paths that are
	// about to become binds, and until now nothing on the path asked whose
	// workspace they came from — the only member-mount check downstream is
	// evaluated against the workspace OWNER, so any member (and a security
	// admin, a tier defined never to reach the host) could name another
	// member's workspace id and get their directory bound inside a sandbox they
	// control. The store-less case is left to seedRequestWorkspace, which
	// answers it with its own 422.
	if req.WorkspaceID != nil && s.cfg.Store != nil {
		if _, ok := s.getWorkspaceLaunchable(w, r, *req.WorkspaceID); !ok {
			return nil, false
		}
	}
	ephemeralDirs, seededImageOwner, code, seedErr := s.seedRequestWorkspace(ctx, spec, req)
	if seedErr != nil {
		writeError(w, code, "workspace_id: "+seedErr.Error())
		return nil, false
	}
	if s.denyMemberSeededImage(w, r, seededImageOwner, req.Image) {
		return nil, false
	}
	if msg := s.validateImageBuildRequest(*req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return nil, false
	}
	if code, err := s.validateWorkspaceSources(ctx, *spec); err != nil {
		writeError(w, code, "workspace: "+err.Error())
		return nil, false
	}
	// The caller-scoped twin of the onboarding gate above: onboarded is not the
	// same question as "onboarded BY SOMEONE THIS CALLER MAY LAUNCH AS", and a
	// policy naming a host path directly never passes through the workspace_id
	// door that answers the second one.
	if code, err := s.authorizeSpecWorkspaceSources(ctx, r, *spec); err != nil {
		writeError(w, code, "workspace: "+err.Error())
		return nil, false
	}
	// Both provider gates sit at this chokepoint, over the RESOLVED spec's repos
	// — the un-bypassable one, reached alike by workspace_id, a stored policy and
	// a hand-authored inline policy. ADMISSION runs first (the operator-binding
	// question), then the member capability.
	//
	// HERE and not in validateWorkspaceSources above, which is the other function
	// that sees the resolved spec: this scope holds the request, so the refusal
	// can read the caller's tier (a member's 403 names the kind, an operator's 422
	// lists the addresses) and answer the frozen sentence verbatim rather than
	// through that function's "workspace: " error prefix. Its other two callers
	// are the POLICY write doors (policies.go), where nothing clones.
	repos := repoLocatorsOf(spec.WorkspaceRepos)
	if s.admitRepoSources(w, r, repos...) {
		return nil, false
	}
	if s.denyMemberWorkspaceProviders(w, r, "runs.workspace_provider", repos...) {
		return nil, false
	}
	return ephemeralDirs, true
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
// 201: a non-interactive harness run whose agent needs a model but has NO
// resolvable credential boots and 404s on its FIRST model call — classically a
// codex-cli run whose only model access is a claude-code-only managed
// subscription.
//
// WARN, never hard-reject (edge cases); the CLI already prints warnings, so the
// operator sees it before the run wastes a sandbox. Computed on the resolved
// spec through the SAME helper preflight's checklist uses, so the two agree.
// Extracted from handleCreateRun for the function-size gate.
func (s *Server) noModelAccessWarning(ctx context.Context, req createRunRequest, spec types.RunPolicySpec,
	present map[string]bool, bedrockRef *types.WorkspaceBedrockRef, ssoSubject string,
) []string {
	if !runNeedsModelWarning(req) {
		return nil
	}
	if la := s.resolveRunLLMAccess(ctx, req, spec, present, bedrockRef, ssoSubject); la != nil && la.Provisioned {
		return nil
	}
	return []string{s.noModelAccessWarningFor(req.Agent)}
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
func createRunAuditData(req createRunRequest, policyID *uuid.UUID, enforced types.ConfinementClass, reqCC types.ConfinementClass, jti string,
	clampWarnings []string, autonomy types.AutonomyResolution,
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
		// policy's own policy.inline row carries no run id, and run.policy.effective
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

// resolveRunLLMAccess computes the deterministic model-access verdict for a run's
// RESOLVED spec — the SAME computation preflight's checklist uses, shared so the
// create-path warning and the preflight row can never disagree. It mirrors dispatch's
// precedence (managed subscription > api-key, with an operator-Bedrock fallback) and
// returns nil only for a non-LLM agent (nothing to resolve).
//
// It runs on a CLONE: reconcileLLMAccess drops orphaned grants IN PLACE, but the
// caller's spec is still persisted/dispatched here, so it must never be mutated. The
// grants slice is cloned because a struct copy shares the backing array and
// slices.DeleteFunc zeroes the vacated tail in place.
// subject is the CALLER's run-identity subject — whose captured AWS SSO session
// this run would resolve under a per_user roster row. "" is the operator
// namespace, i.e. every deployment that never declared per_user.
func (s *Server) resolveRunLLMAccess(ctx context.Context, req createRunRequest, spec types.RunPolicySpec, presentSecrets map[string]bool, bedrockRef *types.WorkspaceBedrockRef, subject string) *composeLLMAccess {
	llmSpec := spec
	llmSpec.EligibleGrants = slices.Clone(spec.EligibleGrants)
	// Which lanes this run has available — resolved by the same helper the
	// create-time mechanism refusal uses (resolveRunLLMLanes), so the advisory
	// below and that refusal can never disagree about what would credential this
	// run.
	// The create-path advisory, not a credential door: this scope feeds
	// resolveRunLLMAccess's reply Note and the preflight checklist row — what a
	// run WOULD dispatch on. ok is ignored on purpose: an unreadable roster
	// degrades the ADVICE to the legacy operator answer exactly as it always
	// has, and nothing is written, served or deleted on it. The doors that
	// decide where a credential is written, deleted or SERVED take ok
	// (harnesscred.go, ssotoken.go, enforceReadableRosterForCredential).
	ssoScope, _ := s.awsSSOScopeForAgent(ctx, req.Agent, subject)
	lanes := s.resolveRunLLMLanes(ctx, req, &llmSpec, bedrockRef, ssoScope, false)
	var llmAccess *composeLLMAccess
	if note, provisioned := s.reconcileLLMAccess(&llmSpec, req.Agent, presentSecrets, s.subscriptionInjectEnabled(), lanes.managed); note != "" {
		llmAccess = &composeLLMAccess{Provisioned: provisioned, Note: note}
	}
	// Operator-configured Bedrock credentials the run automatically: dispatch's
	// resolveBedrockAuth OVERRIDES the per-run api-key selection at launch. Thread the
	// picked workspace/container's bedrockRef so a per-run region/model
	// override is honored here too — a workspace can only narrow region/model, never
	// supply credentials — matching what launch enforces.
	if llmAccess == nil || !llmAccess.Provisioned {
		if ba := lanes.bedrock; ba.ready {
			llmAccess = &composeLLMAccess{
				Provisioned: true,
				Note:        "Amazon Bedrock is configured by the operator (region " + ba.region + ", model " + ba.model + "); this run uses it automatically — no per-run API key is needed.",
			}
		}
	}
	return llmAccess
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

// noModelAccessWarning is the create-time advisory for a run whose agent needs a
// model but has no resolvable credential: it will boot and 404 on its first model
// call. managedClaudePresent on a non-claude agent is the canonical trap — a
// managed Claude subscription credentials claude-code only — so the copy names it and
// steers to --agent claude-code.
func noModelAccessWarning(agent string, p llmProvider, managedClaudePresent bool) string {
	msg := fmt.Sprintf(
		"no model credential resolves for agent %q — this run will boot and fail on its first model call (it needs %s "+
			"access via a %q secret, a bound workspace/integration credential, or Bedrock).",
		agent, p.host, p.secret)
	if managedClaudePresent && agent != "claude-code" {
		msg += " A Wardyn-managed Claude subscription is connected, but it credentials claude-code only — " +
			"use --agent claude-code, or connect a credential for this agent."
	}
	return msg
}

// noModelAccessWarningFor renders the no-model-access warning for `agent`
// against its (gateway-aware) provider and the managed-inject readiness.
func (s *Server) noModelAccessWarningFor(agent string) string {
	p, _ := s.llmProviderFor(agent)
	return noModelAccessWarning(agent, p, s.managedInjectReady("claude-code"))
}
