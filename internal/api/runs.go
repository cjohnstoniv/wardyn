// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// createRunRequest is the POST /api/v1/runs body.
type createRunRequest struct {
	Agent    string     `json:"agent"`
	Repo     string     `json:"repo"`
	Task     string     `json:"task"`
	PolicyID *uuid.UUID `json:"policy_id,omitempty"`
	// DevcontainerRepo, when set AND an ImageBuilder is configured, triggers a
	// devcontainer build (envbuilder) of that git repo. The resulting local
	// image becomes the sandbox image for this run instead of the convention
	// image. Ignored when no ImageBuilder is wired (the request still succeeds
	// with the convention image so the field degrades gracefully). This is an
	// api-local request field; the four nouns in internal/types are untouched.
	DevcontainerRepo string `json:"devcontainer_repo,omitempty"`
	// DevcontainerRef is the optional git ref (branch/tag/sha) to build.
	DevcontainerRef string `json:"devcontainer_ref,omitempty"`
	// ConfinementClass, when set, requests a specific confinement class for the
	// run (types.CC1/CC2/CC3). Empty means inherit the policy minimum (unset).
	// An unknown non-empty value fails closed with HTTP 400. The docker driver
	// gates on run.ConfinementClass; this field threads the request value in.
	ConfinementClass string `json:"confinement_class,omitempty"`
	// Interactive requests an INTERACTIVE run: the sandbox is created and set
	// RUNNING but NO agent task is exec'd (no `claude -p`) and NO completion
	// watcher is started. The container comes up idle (it holds open) so a human
	// can `wardyn attach <id>` and drive it. The Task field is ignored for an
	// interactive run. NOTE: pair this with a policy whose AutoStopAfterSec < 0
	// (never-reap) or the idle reaper will stop the idle sandbox.
	//
	// This is request-controlled (unlike mounts) because it only chooses WHETHER
	// the auto-task runs — it grants no new capability, host path, or egress; the
	// attach itself is still admin-gated and confined (invariant 3).
	Interactive bool `json:"interactive,omitempty"`
	// InlinePolicy, when set, supplies the run's full RunPolicySpec INLINE on the
	// create request instead of referencing a stored policy_id. It is MUTUALLY
	// EXCLUSIVE (XOR) with PolicyID — supplying both is a 400; supplying neither
	// falls back to the configured default (unchanged behavior). An inline policy
	// is authored by an admin / SSO-gated human operator (the create-run surface
	// is admin-gated), NOT by the in-sandbox agent. It is validated with the SAME
	// validatePolicySpec the stored-policy path uses (so runner.ValidateMount
	// gates any inline mount), plus validateInlineSecretRefs (so any inline
	// api_key grant references a secret that actually EXISTS and is not reserved).
	// The resolved spec attaches with a NIL policy id (it is not a stored row).
	InlinePolicy *types.RunPolicySpec `json:"inline_policy,omitempty"`
	// ComposeSessionID, when this run was launched from the AI Run Composer,
	// correlates it back to the compose conversation that produced it (see
	// composer.ComposeRequest.SessionID) — stamped into the run.create audit
	// event's Data below so filtering the audit feed on it reconstructs the
	// whole compose→launch trail (Decision 7: audit-feed-only history, no
	// separate session store/table). Purely a correlation label: it grants
	// nothing and is never validated as a UUID — an arbitrary/absent value only
	// makes the audit label less useful, never wrong or unsafe.
	ComposeSessionID string `json:"compose_session_id,omitempty"`
}

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

	// Validate the requested confinement class up front (fail closed before any
	// store write). Empty inherits the policy minimum; an unknown non-empty
	// value is rejected with 400.
	reqCC, ccOK := parseConfinementClass(req.ConfinementClass)
	if !ccOK {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown confinement_class %q", req.ConfinementClass))
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
	spec, policyID, ok := s.resolveRunPolicy(ctx, w, r, &req)
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

	createAuditData := map[string]any{
		"agent": req.Agent, "repo": req.Repo, "policy_id": policyID,
		"confinement_class": enforced, "jti": id.JTI,
		"inline_policy": req.InlinePolicy != nil,
	}
	if req.ComposeSessionID != "" {
		// UUID-validated at the top of the handler, so it can land in Data as-is.
		createAuditData["compose_session_id"] = req.ComposeSessionID
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

	// Resolve the sandbox image. Default: the agent convention image. A request-
	// level devcontainer build wins; else the primary onboarded workspace's profile
	// selects/builds the image. A DEVCONTAINER-REPO build failure marks the run
	// FAILED (observable, never a 500); a WORKSPACE image build failure is fail-open
	// (keeps the convention image), since onboarding is a convenience, not a gate.
	image := agentImage(req.Agent, s.cfg.AgentImages)
	if req.DevcontainerRepo != "" && s.cfg.ImageBuilder != nil {
		outTag := "wardyn-devcontainer/" + runID.String() + ":latest"
		built, berr := s.cfg.ImageBuilder.BuildDevcontainer(ctx, req.DevcontainerRepo, req.DevcontainerRef, outTag)
		if berr != nil {
			_ = s.cfg.Store.UpdateRunState(ctx, runID, types.RunFailed)
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
	} else if len(wsRefs) > 0 {
		if built, ok := s.resolveWorkspaceImage(ctx, runID, wsRefs[0]); ok {
			image = built
		}
	}

	// Dispatch the sandbox if a runner is wired; otherwise stay PENDING.
	if s.cfg.Runner != nil {
		s.dispatch(ctx, created, id.Token, image, spec, firstGitHubGrantID, gitPATGrants, sshGrants, injections, req.Interactive)
		// Re-read so the response reflects the post-dispatch state.
		created = s.refreshRun(ctx, runID, created)
	}

	writeJSON(w, http.StatusCreated, createRunResponse{AgentRun: created, Warnings: warnings})
}

// dispatch launches the sandbox via the runner and advances run state. On any
// failure it marks the run FAILED and audits — but never returns the failure to
// the create caller (the run row exists and is queryable). The run token is
// passed to the proxy sidecar via ProxyConfig (verifiable, not a usable secret).
// image is the resolved sandbox OCI image (convention image or a devcontainer
// build result). firstGitHubGrantID, when non-nil, is surfaced in sandbox env
// as WARDYN_GITHUB_GRANT_ID so the git-credential helper can request the token
// via the proxy's local mint route without holding the run token directly.
//
// After Exec starts the agent, dispatch launches a DETACHED completion watcher
// goroutine (see startCompletionWatcher): it blocks on Runner.Wait(ref) and,
// when the agent process exits, transitions the run to COMPLETED (exit 0) or
// FAILED (non-zero) and tears the sandbox down — but only if the run is still
// RUNNING, so a concurrent kill/stop is never clobbered.
//
// INTERACTIVE MODE: when interactive is true, dispatch does CreateSandbox + set
// RUNNING but SKIPS the agent Exec entirely (no `claude -p`) and does NOT start
// the completion watcher (there is no agent process to wait on — the watcher
// would otherwise mark the idle run COMPLETED the moment Wait failed). The
// sandbox comes up idle (the container holds open via `sleep infinity`) so a
// human can `wardyn attach <id>` and drive it. A non-interactive run is
// unchanged. Pair an interactive run with a never-reap policy (AutoStopAfterSec
// < 0) or the idle reaper will stop the idle sandbox.
func (s *Server) dispatch(ctx context.Context, run types.AgentRun, runToken, image string, policy types.RunPolicySpec, firstGitHubGrantID *uuid.UUID, gitPATGrants, sshGrants map[string]string, injections []runner.InjectionGrant, interactive bool) {
	s.dispatchWithVerify(ctx, run, runToken, image, policy, firstGitHubGrantID, gitPATGrants, sshGrants, injections, interactive, nil)
}

// dispatchWithVerify is dispatch plus an optional verify-plan (JSON
// []workspacescan.SetupCommand). When present, the run is a VERIFY run: it execs
// wardyn-verify (in the built devcontainer image) instead of the scanner or the
// agent, and the commands ride WARDYN_VERIFY_COMMANDS. A verify run still sets
// WorkspaceID (for the trusted result linkage), so verifyPlan is the
// discriminator between scan-only and verify-only in the same dispatch.
func (s *Server) dispatchWithVerify(ctx context.Context, run types.AgentRun, runToken, image string, policy types.RunPolicySpec, firstGitHubGrantID *uuid.UUID, gitPATGrants, sshGrants map[string]string, injections []runner.InjectionGrant, interactive bool, verifyPlan json.RawMessage) {
	// Client-disconnect isolation (M26): dispatch is invoked synchronously from the
	// create-run handler, so a client disconnect cancels ctx mid-flight — which would
	// also fail the compensating StopSandbox below on the same dead ctx and orphan a
	// live sandbox. Detach from cancellation (values preserved) so the whole
	// provision → CAS → compensate sequence always completes. The completion watcher
	// already runs on BaseCtx, not ctx.
	ctx = context.WithoutCancel(ctx)

	// KILL-RACE GUARD (entry): claim PENDING->STARTING conditionally. A
	// POST /runs/{id}/kill landing in the pre-dispatch window (grant writes, the
	// ListRuns scan, a minutes-long devcontainer build) CASes PENDING->KILLED and
	// tears down identity/broker. A blind ->STARTING write here would RESURRECT that
	// killed run: the later STARTING->RUNNING CAS would then apply and the run would
	// boot and execute despite the 202 kill. So if the claim does not apply, the run
	// is no longer PENDING (killed/stopped) — abort without dispatching. Every
	// dispatch caller passes a freshly-created PENDING run.
	claimed, cerr := s.cfg.Store.UpdateRunStateIf(ctx, run.ID, types.RunPending, types.RunStarting)
	if cerr != nil || !claimed {
		data := map[string]any{"note": "run left PENDING by a concurrent kill/stop before dispatch; dispatch aborted"}
		if cerr != nil {
			data["error"] = cerr.Error()
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch",
			run.ID.String(), "failure", mustJSON(data)))
		return
	}

	// CC3 host-eBPF blindness, surfaced AUTOMATICALLY. The host Tetragon sensor
	// cannot see inside a Kata microVM guest, so a CC3 run is blind to the
	// ground-truth stream. wardynd knows the resolved confinement class here, so
	// it records the one-time kernel.sensor.blind audit event itself — making the
	// gap VISIBLE regardless of whether the operator set the sidecar env var
	// WARDYN_GROUNDTRUTH_BLIND_RUNS (that path is kept too). Matches the data
	// shape the sidecar emits (reason="cc3-kata-host-ebpf-blind", run_id) so the
	// downstream audit/correlation is identical.
	if run.ConfinementClass == types.CC3 {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "kernel.sensor.blind",
			run.ID.String(), "success", mustJSON(map[string]any{
				"reason": "cc3-kata-host-ebpf-blind", "run_id": run.ID.String(),
			})))
	}

	// Sandbox env: non-secret values only (invariant 1). The run token never
	// appears here — the proxy holds it via ProxyConfig.RunToken and injects it
	// when forwarding internal API calls from inside the sandbox.
	// Per-run proxy sidecar (docker hostname) unless the config overrides it.
	proxyURL := cmp.Or(s.cfg.ProxyURL, "http://wardyn-proxy:3128")
	sandboxEnv := map[string]string{
		"WARDYN_RUN_ID":    run.ID.String(),
		"WARDYN_PROXY_URL": proxyURL,
		// Standard proxy env: agents using HTTP_PROXY-aware clients route
		// through the wardyn-proxy automatically (L2 enforcement).
		"HTTP_PROXY":  proxyURL,
		"HTTPS_PROXY": proxyURL,
		// Exclude the proxy itself and loopback from proxy traversal.
		"NO_PROXY": "wardyn-proxy,localhost,127.0.0.1,::1",
		// Toolchain-fidelity env (PLATFORM-wide, every image — not just the fat
		// campaign image). These were live-found in the cross-language campaign:
		//   GOTMPDIR: the sandbox mounts /tmp NOEXEC, but `go test` compiles+EXECS
		//     its test binaries in $TMPDIR → "permission denied". Point it at the
		//     agent's exec-allowed HOME. (Plain env survives a shell; only PATH is
		//     reset by a login shell — which wardyn-verify no longer uses.)
		//   MAVEN_OPTS: Maven ALONE ignores HTTP(S)_PROXY (npm/pip/cargo/go/git
		//     honor it) → "Unknown host repo.maven.apache.org". The JVM proxy
		//     sysprops route Maven through wardyn-proxy. (The fat image also bakes
		//     a settings.xml <proxy> as belt-and-braces.)
		//   GRADLE_OPTS: Gradle is the same JVM-networking case as Maven — it
		//     resolves dependencies via java.net's proxy selector, which reads the
		//     standard -Dhttp(s).proxyHost/-port/-DnonProxyHosts sysprops (Gradle's
		//     own docs point at these same properties, normally set in
		//     gradle.properties — GRADLE_OPTS is the env-expressible equivalent, so
		//     it's the exact same JVM opts string as MAVEN_OPTS). Reused verbatim.
		//   NOT covered here (need image/build-time FILE config, not env, so out of
		//     scope for this platform-env pass): apt (/etc/apt/apt.conf.d/*.conf)
		//     and a per-project gradle.properties/init.d script for repos that
		//     don't launch via the gradle/gradlew wrapper JVM. npm/pip/cargo/go/git
		//     already honor HTTP(S)_PROXY above and need nothing further.
		"GOTMPDIR":    "/home/agent/.gotmp",
		"GOCACHE":     "/home/agent/.cache/go-build",
		"MAVEN_OPTS":  mavenProxyOpts(proxyURL),
		"GRADLE_OPTS": mavenProxyOpts(proxyURL),
		// Git commit attribution: carry the sub/act delegation chain into the commit
		// graph so an agent's commits are traceable to the governed run — AUTHOR is
		// the human who authorized the run (sub), COMMITTER is the agent run (act).
		// git reads these env vars without touching the image. (Deterministic
		// Run-Id/On-Behalf-Of commit trailers need an in-image prepare-commit-msg
		// hook — tracked as a follow-up.)
		"GIT_AUTHOR_NAME":     run.CreatedBy,
		"GIT_AUTHOR_EMAIL":    gitEmailLocal(run.CreatedBy) + "@wardyn.local",
		"GIT_COMMITTER_NAME":  "wardyn-agent:" + run.Agent,
		"GIT_COMMITTER_EMAIL": run.ID.String() + "@agent.wardyn.local",
	}
	// Governed repo SCAN run: after cloning, the entrypoint runs wardyn-scan (which
	// walks ~/work and PUTs ScanFacts to the brokered scan-results route) INSTEAD of
	// the agent. A non-nil WorkspaceID uniquely marks a scan run (ordinary runs never
	// set it); no agent CLI / model call happens.
	// A verify run (verifyPlan present) execs wardyn-verify in the built image;
	// a scan run (WorkspaceID set, no verify plan) execs wardyn-scan. verifyPlan
	// is the discriminator since both set WorkspaceID for the trusted upload
	// linkage. The approved setup commands are non-secret operator-authored
	// values (secrets are proxy-injected, never resident), so they ride env.
	if len(verifyPlan) > 0 {
		sandboxEnv["WARDYN_VERIFY_ONLY"] = "1"
		sandboxEnv["WARDYN_VERIFY_COMMANDS"] = string(verifyPlan)
	} else if run.WorkspaceID != nil && !interactive {
		// An INTERACTIVE workspace-linked run (interactive Record Mode) is a
		// human-driven sandbox, not a scan — never mark it WARDYN_SCAN_ONLY.
		sandboxEnv["WARDYN_SCAN_ONLY"] = "1"
	}
	if firstGitHubGrantID != nil {
		sandboxEnv["WARDYN_GITHUB_GRANT_ID"] = firstGitHubGrantID.String()
	}
	// git_pat grants: surface the {host: grant_id} map so the git-credential
	// helper can mint the stored PAT for a matched non-GitHub host. Non-secret
	// (grant ids, not the PAT); the value is returned only via the brokered mint.
	if len(gitPATGrants) > 0 {
		if b, merr := json.Marshal(gitPATGrants); merr == nil {
			sandboxEnv["WARDYN_GIT_PAT_GRANTS"] = string(b)
		}
	}
	// ssh_key grants: surface the {host: grant_id} map so agent-run can mint the
	// resident SSH private key at clone time (SSH has NO credential-helper seam,
	// so the key is written to a 0400 file and wiped after the clone). Non-secret
	// (grant ids, not the key); the key material is returned only via the brokered
	// mint and never touches env. See GrantSSHKey.
	if len(sshGrants) > 0 {
		if b, merr := json.Marshal(sshGrants); merr == nil {
			sandboxEnv["WARDYN_SSH_GRANTS"] = string(b)
		}
	}

	// Repo cloning wiring (non-secret; invariant 1 preserved). The agent-run
	// launcher shallow-clones each repo into the workspace through the governed
	// egress path (wardyn-proxy) before running the agent. Sources: the legacy
	// single run.Repo (backward-compat) PLUS each onboarded WorkspaceRepo on the
	// resolved policy (already gated by the onboarding check + validatePolicySpec).
	// Every field is repoFieldSafe (no whitespace/control chars), so the tab/newline
	// framing of WARDYN_REPOS is injection-safe; every dest is a validated
	// allowed-prefix target and unique. The legacy WARDYN_REPO_URL/SLUG are still
	// emitted for the first entry so a one-release rollout never breaks.
	if slug := strings.TrimSpace(run.Repo); slug != "" && repoFieldSafe(slug) {
		sandboxEnv["WARDYN_REPO_SLUG"] = slug
		if url := repoCloneURL(slug); url != "" {
			sandboxEnv["WARDYN_REPO_URL"] = url
		}
	}
	if repos := buildRepoRecords(run.Repo, policy.WorkspaceRepos); repos != "" {
		sandboxEnv["WARDYN_REPOS"] = repos
	}

	// Artifact Repository Redirection (operator-wide site-config): for each
	// configured ecosystem, SUBSTITUTE the corp mirror host for the language's
	// public-registry hosts in this run's egress, deliver the per-tool config
	// (URL-only) into the sandbox, and — for a redirect WITH a token secret —
	// author a proxy-side injection so the token is added on the wire (the sandbox
	// never holds it). No-op when no override is configured. Read once here (the
	// one composition layer every run — agent/verify/record/scan — funnels through).
	// Read the operator-wide site-config ONCE per dispatch. Both consumers below
	// (artifact redirection here + the upstream/corp proxy near ProxyConfig) share
	// this snapshot, so a concurrent admin PUT /api/v1/site-config can never compose
	// a single run from two different snapshots (e.g. new SCM hosts with stale
	// artifact overrides). Store is guaranteed non-nil in dispatch (UpdateRunState
	// is called unconditionally below).
	siteCfg, siteCfgErr := s.cfg.Store.GetSiteConfig(ctx)

	var artifactPlan artifactRedirectPlan
	if siteCfgErr == nil {
		policy.AllowedDomains = substituteArtifactEgress(policy.AllowedDomains, siteCfg)
		artifactPlan = s.planArtifactRedirect(ctx, run, siteCfg)
		for k, v := range artifactPlan.env {
			sandboxEnv[k] = v
		}
		if artifactPlan.configB64 != "" {
			sandboxEnv["WARDYN_ARTIFACT_CONFIG_B64"] = artifactPlan.configB64
		}
	}
	artifactInject := len(artifactPlan.injections) > 0

	// Anthropic auth mode — set on the SANDBOX ENV (not just in agent-run). An
	// INTERACTIVE run never invokes agent-run (the human runs `claude` in the
	// attach shell), so the auth env must live on the container itself or the
	// manual claude session inherits the image's inject-gateway default and is
	// denied. Detect SUBSCRIPTION by the resident ~/.claude bind mount: claude
	// then talks DIRECTLY to api.anthropic.com with its own OAuth creds over the
	// HTTPS_PROXY tunnel, bypassing /wardyn/llm/anthropic (which would deny a run
	// that has no api_key grant to inject). Otherwise (API-key mode) keep the
	// image's gateway default and seed a NON-SECRET placeholder so claude emits a
	// request the proxy strips + re-injects (invariant 1: the real key is never
	// resident; the placeholder is a sentinel, not a credential).
	subscription := specHasMountTarget(&policy, claudeCredTarget)
	// Subscription runs: inject the operator's LIVE OAuth token PROXY-SIDE (the
	// sandbox holds only an inert sentinel) instead of the resident copy, which
	// goes stale — the access token expires (~hours) and the refresh token ROTATES
	// as the operator's own host `claude` refreshes, locking the copy out. This
	// REQUIRES TLS-MITM of api.anthropic.com so the proxy can swap the credential;
	// it is the safe default whenever a token provider is wired. Escape hatch:
	// WARDYN_SUBSCRIPTION_INJECT=off keeps the legacy resident-copy behavior.
	injectSub := subscription && s.cfg.SubscriptionToken != nil && !s.cfg.DisableSubscriptionInject

	// Bedrock: a third Anthropic transport, mutually exclusive with subscription
	// (checked first) and api-key mode (the fallback). See resolveBedrockAuth for
	// the readiness rule and the resident-AWS-cred rationale.
	//
	// modelRun gates Bedrock on a run that actually invokes the model: a verify run
	// (execs wardyn-verify — verifyPlan present) or a scan run (execs wardyn-scan —
	// WorkspaceID set, non-interactive) makes no model call, so it must NOT receive
	// the resident AWS SigV4 creds (least privilege — the creds are masked + confined
	// regardless, but there's no reason to place them in a sandbox that never signs a
	// Bedrock request). Mirrors the WARDYN_VERIFY_ONLY / WARDYN_SCAN_ONLY discriminator.
	modelRun := len(verifyPlan) == 0 && !(run.WorkspaceID != nil && !interactive)
	bedrock := s.resolveBedrockAuth(ctx, run.Agent, subscription, modelRun)
	bedrockReady := bedrock.ready
	// injectBedrockBearer wires bedrock-runtime for proxy-side bearer injection
	// (never-resident); set in the bedrock handling block below and consumed by the
	// CA / injection / MITM-host wiring alongside the subscription path.
	injectBedrockBearer := bedrockReady && bedrock.bearer

	if subscription {
		sandboxEnv["ANTHROPIC_BASE_URL"] = "https://api.anthropic.com"
		// The subscription creds are bind-mounted READ-ONLY at ~/.claude, but
		// claude-code needs a WRITABLE config dir (session-env/, history) — it fails
		// EROFS trying to mkdir under a read-only ~/.claude. Point CLAUDE_CONFIG_DIR at
		// a writable path that agent-run populates from the read-only mount (creds +
		// ~/.claude.json). Set on the sandbox env so BOTH agent-run and an interactive
		// `wardyn attach` shell inherit it.
		sandboxEnv["CLAUDE_CONFIG_DIR"] = "/home/agent/.claude-run"
	} else if bedrockReady {
		for k, v := range bedrock.env {
			sandboxEnv[k] = v
		}
		// Resident SigV4 creds must stay out of PTY/recording streams and any
		// `agent-run --selftest` echo. Bearer mode holds only a placeholder and the
		// ~/.aws-mount mode holds no keys in env at all (the SDK reads the mount), so
		// neither has anything secret to mask here.
		if s.cfg.MaskRegistry != nil && !bedrock.bearer && !bedrock.awsMount {
			s.cfg.MaskRegistry.Add(run.ID, []byte(bedrock.env["AWS_ACCESS_KEY_ID"]))
			s.cfg.MaskRegistry.Add(run.ID, []byte(bedrock.env["AWS_SECRET_ACCESS_KEY"]))
			if tok := bedrock.env["AWS_SESSION_TOKEN"]; tok != "" {
				s.cfg.MaskRegistry.Add(run.ID, []byte(tok))
			}
		}
		for _, h := range bedrock.egressHosts {
			if !domainAllowedExact(policy.AllowedDomains, h) {
				policy.AllowedDomains = append(policy.AllowedDomains, h)
			}
		}
		detail := "resident AWS SigV4 credentials in sandbox env (SigV4 request signing can't be proxy-injected like a static api key); IAM least-privilege scoping is the operator's responsibility"
		mode := "resident"
		switch {
		case bedrock.bearer:
			detail = "bearer token injected proxy-side into bedrock-runtime (TLS-MITM); sandbox holds only a placeholder — never resident"
			mode = "bearer"
		case bedrock.awsMount:
			detail = "host ~/.aws bind-mounted read-only; the AWS SDK resolves credentials (incl. auto-refreshing SSO) from the mount — no static keys stored, none resident in env"
			mode = "aws-dir-mount"
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.llm.bedrock",
			run.ID.String(), "success", mustJSON(map[string]any{
				"region": s.cfg.BedrockRegion, "model": s.cfg.BedrockModel, "hosts": bedrock.egressHosts,
				"mode": mode, "detail": detail,
			})))
	} else {
		sandboxEnv["ANTHROPIC_API_KEY"] = "wardyn-proxy-injected"
	}
	// Operator model pin: force a specific Anthropic model (e.g. "opus") so the
	// agent doesn't fall back to the account/CLI default (a promo can push that to
	// a cheaper model like Fable). Off unless configured; Claude agent only; never
	// overrides the Bedrock model id (that IS the pin, in inference-profile form).
	if s.cfg.AgentAnthropicModel != "" && run.Agent == "claude-code" && !bedrockReady {
		sandboxEnv["ANTHROPIC_MODEL"] = s.cfg.AgentAnthropicModel
	}

	// Codex (OpenAI) reverse-proxy route: point the OpenAI SDK at the proxy's
	// inspectable /wardyn/llm/openai gateway with a non-secret placeholder; the
	// proxy strips it and injects the brokered OpenAI key (mirrors Anthropic
	// api-key mode). A subscription Codex reaching api.openai.com directly is
	// covered by TLS-MITM when intercept_tls is enabled.
	if run.Agent == "codex-cli" && !subscription {
		sandboxEnv["OPENAI_BASE_URL"] = proxyURL + "/wardyn/llm/openai"
		sandboxEnv["OPENAI_API_KEY"] = "wardyn-proxy-injected"
	}

	// Optional TLS-MITM of opaque LLM CONNECT tunnels (intercept_tls): provision a
	// per-run CA. The PRIVATE key reaches ONLY the proxy sidecar (ProxyConfig,
	// below); the sandbox trusts the PUBLIC cert, installed by agent-run from
	// WARDYN_MITM_CA_PEM and pointed at via NODE_EXTRA_CA_CERTS (additive for Node
	// clients like Claude Code). This makes the subscription-OAuth path inspectable.
	// MITM is provisioned for content inspection (intercept_tls) AND, now, for
	// subscription credential injection (injectSub) — the proxy must terminate the
	// TLS to api.anthropic.com to swap in the live token.
	mitmForInspect := false
	if li := policy.LLMInspection; li != nil && li.InterceptTLS && li.Mode != "" && !strings.EqualFold(li.Mode, "off") {
		mitmForInspect = true
	}
	var mitmCACertPEM, mitmCAKeyPEM string
	if injectSub || mitmForInspect || artifactInject || injectBedrockBearer {
		certPEM, keyPEM, caErr := generateRunCA(time.Now())
		if caErr != nil {
			_ = s.cfg.Store.UpdateRunState(ctx, run.ID, types.RunFailed)
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
				run.ID.String(), "failure", mustJSON(map[string]any{"error": "mitm ca: " + caErr.Error()})))
			return
		}
		mitmCACertPEM, mitmCAKeyPEM = string(certPEM), string(keyPEM)
		sandboxEnv["WARDYN_MITM_CA_PEM"] = mitmCACertPEM
		sandboxEnv["NODE_EXTRA_CA_CERTS"] = "/home/agent/.wardyn/mitm-ca.pem"
	}

	// Subscription: author a re-mintable api_key grant whose SENTINEL secret name
	// resolves to the operator's LIVE OAuth token (host-refreshed) rather than a
	// stored secret; append its injection and ensure the exact host is egress-
	// allowed (the injector's hard requirement). Non-approval api_key grants are
	// re-mintable by design, so the proxy re-resolves the token before expiry
	// indefinitely. This is what makes the sandbox's sentinel sufficient.
	if injectSub {
		const anthropicAPIHost = "api.anthropic.com"
		// Subscription REPLACES any api-key injection for the same host. A ceiling
		// that also lists an anthropic-api-key grant (e.g. the composer-dev ceiling)
		// would otherwise leave TWO injections for api.anthropic.com; the proxy
		// resolves both at startup and the api-key mint fails closed when its secret
		// is absent — crashing the sidecar. Drop it here (the direct-run equivalent
		// of reconcileLLMAccess's removeAPIKeyGrantForHost on the composer path).
		kept := injections[:0]
		for _, ig := range injections {
			if strings.EqualFold(strings.TrimSuffix(ig.Rule.Host, "."), anthropicAPIHost) {
				continue
			}
			kept = append(kept, ig)
		}
		injections = kept
		subGrantID := uuid.New()
		subScope, _ := json.Marshal(map[string]string{
			"host":        anthropicAPIHost,
			"header":      "Authorization",
			"format":      "Bearer %s",
			"secret_name": subscriptionOAuthSecret,
		})
		if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
			ID: subGrantID, RunID: run.ID, CreatedAt: time.Now(),
			Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: subScope, TTLSeconds: 3600},
		}); gerr != nil {
			_ = s.cfg.Store.UpdateRunState(ctx, run.ID, types.RunFailed)
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
				run.ID.String(), "failure", mustJSON(map[string]any{"error": "subscription inject grant: " + gerr.Error()})))
			return
		}
		if rule, derr := injectionRuleFromScope(subScope); derr == nil {
			injections = append(injections, runner.InjectionGrant{GrantID: subGrantID, Rule: rule})
		}
		if !domainAllowedExact(policy.AllowedDomains, anthropicAPIHost) {
			policy.AllowedDomains = append(policy.AllowedDomains, anthropicAPIHost)
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.llm.subscription_inject",
			run.ID.String(), "success", mustJSON(map[string]any{
				"host": anthropicAPIHost, "tls_mitm": true,
				"detail": "live subscription OAuth token injected proxy-side; sandbox holds an inert sentinel",
			})))
	}

	// Bedrock BEARER injection: author an api_key grant whose Authorization: Bearer
	// header injects the operator's Bedrock API key into bedrock-runtime, and mark
	// that host TLS-MITM-eligible for THIS run. This is the same operator-configured
	// MITM-host + paired-injection pattern as corp artifact hosts (isCorpMITMHost) —
	// bedrock-runtime is not a wildcard, the token is the operator's own, and the CA
	// key stays in proxy memory. The sandbox holds only the placeholder bearer.
	var bedrockMITMHosts []string
	if injectBedrockBearer {
		bedrockMITMHosts = append(bedrockMITMHosts, bedrock.runtimeHost)
		beScope, _ := json.Marshal(map[string]string{
			"host":        bedrock.runtimeHost,
			"header":      "Authorization",
			"format":      "Bearer %s",
			"secret_name": bedrockAPIKeySecret,
		})
		beGrantID := uuid.New()
		if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
			ID: beGrantID, RunID: run.ID, CreatedAt: time.Now(),
			Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: beScope, TTLSeconds: 3600},
		}); gerr != nil {
			_ = s.cfg.Store.UpdateRunState(ctx, run.ID, types.RunFailed)
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
				run.ID.String(), "failure", mustJSON(map[string]any{"error": "bedrock bearer inject grant: " + gerr.Error()})))
			return
		}
		if rule, derr := injectionRuleFromScope(beScope); derr == nil {
			injections = append(injections, runner.InjectionGrant{GrantID: beGrantID, Rule: rule})
		}
	}

	// Artifact-redirect token injections (authored in planArtifactRedirect, whose
	// egress substitution already added each corp host to policy.AllowedDomains, so
	// the injector's exact-allowlist check passes). Appended AFTER the subscription
	// block, which reslices `injections` in place.
	injections = append(injections, artifactPlan.injections...)

	// Fail CLOSED at schedule time when inspection is REQUIRED but the resolved LLM
	// transport is OPAQUE (M24). Opaque transports: (a) a subscription/OAuth transport
	// that is NOT being MITM'd (injectSub / intercept_tls auto-enable MITM, making it
	// inspectable); (b) SigV4 Bedrock via ~/.aws or resident keys — uninspectable by
	// construction (we cannot re-sign a MITM'd SigV4 request), so only the Bedrock
	// BEARER path (proxy-injected, MITM'd) is inspectable. Previously only the
	// subscription case failed closed, silently exempting opaque Bedrock. The default
	// (require_inspectable_llm=false) instead degrades visibly rather than failing.
	if li := policy.LLMInspection; li != nil && li.RequireInspectableLLM &&
		li.Mode != "" && !strings.EqualFold(li.Mode, "off") &&
		((subscription && !li.InterceptTLS && !injectSub) || (bedrockReady && !bedrock.bearer)) {
		_ = s.cfg.Store.UpdateRunState(ctx, run.ID, types.RunFailed)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": "require_inspectable_llm: the resolved LLM transport is opaque (subscription without MITM, or SigV4 Bedrock); enable intercept_tls or use an inspectable transport"})))
		return
	}

	// Host bind mounts: copy the resolved POLICY's WorkspaceMounts into the spec.
	// Mounts may be authored on a stored policy (admin-gated policy CRUD) OR
	// INLINE on the create request by an admin / SSO-gated human operator
	// (createRunRequest.InlinePolicy) — both flow through the SAME resolved
	// RunPolicySpec here, so this is still the only path that populates
	// spec.Mounts. They are NEVER chosen by the in-sandbox agent: the agent-run
	// entrypoint has no access to this surface, so a prompt-injected agent can
	// never pick a host mount (invariants 1 & 3). Every mount was already
	// deny-list-validated by runner.ValidateMount at policy-write/inline-validate
	// time (validatePolicySpec); the docker driver re-validates it
	// defense-in-depth at sandbox-create time. runner.ValidateMount is unchanged.
	var mounts []runner.Mount
	for _, wm := range policy.WorkspaceMounts {
		mounts = append(mounts, runner.Mount{
			Source: wm.Source,
			Target: wm.Target,
			// Safe default: omitted read_only => read-only. RW only on explicit
			// read_only=false in the policy.
			ReadOnly: wm.ReadOnlyOrDefault(),
		})
	}

	// Host-mode Bedrock ~/.aws mount (operator config, not agent-chosen; same trust
	// and the same driver deny-list re-validation as the WorkspaceMounts above).
	// READ-ONLY: the sandbox reads the SSO cache / config but can never write to the
	// operator's host AWS state. Only set on the host-mode path (resolveBedrockAuth
	// gated it on BedrockAWSConfigDir existing); never present for a team deployment.
	if bedrockReady && bedrock.awsMount {
		mounts = append(mounts, runner.Mount{
			Source:   bedrock.awsMountSource,
			Target:   sandboxAWSDir,
			ReadOnly: true,
		})
	}

	// Operator-wide upstream/corp proxy (site-config → ProxyConfig.UpstreamProxyURL).
	// A locked-down corporate network may give the sandbox host NO direct internet
	// route at all — site_config.UpstreamProxySecretRef (admin-authored via
	// PUT /api/v1/site-config) names the secret holding the corp CONNECT-proxy URL.
	// The resolved cred-bearing URL lands in the sidecar's WARDYN_PROXY_CONFIG_JSON
	// env var, the SAME posture as RunToken today: proxy-process-only, never on the
	// sandbox side, masked from decision-log/stdout by the proxy — a deliberate,
	// already-documented tradeoff (see runner.ProxyConfig.UpstreamProxyURL), not a
	// new one. Fail SAFE: an unconfigured ref, an unresolvable secret, or a
	// non-http URL all leave this "" (direct egress, today's behavior) plus an
	// audit event; none of them fail the run or crash dispatch.
	upstreamProxyURL := ""
	if siteCfgErr != nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.upstream_proxy.resolve",
			run.ID.String(), "failure", mustJSON(map[string]any{"reason": "site-config-read-error"})))
	} else if siteCfg.UpstreamProxySecretRef != "" {
		var getSecret func(context.Context, string) ([]byte, error)
		if s.cfg.Secrets != nil {
			getSecret = s.cfg.Secrets.Get
		}
		resolved, failReason := resolveUpstreamProxyURL(ctx, siteCfg.UpstreamProxySecretRef, getSecret)
		if failReason != "" {
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.upstream_proxy.resolve",
				run.ID.String(), "failure", mustJSON(map[string]any{
					"reason": failReason, "secret_ref": siteCfg.UpstreamProxySecretRef,
				})))
		} else {
			upstreamProxyURL = resolved
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.upstream_proxy.resolve",
				run.ID.String(), "success", mustJSON(map[string]any{"secret_ref": siteCfg.UpstreamProxySecretRef})))
		}
	}

	spec := runner.SandboxSpec{
		RunID:            run.ID,
		Image:            image,
		ConfinementClass: run.ConfinementClass,
		Env:              sandboxEnv,
		Mounts:           mounts,
		// Interactive runs come up idle for `wardyn attach`; the driver prepares the
		// workspace (clones the repo into ~/work) on the idle process so the attach
		// shell isn't empty. A non-interactive run's task exec does this itself.
		Interactive: interactive,
		ProxyConfig: runner.ProxyConfig{
			RunToken:        runToken,
			ControlPlaneURL: s.cfg.ControlPlaneURL,
			// The proxy sidecar enforces THIS run's egress policy; a proxy
			// without a policy fails closed (no egress at all).
			Policy:    policy,
			Injection: injections,
			// Per-run TLS-MITM CA (empty unless intercept_tls): private key to the
			// proxy only; the sandbox trusts the public cert via the agent env.
			MITMCACertPEM: mitmCACertPEM,
			MITMCAKeyPEM:  mitmCAKeyPEM,
			// Operator-configured corp artifact hosts the proxy is allowed to
			// TLS-MITM (beyond the built-in LLM hosts) so a registry token injects on
			// the wire. Only hosts with a resolved token injection appear here — a
			// tight per-host allowlist, never a blanket. See isMITMHost widening.
			MITMHosts: append(append([]string{}, artifactPlan.mitmHosts...), bedrockMITMHosts...),
			// MITM the BUILT-IN LLM hosts only when that's actually intended for this
			// run — subscription OAuth injection or intercept_tls content inspection.
			// The CA above may also be minted purely for artifact-token injection, so
			// this keeps an artifact-only run from TLS-terminating a direct CONNECT to
			// Anthropic/OpenAI it never asked to intercept.
			MITMLLM: injectSub || mitmForInspect,
			// Resolved above from site-config.UpstreamProxySecretRef; "" when
			// unconfigured or unresolvable (direct dial, backward-compatible).
			UpstreamProxyURL: upstreamProxyURL,
		},
		// Hard resource caps. A nil policy block (or a zero field) becomes the
		// driver's conservative platform default, so EVERY sandbox is CPU/memory/
		// PID capped even when the policy sets nothing — a fleet of independent
		// agents must not be able to OOM-kill, fork-bomb, or disk-fill the host or
		// each other (C5).
		Resources: resourceLimitsToRunner(policy.Resources),
		Labels: map[string]string{
			"wardyn.run":   run.ID.String(),
			"wardyn.agent": run.Agent,
		},
	}

	sb, err := s.cfg.Runner.CreateSandbox(ctx, spec)
	if err != nil {
		// Conditional: only mark FAILED if still STARTING. A kill landing between the
		// entry claim and this failure moved the run to KILLED — don't clobber that
		// terminal state (mirrors the STARTING->RUNNING guard below).
		_, _ = s.cfg.Store.UpdateRunStateIf(ctx, run.ID, types.RunStarting, types.RunFailed)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": err.Error()})))
		return
	}
	_ = s.cfg.Store.SetSandboxRef(ctx, run.ID, sb.Ref)

	// KILL-RACE GUARD: advance STARTING->RUNNING CONDITIONALLY. CreateSandbox can
	// be slow (image pull); a concurrent POST /runs/{id}/kill may have moved the
	// run out of STARTING (to KILLED/STOPPED) and already torn down identity +
	// broker while we were creating the sandbox. An unconditional RUNNING write
	// would resurrect a killed run AND leak the just-created container. So if the
	// conditional transition does NOT apply (the run is no longer STARTING), we
	// tear the sandbox we just created back down and stop — never running Exec or
	// the completion watcher. The kill path already revoked identity/broker; we
	// must not undo its work.
	applied, uerr := s.cfg.Store.UpdateRunStateIf(ctx, run.ID, types.RunStarting, types.RunRunning)
	if uerr != nil || !applied {
		// Killed/stopped mid-dispatch (or a store error). Free the orphaned
		// sandbox and bail without resurrecting the run.
		_ = s.cfg.Runner.StopSandbox(ctx, sb.Ref)
		data := map[string]any{
			"sandbox_ref": sb.Ref,
			"note":        "run left STARTING by a concurrent kill/stop during CreateSandbox; sandbox torn down, dispatch aborted",
		}
		if uerr != nil {
			data["error"] = uerr.Error()
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch",
			run.ID.String(), "failure", mustJSON(data)))
		return
	}

	// INTERACTIVE MODE: skip the agent Exec AND the completion watcher. The
	// sandbox is RUNNING and idle (the container holds open), ready for a human to
	// `wardyn attach`. There is no agent process, so there is nothing for the
	// watcher to Wait on — starting it would have it observe an immediate Wait
	// failure (no tracked agent exec) and could prematurely terminate the run.
	if interactive {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.interactive",
			run.ID.String(), "success", mustJSON(map[string]any{
				"sandbox_ref": sb.Ref,
				"note":        "interactive run: no agent task exec'd; awaiting attach",
			})))
		return
	}

	// When a task is provided, launch the agent process inside the now-running
	// sandbox. The driver wraps the argv with wardyn-rec (recording) when
	// configured. Exec failure: audit + stop the sandbox + mark FAILED.
	if run.Task != "" {
		argv := []string{"/usr/local/bin/agent-run", run.Task}
		if xerr := s.cfg.Runner.Exec(ctx, sb.Ref, argv); xerr != nil {
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.exec",
				run.ID.String(), "failure", mustJSON(map[string]any{"error": xerr.Error()})))
			_ = s.cfg.Runner.StopSandbox(ctx, sb.Ref)
			// Conditional: a concurrent kill may have moved RUNNING->KILLED; don't
			// clobber it with FAILED.
			_, _ = s.cfg.Store.UpdateRunStateIf(ctx, run.ID, types.RunRunning, types.RunFailed)
			return
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.exec",
			run.ID.String(), "success", mustJSON(map[string]any{"argv": argv})))

		// Completion tracking: watch the agent process to exit and propagate its
		// outcome. The watcher runs on a DETACHED context (NOT ctx — that is the
		// request context, cancelled when the create-run handler returns, which
		// would kill the watcher immediately). See startCompletionWatcher.
		s.startCompletionWatcher(run.ID, sb.Ref)
	}
}

