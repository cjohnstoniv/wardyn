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

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// imageBuildTimeout bounds a per-run sandbox image build (BYOI wrap, devcontainer,
// workspace profile). The build is detached from the request ctx so a client
// disconnect cannot abort it — which leaves it needing a deadline of its own, or a
// wedged docker pull would hold the create handler open forever. Generous: a cold
// devcontainer build pulls a base image and runs the repo's full setup.
const imageBuildTimeout = 30 * time.Minute

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
func (s *Server) warnWorkspaceCollision(ctx context.Context, runID uuid.UUID, workspacePath string) []string {
	if workspacePath == "" {
		return nil
	}
	existing, lerr := s.cfg.Store.ListRuns(ctx)
	if lerr != nil {
		return nil
	}
	var others []string
	for _, e := range existing {
		if e.ID != runID && e.WorkspacePath == workspacePath && !isTerminalRunState(e.State) {
			others = append(others, e.ID.String())
		}
	}
	if len(others) == 0 {
		return nil
	}
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.workspace.collision",
		workspacePath, "success", mustJSON(map[string]any{"other_runs": others})))
	return []string{fmt.Sprintf(
		"host workspace %q is already in use by %d active run(s) (%s); independent agents sharing a directory can interfere — proceeding anyway",
		workspacePath, len(others), strings.Join(others, ", "))}
}

