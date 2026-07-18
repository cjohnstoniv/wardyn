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
	if _, known := confinementRank[cc]; !known {
		return "", false
	}
	return cc, true
}

// handleCreateRun validates policy, gates on confinement class against what the
// runner can actually enforce (fail closed), persists the run + its grants,
// mints the run identity, and (if a runner is wired) dispatches the sandbox.
// Without a runner the run stays PENDING with a clear status message (headless
// API-only operation is allowed for v0).
func (s *Server) handleCreateRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	req, reqCC, ok := s.decodeAndValidateCreateRun(w, r)
	if !ok {
		return
	}

	// Resolve the policy: inline_policy (validated here), explicit policy_id, or
	// the configured default. resolveRunPolicy writes its own HTTP error and
	// returns ok=false when it has already responded (XOR violation, invalid
	// inline spec, missing/reserved inline secret ref, …) so we just stop.
	spec, policyID, ok := s.resolveRunPolicy(ctx, w, r, &req, false)
	if !ok {
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
	// The primary host workspace directory this run will operate in (if any), used
	// below to DISCOURAGE — warn, never block — sharing a directory with another
	// active run.
	workspacePath := primaryWorkspacePath(spec)

	// Resolve + gate the confinement class (request vs policy floor, the CC3
	// blast-radius floor, runner capability membership, cloud_sts grant gating) —
	// invariant 5, fail closed; see resolveEnforcedConfinement.
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
		PolicyID:         policyID,
		ConfinementClass: enforced,
		State:            types.RunPending,
		SPIFFEID:         id.SPIFFEID,
		RunnerTarget:     s.cfg.RunnerTarget,
		Interactive:      req.Interactive,
		WorkspacePath:    workspacePath,
		AutoStopAfterSec: spec.AutoStopAfterSec,
	}
	created, err := s.cfg.Store.CreateRun(ctx, run)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create run: "+err.Error())
		return
	}

	// Workspace-collision warning (DISCOURAGE, never block): if another non-terminal
	// run already operates on this host directory, two independent agents could
	// interfere. Surface it as an advisory warning on the response + an audit event;
	// the run still launches. Best-effort — a list error never blocks create.
	var warnings []string
	if workspacePath != "" {
		if existing, lerr := s.cfg.Store.ListRuns(ctx); lerr == nil {
			var others []string
			for _, e := range existing {
				if e.ID != runID && e.WorkspacePath == workspacePath && !isTerminalRunState(e.State) {
					others = append(others, e.ID.String())
				}
			}
			if len(others) > 0 {
				warnings = append(warnings, fmt.Sprintf(
					"host workspace %q is already in use by %d active run(s) (%s); independent agents sharing a directory can interfere — proceeding anyway",
					workspacePath, len(others), strings.Join(others, ", ")))
				s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.workspace.collision",
					workspacePath, "success", mustJSON(map[string]any{"other_runs": others})))
			}
		}
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
	if req.ComposeSessionID != "" {
		// UUID-validated at the top of the handler, so it can land in Data as-is.
		createAuditData["compose_session_id"] = req.ComposeSessionID
	}
	if req.TaskMode == "exec" {
		// The run row doesn't store task_mode (request-scoped), so the audit
		// event is the provenance record that this run ran a plain command.
		createAuditData["task_mode"] = req.TaskMode
	}
	s.recordAudit(ctx, s.auditEvent(&runID, createdByType, createdBy, "run.create",
		runID.String(), "success", mustJSON(createAuditData)))

	// Widen the RESOLVED spec's egress from the deterministic operator-trusted
	// sources (onboarded-workspace registries, site-config SCM hosts, the SSH and
	// ADO SCM lanes) — never the LLM; see unionRunEgress. wsRefs feeds the
	// workspace-image resolution below.
	wsRefs := s.unionRunEgress(ctx, runID, &spec, gw)

	// CLIENT-DISCONNECT ISOLATION (M26), same rationale as dispatchWithVerify's own
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