// startCompletionWatcher launches the detached goroutine that blocks until the
// agent process exits and then propagates its outcome to the run's durable
// state + audit trail. Invariants it upholds:
//
//   - DETACHED CONTEXT: it derives its context from s.cfg.BaseCtx (the daemon
//     rootCtx), NOT the request/dispatch ctx. The request ctx is cancelled the
//     moment the create-run handler returns, which would otherwise cancel
//     Runner.Wait and the watcher before the agent ever finishes. BaseCtx lives
//     for the daemon's lifetime (cancelled on shutdown).
//   - KILLED-RACE GUARD: the terminal transition is a conditional store update
//     from RUNNING only (UpdateRunStateIf). A user may `wardyn kill` mid-run,
//     moving the run to KILLED and tearing the sandbox down; the watcher must
//     NOT clobber that. If the conditional update does not apply (the run is no
//     longer RUNNING), the watcher does nothing further — in particular it does
//     NOT tear the sandbox down (kill already did).
//   - TEARDOWN ONLY ON WIN: StopSandbox is called only when the watcher won the
//     RUNNING->terminal transition, so resources are freed exactly once and a
//     run someone else killed is left alone.
func (s *Server) startCompletionWatcher(runID uuid.UUID, ref string) {
	if s.cfg.Runner == nil {
		return
	}
	base := s.cfg.BaseCtx
	if base == nil {
		base = context.Background()
	}
	go func() {
		// Contain a panic in the detached watcher (e.g. a driver Wait bug) so it
		// can't crash the daemon; record it for forensics (mirrors reconcileWatch).
		defer func() {
			if r := recover(); r != nil {
				s.recordAudit(base, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.complete",
					runID.String(), "failure", mustJSON(map[string]any{"panic": fmt.Sprintf("%v", r)})))
			}
		}()
		exitCode, werr := s.cfg.Runner.Wait(base, ref)
		if werr != nil {
			// Wait failed (ctx cancelled at shutdown, daemon stopping, or the
			// sandbox was torn down out from under us by a kill). Do not force a
			// state change: a kill already set the terminal state, and at
			// shutdown the run is best left as-is for the next boot to observe.
			// Audit the watcher's exit for forensics.
			s.recordAudit(base, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.complete",
				runID.String(), "failure", mustJSON(map[string]any{"error": werr.Error()})))
			return
		}

		// Map exit code to terminal state: 0 => COMPLETED, non-zero => FAILED.
		terminal := types.RunCompleted
		outcome := "success"
		if exitCode != 0 {
			terminal = types.RunFailed
			outcome = "failure"
		}

		// KILLED-race guard: transition ONLY from RUNNING. If a kill/stop already
		// moved the run to a terminal state, applied is false and we leave it be.
		applied, uerr := s.cfg.Store.UpdateRunStateIf(base, runID, types.RunRunning, terminal)
		if uerr != nil {
			s.recordAudit(base, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.complete",
				runID.String(), "failure", mustJSON(map[string]any{
					"exit_code": exitCode, "error": uerr.Error(),
				})))
			return
		}
		if !applied {
			// Run already terminal (e.g. KILLED by a user mid-run). Do nothing —
			// the kill path already tore the sandbox down.
			return
		}

		s.recordAudit(base, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.complete",
			runID.String(), outcome, mustJSON(map[string]any{
				"exit_code": exitCode, "state": terminal,
			})))

		// KILL-SWITCH CASCADE ON EVERY TERMINAL TRANSITION (matches the docs:
		// revocation fires on every run stop, "including failure"). We won the
		// RUNNING->terminal transition, so this is the authoritative end of the
		// run: deny-list the run token (Identity.RevokeRun) and revoke any minted
		// broker credentials (Broker.RevokeRun), mirroring handleKillRun. Both are
		// nil-safe and best-effort; a revocation error is audited, never fatal.
		s.revokeRunCascade(base, runID)

		// We won the transition: free the sandbox resources. Idempotent on a gone
		// sandbox. Only reached when the watcher (not a concurrent kill) advanced
		// the state, so we never tear down a run someone else killed.
		if serr := s.cfg.Runner.StopSandbox(base, ref); serr != nil {
			s.recordAudit(base, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.complete",
				ref, "failure", mustJSON(map[string]any{
					"exit_code": exitCode, "teardown_error": serr.Error(),
				})))
		}

		// Reconcile a governed workspace run that ended WITHOUT delivering its
		// result (e.g. the sandbox couldn't reach the control plane) so the
		// workspace never hangs in scanning/verifying forever.
		s.reconcileWorkspaceRun(base, runID)
		// Capture a record run's evidence from its audit events (server-side,
		// pure — no upload involved) now that the run is terminal.
		s.reconcileRecordRun(base, runID)
	}()
}

