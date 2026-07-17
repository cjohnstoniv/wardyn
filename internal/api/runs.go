// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/runner"
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
	var req createRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Only agent is hard-required. Repo is OPTIONAL: an inline-policy run that
	// mounts a local host folder (WorkspaceMount target /work) has no git repo to
	// clone, so requiring a repo would block the local-folder wizard path. The
	// clone wiring in dispatch is already nil/empty-safe (it surfaces no repo env
	// when run.Repo is blank), so an empty repo simply runs in the mounted
	// workspace (or an empty one).
	if req.Agent == "" {
		writeError(w, http.StatusBadRequest, "agent is required")
		return
	}

	// BYOI validation (fail closed before any store write): a user-supplied image
	// is mutually exclusive with a devcontainer build, and — unlike DevcontainerRepo,
	// which degrades to the convention image — an explicitly chosen image with no
	// ImageBuilder wired is a hard error (never silently swap a chosen image for the
	// convention one).
	if req.Image != "" {
		if req.DevcontainerRepo != "" {
			writeError(w, http.StatusBadRequest, "image and devcontainer_repo are mutually exclusive")
			return
		}
		if s.cfg.ImageBuilder == nil {
			writeError(w, http.StatusBadRequest,
				"a custom sandbox image was requested but this control plane has no image builder wired "+
					"(start wardynd with -tags docker and set WARDYN_ENVBUILD_TOOLS_DIR / -envbuild)")
			return
		}
	}

	// Validate the requested confinement class up front (fail closed before any
	// store write). Empty inherits the policy minimum; an unknown non-empty
	// value is rejected with 400.
	reqCC, ccOK := parseConfinementClass(req.ConfinementClass)
	if !ccOK {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown confinement_class %q", req.ConfinementClass))
		return
	}

	// task_mode is a tiny closed enum; reject anything else up front (fail
	// closed, same shape as confinement_class above).
	if req.TaskMode != "" && req.TaskMode != "harness" && req.TaskMode != "exec" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown task_mode %q (want harness or exec)", req.TaskMode))
		return
	}

	// Same UUID contract as the compose endpoint's session_id: this field only
	// exists to correlate audit rows, so reject graffiti before anything is
	// created rather than capping arbitrary text into run.create's Data.
	if req.ComposeSessionID != "" {
		if _, err := uuid.Parse(req.ComposeSessionID); err != nil {
			writeError(w, http.StatusBadRequest, "compose_session_id must be a UUID")
			return
		}
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

	// Resolve the run's confinement class: the request value when set, else the
	// policy minimum. A requested class must not be WEAKER than the policy
	// minimum — a run can only request equal-or-stronger confinement, never
	// erode the policy floor (invariant 5, fail closed). The resolved class is
	// what the docker driver gates on (run.ConfinementClass).
	enforced := spec.MinConfinementClass
	if reqCC != "" {
		if !confinementGE(reqCC, spec.MinConfinementClass) {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(
				"confinement_class %s is weaker than the policy minimum %s",
				reqCC, spec.MinConfinementClass))
			return
		}
		enforced = reqCC
	}

	// Deterministic BLAST-RADIUS floor (defense-in-depth — applies to EVERY run,
	// including the manual wizard and direct API callers, not just composed ones):
	// a run holding powerful credentials (write-capable, or a third-party/production
	// api_key) MUST run in the strongest sandbox so a sandbox escape can't carry
	// those credentials out to your host. Raise the enforced class to CC3; a host
	// that cannot provide CC3 then fails closed at the capability check below rather
	// than running the workload under-confined (invariant 5).
	if composer.RequiredConfinementFloor(spec) == types.CC3 && !confinementGE(enforced, types.CC3) {
		enforced = types.CC3
	}

	// Confinement gating: refuse to schedule a run whose confinement class the
	// runner cannot structurally enforce (invariant 5, fail closed).
	if s.cfg.Runner != nil {
		caps, cerr := s.cfg.Runner.Capabilities(ctx)
		if cerr != nil {
			writeError(w, http.StatusServiceUnavailable, "runner capabilities unavailable: "+cerr.Error())
			return
		}
		// Membership, not rank (M8): CC2 (gVisor/runsc) and CC3 (Kata/krun) resolve to
		// INDEPENDENT runtimes, so a host can advertise a non-contiguous set (e.g. a
		// Kata-only host advertises [CC1, CC3], no CC2). A rank check —
		// confinementGE(best, enforced) — would let a CC2 demand pass on that host
		// because CC3 outranks CC2, then fail at sandbox create with a raw docker
		// error. Require the exact enforced class to be advertised. enforced=="" means
		// no class is required (policy floor unset, no request) ⇒ any runner passes.
		if enforced != "" && !slices.Contains(caps.ConfinementClasses, enforced) {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(
				"runner %q cannot enforce confinement_class %s (available: %s)",
				caps.Driver, enforced, classesOrNone(caps.ConfinementClasses)))
			return
		}
	}

	// Reject cloud_sts grants up front: the embedded provider hard-requires
	// SPIRE for them (invariant 5). We check via the identity provider so the
	// spire provider can later accept them without an API change.
	if checker, ok := s.cfg.Identity.(grantChecker); ok {
		if err := checker.CheckGrants(spec.EligibleGrants); err != nil {
			writeError(w, http.StatusUnprocessableEntity,
				"policy requires the spire identity provider: "+err.Error())
			return
		}
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

	// Persist each eligible grant as an eligibility record (NOT issuance).
	// Track the first github_token grant id so dispatch can surface it in the
	// sandbox env as WARDYN_GITHUB_GRANT_ID (non-secret: the grant is an
	// eligibility record, not a token). The run token never appears in env.
	// Auto-mintable api_key grants become proxy injection configs: the proxy
	// resolves their secret VALUES at startup via the internal injection
	// endpoint (values live only in proxy memory, never in the sandbox).
	var firstGitHubGrantID *uuid.UUID
	var injections []runner.InjectionGrant
	// git_pat grants: host -> grant id, surfaced in the sandbox as
	// WARDYN_GIT_PAT_GRANTS so the git-credential helper can mint the stored PAT
	// for a matched non-GitHub host (non-secret: an eligibility record, not the
	// PAT itself; the value is returned only through the brokered mint path).
	gitPATGrants := map[string]string{}
	// gitPATEgress collects the extra hosts a git_pat grant's host needs
	// reachable beyond the grant's own host (currently just ADO's dev.azure.com
	// / *.visualstudio.com bundle — see adoEgressDomains).
	var gitPATEgress []string
	// ssh_key grants: host -> grant id, surfaced in the sandbox as
	// WARDYN_SSH_GRANTS so agent-run can mint the resident private key at clone
	// time (non-secret: an eligibility record, not the key; the key material is
	// returned only through the brokered mint path and wiped after the clone).
	// sshEgress collects the SSH-over-443 endpoints these grants need reachable.
	sshGrants := map[string]string{}
	var sshEgress []string
	for _, g := range spec.EligibleGrants {
		grantID := uuid.New()
		if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
			ID:        grantID,
			RunID:     runID,
			CreatedAt: now,
			Spec:      g,
		}); gerr != nil {
			// A grant write failure is fatal: the run would be ungovernable.
			writeError(w, http.StatusInternalServerError, "create grant: "+gerr.Error())
			return
		}
		if g.Kind == types.GrantGitHubToken && firstGitHubGrantID == nil {
			id := grantID // copy loop var
			firstGitHubGrantID = &id
		}
		if g.Kind == types.GrantGitPAT {
			if host, _, _, derr := gitPATScopeFields(g.Scope); derr == nil {
				gitPATGrants[host] = grantID.String()
				gitPATEgress = append(gitPATEgress, adoEgressDomains(host)...)
			}
		}
		if g.Kind == types.GrantSSHKey {
			// validatePolicySpec already vetted the host is a supported SSH-over-443
			// provider, so sshOver443Endpoint is expected to resolve here.
			if host, _, _, _, derr := sshKeyScopeFields(g.Scope); derr == nil {
				sshGrants[host] = grantID.String()
				if ep, ok := sshOver443Endpoint(host); ok {
					sshEgress = append(sshEgress, ep)
				}
			}
		}
		// Approval-gated api_key grants are deliberately excluded: an unmet
		// approval would fail the proxy's startup mint and brick the sandbox's
		// egress (fail closed, but a footgun as a default).
		if g.Kind == types.GrantAPIKey && !g.RequiresApproval {
			if rule, derr := injectionRuleFromScope(g.Scope); derr == nil {
				injections = append(injections, runner.InjectionGrant{GrantID: grantID, Rule: rule})
			} else {
				s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.create",
					grantID.String(), "failure", mustJSON(map[string]any{
						"error": "api_key grant scope invalid, injection skipped: " + derr.Error(),
					})))
			}
		}
	}

	// codex-cli has no SSH clone lane (no openssh/corkscrew in the image; its
	// agent-run never reads WARDYN_SSH_GRANTS), so an ssh_key grant would sit
	// unconsumed and the clone would fail SILENTLY mid-run. Fail loud at create
	// instead: drop the wiring and tell the operator on the response + audit
	// log. The persisted grant rows stay — they are eligibility records nothing
	// will mint, not issued credentials.
	if req.Agent == "codex-cli" && len(sshGrants) > 0 {
		hosts := make([]string, 0, len(sshGrants))
		for h := range sshGrants {
			hosts = append(hosts, h)
		}
		slices.Sort(hosts)
		warnings = append(warnings, fmt.Sprintf(
			"codex-cli has no SSH clone lane — dropping ssh_key grant(s) for %s; use an HTTPS/PAT source for this repo, or run it under claude-code",
			strings.Join(hosts, ", ")))
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.ssh.unsupported_agent",
			req.Agent, "failure", mustJSON(map[string]any{"dropped_hosts": hosts})))
		sshGrants = map[string]string{}
		sshEgress = nil
	}
	// BYOI images get claude-code's agent-run, whose SSH lane needs openssh +
	// corkscrew in the BASE image — which Wardyn cannot inspect from the control
	// plane. Advise softly; the runtime guard in agent-run still fails loud
	// in-sandbox if the tools are missing.
	if req.Image != "" && len(sshGrants) > 0 {
		warnings = append(warnings,
			"this run clones over SSH: your custom image must carry openssh-client + corkscrew, or the clone is skipped (agent-run warns in the run log)")
	}

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

	// Onboarded-workspace profile drives the run (DETERMINISTIC, never the LLM):
	// union each referenced workspace's detected package registries into the egress
	// allowlist. The operator onboarded + reviewed these workspaces, so their
	// filename-keyed registry hosts are trusted to widen the run's allowlist; the
	// deny-list + confinement floor are unaffected. Image selection from the PRIMARY
	// workspace follows in the image block below.
	wsRefs := s.referencedWorkspaces(ctx, spec)
	if added := unionWorkspaceEgress(&spec, wsRefs); len(added) > 0 {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.workspace.egress",
			runID.String(), "success", mustJSON(map[string]any{"added_domains": added})))
	}

	// Site-config SCM hosts: the operator's declared enterprise SCM hosts (GHES /
	// ADO Server — see unionSiteConfigScmHosts) are reachable by every run, since
	// unlike github.com/dev.azure.com they have no built-in egress bundle.
	if added := s.unionSiteConfigScmHosts(ctx, &spec); len(added) > 0 {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.site_config.egress",
			runID.String(), "success", mustJSON(map[string]any{"added_domains": added})))
	}

	// SSH SCM lane: an ssh_key grant needs its SSH-over-443 endpoint reachable.
	// Add each PORT-QUALIFIED (":443") endpoint to the allowlist so it reuses the
	// CONNECT-443 lane (no port-policy change) and matches ONLY :443 — closing the
	// bare-entry "matches any port" permissiveness for SSH hosts. Deduped against
	// existing entries.
	if added := unionAllowedDomains(&spec, sshEgress); len(added) > 0 {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.ssh.egress",
			runID.String(), "success", mustJSON(map[string]any{"added_domains": added})))
	}

	// ADO SCM lane: a git_pat grant for an Azure DevOps host needs dev.azure.com
	// + *.visualstudio.com reachable (see adoEgressDomains) — unlike GitHub,
	// whose egress is already baked into every example policy, nothing adds
	// these for ADO today. Mirrors the SSH lane above. Deduped against existing
	// entries.
	if added := unionAllowedDomains(&spec, gitPATEgress); len(added) > 0 {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.git_pat.egress",
			runID.String(), "success", mustJSON(map[string]any{"added_domains": added})))
	}

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

	// Resolve the sandbox image. Default: the agent convention image. Precedence:
	// a BYOI wrap (top) > a request-level devcontainer build > the primary onboarded
	// workspace's profile. A BYOI or DEVCONTAINER-REPO build failure marks the run
	// FAILED (observable, never a 500); a WORKSPACE image build failure is fail-open
	// (keeps the convention image), since onboarding is a convenience, not a gate.
	image := agentImage(req.Agent, s.cfg.AgentImages)
	switch {
	case req.Image != "": // validated above: ImageBuilder is non-nil, not XOR'd
		// The wardyn-byoi/ output tag is also the discriminator dispatch keys the
		// runtime selftest preflight off (a wrapped arbitrary image may still lack a
		// shell or the harness binary).
		outTag := "wardyn-byoi/" + runID.String() + ":latest"
		buildCtx, cancelBuild := context.WithTimeout(ctx, imageBuildTimeout)
		built, berr := s.cfg.ImageBuilder.FinalizeBase(buildCtx, req.Image, outTag)
		cancelBuild()
		if berr != nil {
			// CAS from PENDING so a run a concurrent kill already moved to KILLED
			// is not silently clobbered back to FAILED (was: unconditional write).
			s.failAndRevoke(ctx, runID, types.RunPending)
			s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.build",
				runID.String(), "failure", mustJSON(map[string]any{
					"byoi_base": req.Image, "error": berr.Error(),
				})))
			created = s.refreshRun(ctx, runID, created)
			writeJSON(w, http.StatusCreated, createRunResponse{AgentRun: created, Warnings: warnings})
			return
		}
		image = built
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.build",
			runID.String(), "success", mustJSON(map[string]any{
				"byoi_base": req.Image, "image": built,
			})))
	case req.DevcontainerRepo != "" && s.cfg.ImageBuilder != nil:
		outTag := "wardyn-devcontainer/" + runID.String() + ":latest"
		buildCtx, cancelBuild := context.WithTimeout(ctx, imageBuildTimeout)
		built, berr := s.cfg.ImageBuilder.BuildDevcontainer(buildCtx, req.DevcontainerRepo, req.DevcontainerRef, outTag)
		cancelBuild()
		if berr != nil {
			// CAS from PENDING so a run a concurrent kill already moved to KILLED
			// is not silently clobbered back to FAILED (was: unconditional write).
			s.failAndRevoke(ctx, runID, types.RunPending)
			s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.build",
				runID.String(), "failure", mustJSON(map[string]any{
					"devcontainer_repo": req.DevcontainerRepo, "error": berr.Error(),
				})))
			created = s.refreshRun(ctx, runID, created)
			writeJSON(w, http.StatusCreated, createRunResponse{AgentRun: created, Warnings: warnings})
			return
		}
		image = built
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.build",
			runID.String(), "success", mustJSON(map[string]any{
				"devcontainer_repo": req.DevcontainerRepo, "image": built,
			})))
	case len(wsRefs) > 0:
		buildCtx, cancelBuild := context.WithTimeout(ctx, imageBuildTimeout)
		if built, ok := s.resolveWorkspaceImage(buildCtx, runID, wsRefs[0]); ok {
			image = built
		}
		cancelBuild()
	}

	// Persist the resolved image for provenance (best-effort: a failed write
	// must not block dispatch — the audit trail still carries build events).
	if err := s.cfg.Store.SetRunImage(ctx, runID, image); err != nil {
		slog.ErrorContext(ctx, "wardynd: persist run image failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}

	// Dispatch the sandbox if a runner is wired; otherwise stay PENDING.
	if s.cfg.Runner != nil {
		s.dispatch(ctx, created, id.Token, image, spec, firstGitHubGrantID, gitPATGrants, sshGrants, injections, req.Interactive, req.TaskMode)
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