// handleCreateRun validates policy, gates on confinement class against what the
// runner can actually enforce (fail closed), persists the run + its grants,
// mints the run identity, and (if a runner is wired) dispatches the sandbox.
// Without a runner the run stays PENDING with a clear status message (headless
// API-only operation is allowed for v0).
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, reqCC, taskWarning, ok := s.decodeAndValidateCreateRun(w, r)
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
	// workspace_id: seed the named onboarded workspace's sources onto the resolved
	// spec (prepended, so it is the PRIMARY) before anything reads it. This is what
	// lets a CLI/SDK caller launch against a workspace without hand-reproducing its
	// exact source path in a policy file — see seedRequestWorkspace.
	ephemeralDirs, code, seedErr := s.seedRequestWorkspace(ctx, &spec, &req)
	if seedErr != nil {
		writeError(w, code, "workspace_id: "+seedErr.Error())
		return
	}
	// A workspace's base_image may have just set req.Image (seedRequestWorkspace):
	// re-run the image/devcontainer_repo XOR + builder-wired check
	// decodeAndValidateCreateRun already ran on an EXPLICIT --image, since that
	// ran before workspace_id was resolved and would otherwise let a
	// workspace's base_image bypass it entirely.
	if msg := s.validateImageBuildRequest(req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	// ONBOARDING GATE (un-bypassable): every user-workspace mount source and repo on
	// the RESOLVED spec must be a pre-onboarded workspace. Runs over inline, stored,
	// and default policies alike (this is the single chokepoint on the resolved
	// spec), so a hand-authored stored policy cannot smuggle an arbitrary host path
	// or repo. System credential mounts are exempt by target.
	if code, err := s.validateWorkspaceSources(ctx, spec); err != nil {
		writeError(w, code, "workspace: "+err.Error())
		return
	}

	// Fold the run's model-access binding AND each referenced workspace's
	// requirements contract into the spec BEFORE the confinement floor + risk
	// grade read it (SPINE-2/SPINE-6). The deterministic CC3 blast-radius floor is
	// computed from spec.EligibleGrants, so a workspace's integration:<id>
	// requirement — a third-party/production api_key grant — must be present when
	// the floor is computed, or invariant 5's "powerful credentials run in the
	// strongest sandbox" is silently bypassed (the grant used to land AFTER the
	// class was already fixed at CC1/CC2). wsRefs is derived from the seeded spec's
	// mounts/repos, which nothing below mutates, so it is equally valid here and is
	// reused for the egress union + image resolution. Both folds are the AUDIT-FREE
	// halves (foldRunIntegration; applyWorkspaceRequirements returns its events) —
	// the audit is recorded once the run id is minted, below.
	wsRefs := s.referencedWorkspaces(ctx, spec)
	foldInteg, foldKind, bedrockRef := s.foldRunIntegration(ctx, &spec, req, wsRefs)
	reqEvents := s.applyWorkspaceRequirements(ctx, &spec, req.Agent, wsRefs, resolveWorkspaceSelections(req))

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

	createdByType, createdBy := actorFromRequest(r)
	runID := uuid.New()
	id, err := s.cfg.Identity.MintRunIdentity(ctx, runID, createdBy, createdBy, internalAudience)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mint run identity: "+err.Error())
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
		WorkspaceIDs:     workspaceIDsOf(wsRefs), // resolved at :142, same spec as WorkspacePath above
		AutoStopAfterSec: spec.AutoStopAfterSec,
	}
	created, err := s.cfg.Store.CreateRun(ctx, run)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create run: "+err.Error())
		return
	}

	// resolveRunPolicy's own notes come FIRST: they are the ones that say the
	// run is narrower than what the caller asked for. Launch used to drop them
	// on the floor, leaving only preflight (which the console never calls) to
	// tell a member their egress host or secret grant had been dropped — so
	// three shipped claims about drops "surfacing as a warning before launch"
	// were true of nothing a member ever saw. The strings are
	// narrowMemberInlinePolicy's/filterMemberGrants' own: they name the kind
	// and the dropped VALUE (a host, a secret NAME), never a secret value.
	warnings := append(policyWarns, s.warnWorkspaceCollision(ctx, runID, workspacePath)...)
	if taskWarning != "" {
		warnings = append(warnings, taskWarning)
	}

	// Record the model-access + requirements folds that ran ABOVE the confinement
	// floor (SPINE-2). The spec was already mutated there — so the floor/grade saw
	// the full picture — and those same grants still reach persistRunGrants below
	// (it snapshots this same spec.EligibleGrants into the persisted grants + proxy
	// injections). foldRunIntegration is the audit-free fold; the audit is emitted
	// here now that runID exists (kept split so preflight can call the same fold).
	if foldKind != "" {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.workspace.creds",
			runID.String(), "success", mustJSON(map[string]any{"integration_ref": foldInteg.ID, "type": foldKind})))
	}
	for _, ev := range reqEvents {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", ev.action, ev.target, "success", mustJSON(ev.data)))
	}

	// Persist the eligibility records + derive the non-secret sandbox wiring
	// (github/git_pat/ssh grant ids, api_key proxy injections, SCM egress) —
	// see persistRunGrants. A grant write failure has already answered 500.
	gw, ok := s.persistRunGrants(ctx, w, runID, now, spec)
	if !ok {
		return
	}
	// SSH-lane honesty: drop the wiring for agents with no SSH clone lane
	// (codex-cli) and advise BYOI images about the required tools.
	warnings = append(warnings, s.applySSHLaneWarnings(ctx, req, runID, &gw)...)

	// Git-broker: map the run's declared GitHub clone set to its github grant so the
	// proxy's /wardyn/gh/ route serves exactly these repos (github.com is dropped
	// from egress; an un-granted github repo is denied).
	gw.augmentGitBrokerGrants(req.Repo, spec.WorkspaceRepos)

	createAuditData := map[string]any{
		"agent": req.Agent, "repo": req.Repo, "policy_id": policyID,
		"confinement_class": enforced, "jti": id.JTI,
		"inline_policy": req.InlinePolicy != nil,
	}
	if req.TaskMode == "exec" {
		// The run row doesn't store task_mode (request-scoped), so the audit
		// event is the provenance record that this run ran a plain command.
		createAuditData["task_mode"] = req.TaskMode
	}
	if req.Interactive && req.InteractiveStart != "" {
		// Same reason as task_mode above: interactive_start is request-scoped, so
		// the audit event is the only record that this sandbox opened straight
		// into the agent CLI rather than a bare shell.
		createAuditData["interactive_start"] = req.InteractiveStart
	}
	if req.Interactive && req.SeedAutoTools {
		// SeedAutoTools is request-scoped like interactive_start above; only
		// meaningful alongside a non-empty Task (the boot seed) — recorded
		// whenever requested regardless, since dispatch's own `interactive &&`
		// gate is what makes it structurally inert otherwise.
		createAuditData["seed_auto_tools"] = req.SeedAutoTools
	}
	if req.ToolApprovals != "" {
		// Request-scoped like task_mode: this is the only record that an
		// autonomous run's tool calls were routed to Wardyn approvals instead of
		// running unsupervised.
		createAuditData["tool_approvals"] = req.ToolApprovals
	}
	s.recordAudit(ctx, s.auditEvent(&runID, createdByType, createdBy, "run.create",
		runID.String(), "success", mustJSON(createAuditData)))

	// Model-resolution fail-fast (AGT4-2): a non-interactive harness run whose agent
	// needs a model but has NO resolvable credential boots and 404s on its FIRST model
	// call — classically a codex-cli run whose only model access is a claude-code-only
	// managed subscription. WARN (never hard-reject: edge cases); the CLI already prints
	// warnings, so the operator sees it before the run wastes a sandbox. Computed on the
	// resolved spec via the SAME helper preflight's checklist uses, so the two agree.
	if runNeedsModelWarning(req) {
		if la := s.resolveRunLLMAccess(ctx, req, spec, s.presentSecretNames(ctx), bedrockRef); la == nil || !la.Provisioned {
			p, _ := agentLLMProvider(req.Agent)
			warnings = append(warnings, noModelAccessWarning(req.Agent, p, s.managedInjectReady("claude-code")))
		}
	}

	// Widen the RESOLVED spec's egress from the deterministic operator-trusted
	// sources (onboarded-workspace registries, site-config SCM hosts, the SSH and
	// ADO SCM lanes) — never the LLM; see unionRunEgress.
	s.unionRunEgress(ctx, runID, &spec, gw, wsRefs, req.Repo)

	// CLIENT-DISCONNECT ISOLATION, same rationale as dispatchWithVerify's own
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
	// BYOI/devcontainer build has already marked the run FAILED and answered 201.
	image, responded := s.resolveCreateRunImage(ctx, w, req, runID, created, warnings, wsRefs)
	if responded {
		return
	}

	// Dispatch the sandbox if a runner is wired; otherwise stay PENDING.
	if s.cfg.Runner != nil {
		s.dispatchRun(ctx, created, dispatchParams{
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
func (s *Server) resolveRunLLMAccess(ctx context.Context, req createRunRequest, spec types.RunPolicySpec, presentSecrets map[string]bool, bedrockRef *types.WorkspaceBedrockRef) *composeLLMAccess {
	llmSpec := spec
	llmSpec.EligibleGrants = slices.Clone(spec.EligibleGrants)
	_, hasAnthropicKey := apiKeyGrantForHost(&llmSpec, "api.anthropic.com")
	subscriptionActive := specHasMountTarget(&llmSpec, claudeCredTarget)
	// managed mirrors dispatch's precedence: a compose-mode managed token credentials a
	// claude run with no resident subscription mount and no anthropic api-key grant.
	managed := req.Agent == "claude-code" && !subscriptionActive &&
		!hasAnthropicKey && s.managedInjectReady(req.Agent) &&
		(llmSpec.AllowAllEgress || len(llmSpec.AllowedDomains) > 0)
	var llmAccess *composeLLMAccess
	if note, provisioned := reconcileLLMAccess(&llmSpec, req.Agent, presentSecrets, s.subscriptionInjectEnabled(), managed); note != "" {
		llmAccess = &composeLLMAccess{Provisioned: provisioned, Note: note}
	}
	// Operator-configured Bedrock credentials the run automatically: dispatch's
	// resolveBedrockAuth OVERRIDES the per-run api-key selection at launch. Thread the
	// picked workspace/container's bedrockRef (SPINE-5) so a per-run region/model
	// override is honored here too — a workspace can only narrow region/model, never
	// supply credentials — matching what launch enforces.
	if llmAccess == nil || !llmAccess.Provisioned {
		if ba := s.resolveBedrockAuth(ctx, req.Agent, subscriptionActive, true, bedrockRef); ba.ready {
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
// call. managedClaudePresent on a non-claude agent is the canonical trap (AGT4-2) — a
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