// handleListRuns returns all runs in reverse creation order.
func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.cfg.Store.ListRuns(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list runs: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

// handleGetRun returns one run by id.
func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, err := s.cfg.Store.GetRun(r.Context(), id)
	if notFoundIf(w, err, "run") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get run: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// isTerminalRunState reports whether a run is in a terminal (already-ended)
// state. The kill-switch must not re-kill or clobber a run that already ended
// (COMPLETED/FAILED by the watcher, or KILLED/STOPPED/ARCHIVED earlier).
func isTerminalRunState(st types.RunState) bool {
	switch st {
	case types.RunCompleted, types.RunFailed, types.RunKilled, types.RunStopped, types.RunArchived:
		return true
	default:
		return false
	}
}

// revokeRunCascade performs the identity + broker revocation half of the
// kill-switch cascade for a run: it deny-lists the run token
// (Identity.RevokeRun) and revokes any minted broker credentials
// (Broker.RevokeRun). Both are nil-safe and best-effort; an error is audited as
// run.revoke/failure but never propagated (revocation must not gate the caller).
// It is shared by handleKillRun and the completion watcher so EVERY terminal
// transition revokes (matching the documented cascade-on-every-stop promise).
func (s *Server) revokeRunCascade(ctx context.Context, runID uuid.UUID) {
	data := map[string]any{}
	if s.cfg.Identity != nil {
		if rerr := s.cfg.Identity.RevokeRun(ctx, runID); rerr != nil {
			data["identity_error"] = rerr.Error()
		}
	}
	if s.cfg.Broker != nil {
		if berr := s.cfg.Broker.RevokeRun(ctx, runID); berr != nil {
			data["broker_error"] = berr.Error()
		}
	}
	if len(data) > 0 {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.revoke",
			runID.String(), "failure", mustJSON(data)))
	}
}

// handleKillRun is the kill-switch: it cascades in a FIXED order — runner
// teardown, identity revocation, broker credential revocation, state KILLED —
// then audits run.kill. The order matters: tear the sandbox down first so it
// cannot use a credential it already holds, then deny any future mints
// (identity + broker), then mark the durable state.
//
// IDEMPOTENCY / TERMINAL GUARD: a run that is ALREADY terminal
// (COMPLETED/FAILED/KILLED/STOPPED/ARCHIVED) is NOT re-killed: blindly writing
// KILLED would corrupt a COMPLETED/FAILED outcome and emit a bogus run.kill
// audit. We return 409 without touching state, the runner, or the cascade.
func (s *Server) handleKillRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, id)
	if notFoundIf(w, err, "run") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get run: "+err.Error())
		return
	}

	// TERMINAL GUARD: do not clobber an already-ended run. No state write, no
	// runner teardown, no revocation, no run.kill audit — the run already ended
	// and its terminal transition (the watcher's COMPLETED/FAILED, or a prior
	// kill/stop) already ran the cascade.
	if isTerminalRunState(run.State) {
		writeError(w, http.StatusConflict,
			"run is already terminal (state="+string(run.State)+"); not re-killing")
		return
	}

	// Run the teardown/revocation cascade and the terminal state write on a
	// context DETACHED from the request: once a kill begins it must complete even
	// if the client disconnects, or a half-applied kill could strand a live token
	// or a running sandbox (C4). Read the principal from the request first.
	killerType, killer := actorFromRequest(r)
	cascadeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	killData := map[string]any{}

	// (1) Runner teardown (immediate). Idempotent on a gone sandbox.
	if s.cfg.Runner != nil && run.SandboxRef != "" {
		if kerr := s.cfg.Runner.KillSandbox(cascadeCtx, run.SandboxRef); kerr != nil {
			killData["runner_error"] = kerr.Error()
		}
	}
	// (2) Identity revocation: deny every (current+future) token for the run. A
	// bounded retry guards against a transient store error leaving the run's
	// JWT-SVID valid (until its <=1h TTL) while the kill is reported as success.
	if rerr := retryQuick(cascadeCtx, func() error { return s.cfg.Identity.RevokeRun(cascadeCtx, id) }); rerr != nil {
		killData["identity_error"] = rerr.Error()
	}
	// (3) Broker credential revocation (best-effort; audits per minted jti).
	if s.cfg.Broker != nil {
		if berr := retryQuick(cascadeCtx, func() error { return s.cfg.Broker.RevokeRun(cascadeCtx, id) }); berr != nil {
			killData["broker_error"] = berr.Error()
		}
	}
	// (4) Durable state. Conditional from the (non-terminal) state we read so a
	// terminal transition that raced in between us reading and writing (e.g. the
	// completion watcher winning RUNNING->COMPLETED) is not clobbered.
	applied, serr := s.cfg.Store.UpdateRunStateIf(cascadeCtx, id, run.State, types.RunKilled)
	if serr != nil {
		writeError(w, http.StatusInternalServerError, "update run state: "+serr.Error())
		return
	}
	if !applied {
		// The run moved to another state between our read and our write (a
		// concurrent terminal transition). Do not emit a bogus run.kill; report
		// the conflict. The teardown/revocation above are idempotent.
		writeError(w, http.StatusConflict,
			"run state changed concurrently; not overwriting with KILLED")
		return
	}

	// HONEST OUTCOME (C2): the kill-switch is the central governance control and
	// the audit log is the system of record. If ANY teardown/revocation step
	// failed, the run is marked KILLED but a minted token may still be valid until
	// its TTL or the sandbox may still be live — so we must NOT report success.
	// Emit run.kill with the TRUE outcome plus a distinct run.revoke/failure
	// event, and return 500 so the operator/CLI retries instead of believing the
	// run is contained.
	outcome := "success"
	if len(killData) > 0 {
		outcome = "failure"
	}
	s.recordAudit(cascadeCtx, s.auditEvent(&id, killerType, killer, "run.kill",
		id.String(), outcome, mustJSON(killData)))

	// A killed workspace run still settles its workspace: verify/scan runs get
	// the no-result reconcile; a record run's kill IS the normal "Done recording"
	// for interactive mode (which has no completion watcher), so capture here.
	s.reconcileWorkspaceRun(cascadeCtx, id)
	s.reconcileRecordRun(cascadeCtx, id)

	if outcome == "failure" {
		s.recordAudit(cascadeCtx, s.auditEvent(&id, types.ActorSystem, "wardynd", "run.revoke",
			id.String(), "failure", mustJSON(killData)))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"id":     id,
			"state":  types.RunKilled,
			"errors": killData,
			"error":  "run marked KILLED but one or more teardown/revocation steps failed; the run may not be fully contained — retry the kill",
		})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "state": types.RunKilled})
}

// retryQuick runs fn up to 3 times with a short linear backoff, returning the
// last error. It stops early if ctx is done. Used by the kill-switch revocation
// steps so a transient store error does not leave a token valid while the kill is
// reported as success.
func retryQuick(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(time.Duration(attempt) * 100 * time.Millisecond):
			}
		}
		if err = fn(); err == nil {
			return nil
		}
	}
	return err
}

// resolvePolicy returns the spec + policy id to attach. When policyID is nil it
// returns the configured default with a nil id (the default is not a stored row).
func (s *Server) resolvePolicy(ctx context.Context, policyID *uuid.UUID) (types.RunPolicySpec, *uuid.UUID, error) {
	if policyID == nil {
		return s.cfg.DefaultPolicy, nil, nil
	}
	p, err := s.cfg.Store.GetPolicy(ctx, *policyID)
	if err != nil {
		return types.RunPolicySpec{}, nil, err
	}
	pid := p.ID
	return p.Spec, &pid, nil
}

// grantChecker is the optional grant-gating surface implemented by the embedded
// identity provider (CheckGrants). The API uses it to refuse policies whose
// grants require a different provider, without importing the embedded package.
type grantChecker interface {
	CheckGrants(grants []types.GrantSpec) error
}

// bestClass returns the strongest class a runner declares (slice is
// strongest-last per the Capabilities contract), or "" if none.
func bestClass(classes []types.ConfinementClass) types.ConfinementClass {
	best := types.ConfinementClass("")
	for _, c := range classes {
		if confinementRank[c] > confinementRank[best] {
			best = c
		}
	}
	return best
}

// classesOrNone renders an advertised confinement set for error messages, or
// "none" when the runner advertises no class.
func classesOrNone(classes []types.ConfinementClass) string {
	if len(classes) == 0 {
		return "none"
	}
	parts := make([]string, len(classes))
	for i, c := range classes {
		parts[i] = string(c)
	}
	return strings.Join(parts, ", ")
}

// agentImage resolves an agent name to its OCI image. images is consulted first
// (operator-provided map from WARDYN_AGENT_IMAGES); when the agent is absent or
// the map is nil, the ghcr convention image is used as fallback.
func agentImage(agent string, images map[string]string) string {
	if ref, ok := images[agent]; ok {
		return ref
	}
	return "ghcr.io/cjohnstoniv/agent-" + agent + ":latest"
}

// gitEmailLocal makes a git-safe email local-part from a principal (e.g.
// "local:cjohn" -> "local_cjohn"): keep alphanumerics and a few safe symbols,
// map everything else to '_', so the synthesized GIT_AUTHOR_EMAIL is well-formed.
func gitEmailLocal(principal string) string {
	if principal == "" {
		return "operator"
	}
	b := make([]byte, 0, len(principal))
	for i := 0; i < len(principal); i++ {
		c := principal[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '-', c == '_':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}

// mavenProxyOpts builds the MAVEN_OPTS JVM proxy sysprops that route Maven
// through the wardyn-proxy sidecar. Maven (unlike npm/pip/cargo/go/git) ignores
// HTTP(S)_PROXY env, so without these it resolves Maven Central directly and
// fails "Unknown host" in the gatewayless sandbox. proxyURL is "http://host:port".
// nonProxyHosts excludes loopback + the proxy itself. Returns "" if unparseable.
func mavenProxyOpts(proxyURL string) string {
	s := strings.TrimSpace(proxyURL)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "https://"), "http://")
	s = strings.TrimRight(s, "/")
	host, port := s, "3128"
	if i := strings.LastIndex(s, ":"); i >= 0 {
		host, port = s[:i], s[i+1:]
	}
	if host == "" {
		return ""
	}
	return fmt.Sprintf(
		"-Dhttp.proxyHost=%s -Dhttp.proxyPort=%s -Dhttps.proxyHost=%s -Dhttps.proxyPort=%s "+
			"-Dhttp.nonProxyHosts=localhost|127.0.0.1|::1|wardyn-proxy",
		host, port, host, port)
}

// resolveUpstreamProxyURL resolves the operator-wide site-config upstream/corp
// proxy secret ref (types.SiteConfig.UpstreamProxySecretRef) to a URL for
// ProxyConfig.UpstreamProxyURL. getSecret resolves a secret name to its
// plaintext value (typically s.cfg.Secrets.Get); nil means no secret store is
// configured.
//
// Fail SAFE, never errors: an empty ref, a reserved platform-internal name
// (defense-in-depth — validateSiteConfig/validSecretRef already reject this at
// PUT /api/v1/site-config write time, but this guards a row written before
// that check existed, mirroring handleInternalInjection's sink-side reserved-
// name guard), a missing secret store, an unresolvable secret, or a non-http
// URL all return ("", <reason>) — the caller audits the reason and dispatches
// with direct egress instead of failing the run. A resolved http URL returns
// (url, "").
//
// Scheme is restricted to http because the sidecar's own config validation
// (parseUpstreamProxy, internal/egress/proxy/upstream.go) rejects https: the
// hop TO the corp proxy is a plaintext CONNECT + Proxy-Authorization today, and
// an https:// proxy URL would need a TLS wrap first or leak that Basic
// credential in cleartext — so an https ref is skipped here rather than
// crashing the proxy sidecar at startup.
func resolveUpstreamProxyURL(ctx context.Context, secretRef string, getSecret func(context.Context, string) ([]byte, error)) (proxyURL, failReason string) {
	if secretRef == "" {
		return "", ""
	}
	if reservedSecretNames[secretRef] {
		return "", "reserved-secret-name"
	}
	if getSecret == nil {
		return "", "no-secret-store"
	}
	val, err := getSecret(ctx, secretRef)
	if err != nil {
		return "", "secret-not-found"
	}
	raw := strings.TrimSpace(string(val))
	u, perr := url.Parse(raw)
	if perr != nil || !strings.EqualFold(u.Scheme, "http") {
		return "", "unsupported-scheme"
	}
	return raw, ""
}

// Bedrock: AWS Bedrock as an Anthropic transport for claude-code runs (an
// enterprise path — no direct Anthropic egress, billed via AWS). Bedrock
// authenticates with AWS SigV4 REQUEST SIGNING, not a static bearer header, so
// unlike an api_key grant (proxy-injected, never resident) the proxy has
// nothing to strip-and-replace: the AWS credentials MUST be resident in the
// sandbox env, same tradeoff already accepted for the Claude subscription
// mount above. A ~/.aws host mount (mirroring the ~/.claude subscription
// mount) is a documented alternative for a future pass; this wires the
// secret-env lane only.
//
// Region/model are OPERATOR BOOT-TIME config (BedrockRegion/BedrockModel,
// mirroring AgentAnthropicModel — no live admin write path, same as the
// WARDYN_DEFAULT_POLICY precedent); the AWS credentials are read directly
// from the secret store at dispatch time — a new kind of secret consumption
// for this codebase (every other consumer is proxy-injection-at-mint-time),
// necessary because SigV4 can't be injected after the fact.
const (
	bedrockAccessKeyIDSecret     = "aws-access-key-id"
	bedrockSecretAccessKeySecret = "aws-secret-access-key"
	bedrockSessionTokenSecret    = "aws-session-token" // optional (STS/AssumeRole creds)
	// bedrockAPIKeySecret holds an AWS Bedrock BEARER token (AWS_BEARER_TOKEN_BEDROCK).
	// Unlike SigV4 access keys, a bearer token is a STATIC Authorization header, so
	// it can be proxy-INJECTED (never resident) exactly like an api_key — the
	// preferred, higher-trust Bedrock path when present.
	bedrockAPIKeySecret = "bedrock-api-key"
)

// bedrockAuth is the resolved Bedrock authentication plan for a run.
type bedrockAuth struct {
	env         map[string]string // sandbox env additions (bearer: placeholder; resident: real creds)
	egressHosts []string          // regional data+control plane hosts to allow
	ready       bool              // false => fall back to api-key mode
	// bearer selects the never-resident path: bedrock-runtime is TLS-MITM'd and the
	// Authorization: Bearer header is injected proxy-side from bedrockAPIKeySecret,
	// so the sandbox holds only a placeholder. When false (resident path), AWS SigV4
	// creds are placed in env (SigV4 can't be proxy-injected).
	bearer      bool
	runtimeHost string // bedrock-runtime host (MITM+inject target in bearer mode)
	// awsMount selects the host-mode ~/.aws bind-mount path: the SDK resolves
	// credentials (incl. auto-refreshing AWS SSO) from the read-only mount, so no
	// static keys are stored and none are resident in env. awsMountSource is the
	// host dir to bind read-only at /home/agent/.aws. Mutually exclusive with the
	// resident-key path; bearer still wins over it.
	awsMount       bool
	awsMountSource string
}

// bedrockRuntimeHost is the regional Bedrock DATA-PLANE host claude-code's
// InvokeModel/Converse calls hit. bedrockControlHost is the companion
// CONTROL-PLANE host claude-code also calls (bedrock:ListInferenceProfiles /
// GetInferenceProfile) to resolve a cross-region inference-profile model id —
// omitting it from egress 403s a profile-id model, so both hosts are required,
// not just the data-plane one.
func bedrockRuntimeHost(region string) string {
	return fmt.Sprintf("bedrock-runtime.%s.amazonaws.com", region)
}
func bedrockControlHost(region string) string {
	return fmt.Sprintf("bedrock.%s.amazonaws.com", region)
}

// ssoEgressHosts are the AWS IAM Identity Center (SSO) endpoints the sandbox SDK
// must reach to exchange a cached SSO token for role credentials: oidc.<r> for
// token refresh and portal.sso.<r> for GetRoleCredentials. Only needed on the
// ~/.aws-mount path; region is the SSO region (may differ from the Bedrock one).
func ssoEgressHosts(ssoRegion string) []string {
	return []string{
		fmt.Sprintf("oidc.%s.amazonaws.com", ssoRegion),
		fmt.Sprintf("portal.sso.%s.amazonaws.com", ssoRegion),
	}
}

// sandboxAWSDir is where the host ~/.aws is bind-mounted read-only in the run.
const sandboxAWSDir = "/home/agent/.aws"

// resolveBedrockAuth decides whether this run should authenticate to Claude via
// Amazon Bedrock and, if so, returns the sandbox env additions (the
// CLAUDE_CODE_USE_BEDROCK on-switch, region, model id, and resident AWS creds)
// plus the regional egress hosts to allow. ready is false whenever Bedrock isn't
// configured (region/model unset — the common case for non-Bedrock operators)
// OR is misconfigured (region/model set but the AWS credential secrets aren't
// both present) — either way the caller falls back to the existing api-key
// path, so a partial Bedrock config never breaks a run, it just doesn't get
// Bedrock. subscriptionActive pre-empts Bedrock: the resident Claude OAuth
// mount and Bedrock are mutually exclusive Anthropic transports. modelRun=false
// (a verify or scan run that makes no model call) also returns ready=false, so
// the resident AWS creds never land in a sandbox that won't sign a Bedrock request.
func (s *Server) resolveBedrockAuth(ctx context.Context, runAgent string, subscriptionActive, modelRun bool) bedrockAuth {
	if !modelRun || subscriptionActive || runAgent != "claude-code" ||
		s.cfg.BedrockRegion == "" || s.cfg.BedrockModel == "" || s.cfg.Secrets == nil {
		return bedrockAuth{}
	}
	runtimeHost := bedrockRuntimeHost(s.cfg.BedrockRegion)
	hosts := []string{runtimeHost, bedrockControlHost(s.cfg.BedrockRegion)}
	// Common Bedrock env: the on-switch, region, and model id. AWS_REGION is what
	// claude-code reads; AWS_DEFAULT_REGION is the broader AWS-SDK fallback. The
	// model id is a cross-region INFERENCE-PROFILE id (e.g.
	// "us.anthropic.claude-sonnet-4-5-...") or an application-inference-profile ARN
	// — NOT a bare foundation-model id (Bedrock silently rewrites those and can 403
	// under an SCP). Operator-supplied; Wardyn does not validate the format.
	base := func() map[string]string {
		return map[string]string{
			"CLAUDE_CODE_USE_BEDROCK": "1",
			"AWS_REGION":              s.cfg.BedrockRegion,
			"AWS_DEFAULT_REGION":      s.cfg.BedrockRegion,
			"ANTHROPIC_MODEL":         s.cfg.BedrockModel,
		}
	}

	// PREFERRED: bearer-token mode. A Bedrock API key is a STATIC Authorization
	// header, so the proxy TLS-MITMs bedrock-runtime and injects it — the sandbox
	// holds only a placeholder, never the real token (trust parity with api-key /
	// subscription). Selected whenever a bedrock-api-key secret exists.
	if bearer, berr := s.cfg.Secrets.Get(ctx, bedrockAPIKeySecret); berr == nil && len(bearer) > 0 {
		env := base()
		// A non-empty sentinel so claude-code uses bearer auth (not SigV4); the proxy
		// overwrites the Authorization header with the real token on the wire.
		env["AWS_BEARER_TOKEN_BEDROCK"] = "wardyn-proxy-injected"
		return bedrockAuth{env: env, egressHosts: hosts, ready: true, bearer: true, runtimeHost: runtimeHost}
	}

	// HOST-MODE ~/.aws MOUNT: bind the operator's host ~/.aws read-only into the
	// sandbox and let the AWS SDK resolve credentials itself — including AWS SSO /
	// IAM Identity Center sessions it refreshes on demand, so a short-lived login
	// never goes stale and nothing is stored in Wardyn. No resident static keys.
	// Opt-in + host-mode-only (BedrockAWSConfigDir is set only by run-host.sh /
	// setup.sh); fail SAFE to the next path if the dir doesn't exist on this host.
	if s.cfg.BedrockAWSConfigDir != "" {
		if st, err := os.Stat(s.cfg.BedrockAWSConfigDir); err == nil && st.IsDir() {
			env := base()
			// Point the SDK at the mount explicitly (robust even if HOME isn't
			// /home/agent for some exec path); no AWS_ACCESS_KEY_ID — the SDK
			// resolves from the mounted config + SSO cache.
			env["AWS_CONFIG_FILE"] = sandboxAWSDir + "/config"
			env["AWS_SHARED_CREDENTIALS_FILE"] = sandboxAWSDir + "/credentials"
			if s.cfg.BedrockAWSProfile != "" {
				env["AWS_PROFILE"] = s.cfg.BedrockAWSProfile
			}
			ssoRegion := cmp.Or(s.cfg.BedrockAWSSSORegion, s.cfg.BedrockRegion)
			hosts = append(hosts, ssoEgressHosts(ssoRegion)...)
			return bedrockAuth{env: env, egressHosts: hosts, ready: true,
				awsMount: true, awsMountSource: s.cfg.BedrockAWSConfigDir}
		}
	}

	// FALLBACK: resident SigV4 access keys. SigV4 signs each request in-process, so
	// the creds MUST be resident in the sandbox env (documented exception, masked +
	// modelRun-gated). Requires both access key + secret key.
	accessKey, aerr := s.cfg.Secrets.Get(ctx, bedrockAccessKeyIDSecret)
	secretKey, serr := s.cfg.Secrets.Get(ctx, bedrockSecretAccessKeySecret)
	if aerr != nil || serr != nil || len(accessKey) == 0 || len(secretKey) == 0 {
		return bedrockAuth{}
	}
	env := base()
	env["AWS_ACCESS_KEY_ID"] = string(accessKey)
	env["AWS_SECRET_ACCESS_KEY"] = string(secretKey)
	if tok, terr := s.cfg.Secrets.Get(ctx, bedrockSessionTokenSecret); terr == nil && len(tok) > 0 {
		env["AWS_SESSION_TOKEN"] = string(tok)
	}
	return bedrockAuth{env: env, egressHosts: hosts, ready: true}
}

// createRunResponse is the create-run reply: the run's fields at the TOP LEVEL
// (so existing AgentRun decoders are unaffected) plus optional advisory warnings
// (e.g. a workspace-directory collision with another active run — discouraged,
// never blocked).
type createRunResponse struct {
	types.AgentRun
	Warnings []string `json:"warnings,omitempty"`
}

// primaryWorkspacePath returns the run's first local host workspace mount source
// (the directory the agent operates in), or "" for a git-clone / ephemeral run.
func primaryWorkspacePath(spec types.RunPolicySpec) string {
	for _, m := range spec.WorkspaceMounts {
		if strings.TrimSpace(m.Source) != "" {
			return m.Source
		}
	}
	return ""
}

// resourceLimitsToRunner maps a policy's optional ResourceLimits onto the runner
// spec. A nil policy block (or a zero field) yields the zero value, which the
// docker driver fills with conservative platform defaults — so EVERY run is
// CPU/memory/PID capped even when a policy sets nothing (C5: fleet safety).
func resourceLimitsToRunner(rl *types.ResourceLimits) runner.Resources {
	if rl == nil {
		return runner.Resources{}
	}
	return runner.Resources{
		CPUMillis: int64(rl.CPUMillis),
		MemoryMiB: int64(rl.MemoryMiB),
		PidsLimit: int64(rl.PidsLimit),
		DiskMiB:   int64(rl.DiskMiB),
	}
}

// adminTokenPrincipal is the actor recorded for an admin-bearer-token caller
// with no verified human identity (no LocalMode operator, no OIDC session). It
// is a NON-HUMAN, mechanism-named principal so the audit never implies a named
// person acted when only the shared token did.
const adminTokenPrincipal = "admin-token"

// principalFromRequest derives the actor name for an admin-gated action. It is
// the name half of actorFromRequest; callers that also record actor_type should
// prefer actorFromRequest so a token action is not mislabeled as human.
func principalFromRequest(r *http.Request) string {
	_, name := actorFromRequest(r)
	return name
}

// actorTypeFromRequest is the actor-type half of actorFromRequest, for audit
// sites that already pass principalFromRequest(r) for the name. Pairing them as
// (actorTypeFromRequest(r), principalFromRequest(r)) records a bare admin-token
// caller as system, not a forged human, without threading a local through every
// handler.
func actorTypeFromRequest(r *http.Request) types.ActorType {
	t, _ := actorFromRequest(r)
	return t
}

// actorFromRequest resolves the audit actor (type + name) for an admin-gated
// public-API action. Resolution order, strongest attribution first (invariant 4):
//
//  1. LOCAL HOST MODE — the operator injected by humanOrAdminAuth on the trusted
//     single-dev machine. Here (and ONLY here, off SSO) the X-Wardyn-Principal
//     DEV-ONLY override (docs/sdk.md) is honored, since the machine is trusted;
//     otherwise the configured operator (e.g. "local:cjohn") is used.
//  2. A VERIFIED OIDC SSO session (the IdP "sub"), published by humanOrAdminAuth.
//     A real human already won, so the X-Wardyn-Principal header is moot.
//  3. Admin bearer token, NO verified human. Attributed to a non-human
//     system actor ("admin-token").
//
// SECURITY (FIX #10 — human-attribution forgery): X-Wardyn-Principal is
// attacker-controllable and is documented as a DEV-ONLY override. It is
// therefore trusted ONLY in LocalMode (case 1). For a plain admin-token caller
// (case 3) the header is IGNORED: honoring it would let any WARDYN_ADMIN_TOKEN
// bearer forge a named human as decided_by / sub / sponsor in the append-only
// audit — recording that "alice@example.com approved" a credential that was in
// fact never human-gated (breaks invariant 4 per-run identity and invariant 6
// non-repudiation). A token action is recorded as system/admin-token, not human.
func actorFromRequest(r *http.Request) (types.ActorType, string) {
	if op := localPrincipalFromContext(r.Context()); op != "" {
		if h := r.Header.Get("X-Wardyn-Principal"); h != "" {
			return types.ActorHuman, h
		}
		return types.ActorHuman, op
	}
	if sub := oidcHumanFromContext(r.Context()); sub != "" {
		return types.ActorHuman, sub
	}
	return types.ActorSystem, adminTokenPrincipal
}

// repoFieldSafe rejects a repo slug/URL that contains any ASCII control
// character or whitespace. run.Repo is attacker-influenceable run-request text
// and flows verbatim into an in-sandbox `git clone "$WARDYN_REPO_URL"`; the
// agent-run scripts always double-quote it, but we still refuse control/space
// bytes so a slug can never smuggle a newline, NUL, or argument break into the
// sandbox env or the clone command. Fail closed: anything unexpected means the
// repo env is simply not surfaced and the agent runs in an empty workspace.
// ponytail: stricter than the old hand-rolled scan — unicode.IsSpace also
// rejects Unicode space separators (U+2000-200A, U+3000, ...) the old fixed
// list let through; approved as a hardening, not a regression, for this
// trust-boundary check.
func repoFieldSafe(s string) bool {
	return !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	})
}

// repoCloneURL derives a git clone URL from a (already sanitized) repo slug.
//   - If the slug is already a URL (contains "://"), it is passed through as-is.
//   - Otherwise, if it matches a bare <org>/<name> GitHub slug, an https GitHub
//     clone URL is built.
//   - Anything else yields "" (no clone URL; the slug is still surfaced as
//     audit metadata and the agent runs in an empty workspace).
//
// LIMITATION (v0.1): non-URL slugs are assumed to be GitHub. The brokered git
// credential helper (wardyn-git-helper) and the demo egress allowlist are
// GitHub-scoped, so cross-host cloning of a bare slug is out of scope for now;
// pass a full https:// URL (and allowlist its host) to clone elsewhere.
//
// SSH: an ssh://[user@]host/… or scp-form user@host:path clone URL is accepted
// ONLY when the host is a supported SSH-over-443 provider (sshOver443Endpoint:
// GitHub / Azure DevOps). The URL passes through VERBATIM — the agent-run sandbox
// (not this URL) supplies the minted key, known_hosts and the :443 ProxyCommand;
// the run's ssh_key grant (maybeSSHKeyGrant) authorizes it. Any other transport
// (file://, git's ext::/fd:: helpers, an unsupported SSH host, or an explicit
// non-443 SSH port) fails closed.
func repoCloneURL(slug string) string {
	if strings.Contains(slug, "://") {
		if strings.HasPrefix(slug, "https://") || strings.HasPrefix(slug, "http://") {
			return slug
		}
		if strings.HasPrefix(slug, "ssh://") {
			if host, ok := sshCloneHost(slug); ok {
				if _, ok := sshOver443Endpoint(host); ok {
					return slug
				}
			}
		}
		return ""
	}
	// scp-form user@host:path — has '@' and ':' but no scheme.
	if strings.ContainsRune(slug, '@') && strings.ContainsRune(slug, ':') {
		if host, ok := sshCloneHost(slug); ok {
			if _, ok := sshOver443Endpoint(host); ok {
				return slug
			}
		}
		return ""
	}
	// Bare <org>/<name>: exactly two non-empty path segments, no extra slashes.
	parts := strings.Split(slug, "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return "https://github.com/" + slug + ".git"
	}
	return ""
}

// buildRepoRecords assembles the WARDYN_REPOS env value: newline-delimited,
// tab-separated <url>\t<dest>\t<slug> records the agent-run entrypoint iterates to
// clone each repo. Sources are the legacy single run.Repo (first, keeping its
// default ~/work/<name> dest) plus each onboarded WorkspaceRepo (already
// onboarding-gated). Every field is repoFieldSafe (no whitespace/control chars) so
// the tab/newline framing cannot be smuggled past; every dest is a validated
// allowed-prefix target, deduped so two repos never target one directory. A slug
// with no derivable clone URL, an unsafe/out-of-prefix dest, or a duplicate dest is
// skipped. Returns "" when there is nothing to clone.
func buildRepoRecords(legacyRepo string, repos []types.WorkspaceRepo) string {
	const workRoot = "/home/agent/work"
	seenDest := map[string]bool{}
	var b strings.Builder
	add := func(slug, dest string) {
		slug = strings.TrimSpace(slug)
		if slug == "" || !repoFieldSafe(slug) {
			return
		}
		url := repoCloneURL(slug)
		if url == "" {
			return
		}
		if dest == "" {
			name := strings.TrimSuffix(url[strings.LastIndex(url, "/")+1:], ".git")
			if name == "" {
				name = "repo"
			}
			dest = workRoot + "/" + name
		}
		if !repoFieldSafe(dest) || runner.ValidateTarget(dest) != nil || seenDest[dest] {
			return
		}
		seenDest[dest] = true
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(url)
		b.WriteByte('\t')
		b.WriteString(dest)
		b.WriteByte('\t')
		b.WriteString(slug)
	}
	add(legacyRepo, "") // legacy single repo → default dest
	for _, wr := range repos {
		add(wr.Repo, wr.Target)
	}
	return b.String()
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// injectionRuleFromScope decodes an api_key grant scope into its proxy-side
// injection rule. Mirrors the broker's apiKeyScope shape (host, header,
// format, secret_name) with the same defaults.
func injectionRuleFromScope(scope json.RawMessage) (egress.InjectionRule, error) {
	var sc struct {
		Host       string `json:"host"`
		Header     string `json:"header"`
		Format     string `json:"format"`
		SecretName string `json:"secret_name"`
	}
	if err := json.Unmarshal(scope, &sc); err != nil {
		return egress.InjectionRule{}, err
	}
	if sc.Host == "" || sc.SecretName == "" {
		return egress.InjectionRule{}, errors.New("api_key scope requires host and secret_name")
	}
	if sc.Header == "" {
		sc.Header = "Authorization"
	}
	if sc.Format == "" {
		sc.Format = "Bearer %s"
	}
	return egress.InjectionRule{Host: sc.Host, Header: sc.Header, SecretName: sc.SecretName, Format: sc.Format}, nil
}

// gitPATScopeFields decodes a git_pat grant scope {host, secret_name, username?}.
// host and secret_name are REQUIRED (fail closed); mirrors the broker's
// gitPATScope shape. Used by policy validation, inline-secret checks, compose
// grounding, and the sandbox env wiring so all agree on the scope contract.
func gitPATScopeFields(scope json.RawMessage) (host, secretName, username string, err error) {
	var sc struct {
		Host       string `json:"host"`
		SecretName string `json:"secret_name"`
		Username   string `json:"username"`
	}
	if err = json.Unmarshal(scope, &sc); err != nil {
		return "", "", "", err
	}
	if sc.Host == "" || sc.SecretName == "" {
		return "", "", "", errors.New("git_pat scope requires host and secret_name")
	}
	return sc.Host, sc.SecretName, sc.Username, nil
}

// sshKeyScopeFields decodes an ssh_key grant scope
// {host, key_secret_ref, username?, known_hosts_secret_ref?}. host and
// key_secret_ref are REQUIRED (fail closed); mirrors the broker's sshKeyScope
// shape. Used by policy validation, inline-secret checks, and the sandbox env
// wiring so all agree on the scope contract.
func sshKeyScopeFields(scope json.RawMessage) (host, keySecretRef, username, knownHostsSecretRef string, err error) {
	var sc struct {
		Host                string `json:"host"`
		KeySecretRef        string `json:"key_secret_ref"`
		Username            string `json:"username"`
		KnownHostsSecretRef string `json:"known_hosts_secret_ref"`
	}
	if err = json.Unmarshal(scope, &sc); err != nil {
		return "", "", "", "", err
	}
	if sc.Host == "" || sc.KeySecretRef == "" {
		return "", "", "", "", errors.New("ssh_key scope requires host and key_secret_ref")
	}
	return sc.Host, sc.KeySecretRef, sc.Username, sc.KnownHostsSecretRef, nil
}

// sshOver443Endpoint maps a supported SCM host to its SSH-over-443 endpoint
// (host:443) so git-over-SSH reuses the existing CONNECT-443 egress lane with NO
// port-policy change. Returns ok=false for an unsupported host — the SSH lane is
// deliberately limited to the two providers that publish an :443 SSH endpoint
// (GitHub, Azure DevOps); a custom GHES/ADO-Server host would need port-22 egress
// and is out of scope for the SSH lane. The returned endpoint is PORT-QUALIFIED
// (":443") on purpose: it is added to the egress allowlist so it matches ONLY
// :443, closing the bare-entry "matches any port" permissiveness for SSH hosts.
func sshOver443Endpoint(host string) (endpoint string, ok bool) {
	switch strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), ".")) {
	case "github.com", "ssh.github.com":
		return "ssh.github.com:443", true
	case "dev.azure.com", "ssh.dev.azure.com":
		return "ssh.dev.azure.com:443", true
	}
	return "", false
}

// sshCloneHost extracts the host git will dial from an SSH clone URL — either
// ssh://[user@]host[:port]/path or scp-form [user@]host:path. It does NOT validate
// the host is a supported provider (callers gate on sshOver443Endpoint). ok=false
// for a non-SSH string or an explicit non-443 ssh:// port (a port-22 URL would
// override the sandbox's Port-443 ssh_config and defeat the SSH-over-443 egress
// lane, so it fails closed here).
func sshCloneHost(raw string) (host string, ok bool) {
	if strings.HasPrefix(raw, "ssh://") {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return "", false
		}
		if p := u.Port(); p != "" && p != "443" {
			return "", false
		}
		return strings.ToLower(u.Hostname()), true
	}
	if strings.Contains(raw, "://") {
		return "", false // some other scheme, not scp-form
	}
	// scp-form: [user@]host:path — exactly one host, then ':' then a non-empty path.
	s := raw
	if at := strings.IndexByte(s, '@'); at >= 0 {
		s = s[at+1:]
	}
	colon := strings.IndexByte(s, ':')
	if colon <= 0 || colon == len(s)-1 {
		return "", false
	}
	host = strings.ToLower(s[:colon])
	if strings.ContainsAny(host, "/@") {
		return "", false
	}
	return host, true
}

// canonicalSSHKeySecret maps a supported SSH host (either the primary or its
// ssh.<host> form) to the canonical ssh-key-<host-slug> secret name — the SAME
// convention setup.sh's SCM import writes and setup.go documents. Keying the
// secret off the canonical provider (not the raw URL host) is what lets an
// operator store ONE `ssh-key-github-com` and clone either github.com or
// ssh.github.com URL forms.
func canonicalSSHKeySecret(host string) (secretName string, ok bool) {
	switch strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), ".")) {
	case "github.com", "ssh.github.com":
		return "ssh-key-github-com", true
	case "dev.azure.com", "ssh.dev.azure.com":
		return "ssh-key-dev-azure-com", true
	}
	return "", false
}

// adoEgressDomains returns the Azure DevOps egress bundle {dev.azure.com,
// *.visualstudio.com} when host is an ADO host (either the modern
// dev.azure.com or a legacy org.visualstudio.com), else nil. Unlike GitHub
// (whose egress is baked into the example policies' static AllowedDomains),
// nothing today adds ADO's hosts for a git_pat grant, so dev.azure.com /
// *.visualstudio.com are in NO example policy — a plain ADO PAT grant would
// mint a credential the sandbox then has no egress to use it with. Both hosts
// are returned together (not just the matched one) because an org may clone
// via one host while ADO's REST/API surface uses the other.
func adoEgressDomains(host string) []string {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if h == "dev.azure.com" || strings.HasSuffix(h, ".visualstudio.com") {
		return []string{"dev.azure.com", "*.visualstudio.com"}
	}
	return nil
}
