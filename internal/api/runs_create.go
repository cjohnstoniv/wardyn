// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// decodeAndValidateCreateRun decodes the POST /api/v1/runs body and applies the
// fail-closed request-shape checks that need no store access: agent required,
// BYOI/devcontainer exclusivity, a known confinement_class, the task_mode enum,
// and the compose_session_id UUID contract. On any violation it writes the HTTP
// error itself and returns ok=false. Extracted verbatim from handleCreateRun.
func (s *Server) decodeAndValidateCreateRun(w http.ResponseWriter, r *http.Request) (createRunRequest, types.ConfinementClass, bool) {
	var req createRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return req, "", false
	}
	// Only agent is hard-required. Repo is OPTIONAL: an inline-policy run that
	// mounts a local host folder (WorkspaceMount target /work) has no git repo to
	// clone, so requiring a repo would block the local-folder wizard path. The
	// clone wiring in dispatch is already nil/empty-safe (it surfaces no repo env
	// when run.Repo is blank), so an empty repo simply runs in the mounted
	// workspace (or an empty one).
	if req.Agent == "" {
		writeError(w, http.StatusBadRequest, "agent is required")
		return req, "", false
	}

	// BYOI validation (fail closed before any store write): a user-supplied image
	// is mutually exclusive with a devcontainer build, and — unlike DevcontainerRepo,
	// which degrades to the convention image — an explicitly chosen image with no
	// ImageBuilder wired is a hard error (never silently swap a chosen image for the
	// convention one).
	if req.Image != "" {
		if req.DevcontainerRepo != "" {
			writeError(w, http.StatusBadRequest, "image and devcontainer_repo are mutually exclusive")
			return req, "", false
		}
		if s.cfg.ImageBuilder == nil {
			writeError(w, http.StatusBadRequest,
				"a custom sandbox image was requested but this control plane has no image builder wired "+
					"(start wardynd with -tags docker and set WARDYN_ENVBUILD_TOOLS_DIR / -envbuild)")
			return req, "", false
		}
	}

	// Validate the requested confinement class up front (fail closed before any
	// store write). Empty inherits the policy minimum; an unknown non-empty
	// value is rejected with 400.
	reqCC, ccOK := parseConfinementClass(req.ConfinementClass)
	if !ccOK {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown confinement_class %q", req.ConfinementClass))
		return req, "", false
	}

	// task_mode is a tiny closed enum; reject anything else up front (fail
	// closed, same shape as confinement_class above).
	if req.TaskMode != "" && req.TaskMode != "harness" && req.TaskMode != "exec" {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown task_mode %q (want harness or exec)", req.TaskMode))
		return req, "", false
	}

	// Same UUID contract as the compose endpoint's session_id: this field only
	// exists to correlate audit rows, so reject graffiti before anything is
	// created rather than capping arbitrary text into run.create's Data.
	if req.ComposeSessionID != "" {
		if _, err := uuid.Parse(req.ComposeSessionID); err != nil {
			writeError(w, http.StatusBadRequest, "compose_session_id must be a UUID")
			return req, "", false
		}
	}
	return req, reqCC, true
}

// resolveEnforcedConfinement resolves the run's confinement class and gates it
// against what the runner and identity provider can actually deliver (invariant
// 5, fail closed). The request value wins when set, else the policy minimum; a
// requested class must not be WEAKER than the policy minimum. The deterministic
// BLAST-RADIUS floor then raises powerful-credential runs to CC3, and the
// runner must advertise the EXACT enforced class (membership, not rank — M8: a
// Kata-only host advertises [CC1, CC3] with no CC2, so a rank check would pass
// a CC2 demand and fail later with a raw docker error). Writes the HTTP error
// itself and returns ok=false on any refusal. Extracted verbatim from
// handleCreateRun.
func (s *Server) resolveEnforcedConfinement(ctx context.Context, w http.ResponseWriter, spec types.RunPolicySpec, reqCC types.ConfinementClass) (types.ConfinementClass, bool) {
	enforced := spec.MinConfinementClass
	if reqCC != "" {
		if !confinementGE(reqCC, spec.MinConfinementClass) {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf(
				"confinement_class %s is weaker than the policy minimum %s",
				reqCC, spec.MinConfinementClass))
			return "", false
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
			return "", false
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
			return "", false
		}
	}

	// Reject cloud_sts grants up front: the embedded provider hard-requires
	// SPIRE for them (invariant 5). We check via the identity provider so the
	// spire provider can later accept them without an API change.
	if checker, ok := s.cfg.Identity.(grantChecker); ok {
		if err := checker.CheckGrants(spec.EligibleGrants); err != nil {
			writeError(w, http.StatusUnprocessableEntity,
				"policy requires the spire identity provider: "+err.Error())
			return "", false
		}
	}
	return enforced, true
}

// grantWiring is what persistRunGrants derives from the policy's eligible
// grants: the non-secret sandbox wiring (grant ids, never credential values)
// plus the extra egress each SCM lane needs.
type grantWiring struct {
	// firstGitHubGrantID is surfaced in the sandbox env as
	// WARDYN_GITHUB_GRANT_ID (non-secret: the grant is an eligibility record,
	// not a token). The run token never appears in env.
	firstGitHubGrantID *uuid.UUID
	// gitGrants is the git-broker per-repo allowlist: canonical "<org>/<repo>" ->
	// grant id, from each github_token grant's scope.repos. Delivered proxy-side
	// only (never the sandbox) so the /wardyn/gh/ route serves exactly these repos.
	gitGrants map[string]uuid.UUID
	// injections are the proxy injection configs for auto-mintable api_key
	// grants: the proxy resolves their secret VALUES at startup via the internal
	// injection endpoint (values live only in proxy memory, never in the sandbox).
	injections []runner.InjectionGrant
	// gitPATGrants: host -> grant id, surfaced as WARDYN_GIT_PAT_GRANTS so the
	// git-credential helper can mint the stored PAT for a matched non-GitHub
	// host (non-secret: an eligibility record, not the PAT itself).
	gitPATGrants map[string]string
	// gitPATEgress collects the extra hosts a git_pat grant's host needs
	// reachable beyond the grant's own host (currently just ADO's dev.azure.com
	// / *.visualstudio.com bundle — see adoEgressDomains).
	gitPATEgress []string
	// sshGrants: host -> grant id, surfaced as WARDYN_SSH_GRANTS so agent-run
	// can mint the resident private key at clone time (the key material is
	// returned only through the brokered mint path and wiped after the clone).
	sshGrants map[string]string
	// sshEgress collects the SSH-over-443 endpoints these grants need reachable.
	sshEgress []string
}

// persistRunGrants persists each eligible grant as an eligibility record (NOT
// issuance) and derives the sandbox wiring above. Approval-gated api_key grants
// are deliberately excluded from injections: an unmet approval would fail the
// proxy's startup mint and brick the sandbox's egress (fail closed, but a
// footgun as a default). A grant write failure is fatal (the run would be
// ungovernable): the HTTP error is written here and ok=false returned.
// Extracted verbatim from handleCreateRun.
func (s *Server) persistRunGrants(ctx context.Context, w http.ResponseWriter, runID uuid.UUID, now time.Time, spec types.RunPolicySpec) (grantWiring, bool) {
	gw := grantWiring{
		gitPATGrants: map[string]string{},
		sshGrants:    map[string]string{},
		gitGrants:    map[string]uuid.UUID{},
	}
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
			return gw, false
		}
		if g.Kind == types.GrantGitHubToken {
			if gw.firstGitHubGrantID == nil {
				id := grantID // copy loop var
				gw.firstGitHubGrantID = &id
			}
			// Populate the git-broker allowlist: each granted repo -> THIS grant.
			// The proxy serves /wardyn/gh/<org>/<repo> only for these keys and mints
			// the scoped installation token server-side (never into the sandbox).
			for _, repo := range githubScopeRepos(g.Scope) {
				gw.gitGrants[strings.ToLower(repo)] = grantID
			}
		}
		if g.Kind == types.GrantGitPAT {
			if host, _, _, derr := gitPATScopeFields(g.Scope); derr == nil {
				gw.gitPATGrants[host] = grantID.String()
				gw.gitPATEgress = append(gw.gitPATEgress, adoEgressDomains(host)...)
			}
		}
		if g.Kind == types.GrantSSHKey {
			// validatePolicySpec already vetted the host is a supported SSH-over-443
			// provider, so sshOver443Endpoint is expected to resolve here.
			if host, _, _, _, derr := sshKeyScopeFields(g.Scope); derr == nil {
				gw.sshGrants[host] = grantID.String()
				if ep, ok := sshOver443Endpoint(host); ok {
					gw.sshEgress = append(gw.sshEgress, ep)
				}
			}
		}
		// Approval-gated api_key grants are deliberately excluded: an unmet
		// approval would fail the proxy's startup mint and brick the sandbox's
		// egress (fail closed, but a footgun as a default).
		if g.Kind == types.GrantAPIKey && !g.RequiresApproval {
			if rule, derr := injectionRuleFromScope(g.Scope); derr == nil {
				gw.injections = append(gw.injections, runner.InjectionGrant{GrantID: grantID, Rule: rule})
			} else {
				s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.create",
					grantID.String(), "failure", mustJSON(map[string]any{
						"error": "api_key grant scope invalid, injection skipped: " + derr.Error(),
					})))
			}
		}
	}
	return gw, true
}

// augmentGitBrokerGrants maps the run's DECLARED GitHub clone set (legacy run.Repo +
// WorkspaceRepos) to the run's github grant, so the git-broker serves those repos
// even when the github_token grant's scope.repos is an empty template (the common
// case for example policies + direct `--repo`). Entries already keyed from a grant's
// explicit scope.repos (persistRunGrants) win and are left as-is. No-op without a
// github grant — an un-granted github repo stays uncovered and is denied (repo is
// the unit of trust).
func (gw *grantWiring) augmentGitBrokerGrants(legacyRepo string, wsRepos []types.WorkspaceRepo) {
	if gw.firstGitHubGrantID == nil {
		return
	}
	add := func(slug string) {
		if key := gitBrokerKeyFromSlug(slug); key != "" {
			if _, ok := gw.gitGrants[key]; !ok {
				gw.gitGrants[key] = *gw.firstGitHubGrantID
			}
		}
	}
	add(legacyRepo)
	for _, wr := range wsRepos {
		add(wr.Repo)
	}
}

// applySSHLaneWarnings handles the agents/images whose SSH clone lane is absent
// or unverifiable, mutating gw and returning the warnings to surface:
//
// codex-cli has no SSH clone lane (no openssh/corkscrew in the image; its
// agent-run never reads WARDYN_SSH_GRANTS), so an ssh_key grant would sit
// unconsumed and the clone would fail SILENTLY mid-run. Fail loud at create
// instead: drop the wiring and tell the operator on the response + audit log.
// The persisted grant rows stay — they are eligibility records nothing will
// mint, not issued credentials.
//
// BYOI images get claude-code's agent-run, whose SSH lane needs openssh +
// corkscrew in the BASE image — which Wardyn cannot inspect from the control
// plane. Advise softly; the runtime guard in agent-run still fails loud
// in-sandbox if the tools are missing. Extracted verbatim from handleCreateRun.
func (s *Server) applySSHLaneWarnings(ctx context.Context, req createRunRequest, runID uuid.UUID, gw *grantWiring) []string {
	var warnings []string
	if req.Agent == "codex-cli" && len(gw.sshGrants) > 0 {
		hosts := make([]string, 0, len(gw.sshGrants))
		for h := range gw.sshGrants {
			hosts = append(hosts, h)
		}
		slices.Sort(hosts)
		warnings = append(warnings, fmt.Sprintf(
			"codex-cli has no SSH clone lane — dropping ssh_key grant(s) for %s; use an HTTPS/PAT source for this repo, or run it under claude-code",
			strings.Join(hosts, ", ")))
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.ssh.unsupported_agent",
			req.Agent, "failure", mustJSON(map[string]any{"dropped_hosts": hosts})))
		gw.sshGrants = map[string]string{}
		gw.sshEgress = nil
	}
	if req.Image != "" && len(gw.sshGrants) > 0 {
		warnings = append(warnings,
			"this run clones over SSH: your custom image must carry openssh-client + corkscrew, or the clone is skipped (agent-run warns in the run log)")
	}
	return warnings
}

// unionRunEgress widens the RESOLVED spec's egress allowlist from the
// deterministic, operator-trusted sources (never the LLM), auditing each
// addition under its own event so provenance stays per-source:
//
//   - Onboarded-workspace profiles: union each referenced workspace's detected
//     package registries (the operator onboarded + reviewed these workspaces).
//     The deny-list + confinement floor are unaffected.
//   - Site-config SCM hosts: the operator's declared enterprise SCM hosts
//     (GHES / ADO Server — see unionSiteConfigScmHosts), which unlike
//     github.com/dev.azure.com have no built-in egress bundle.
//   - SSH SCM lane: each ssh_key grant's PORT-QUALIFIED (":443") SSH-over-443
//     endpoint, reusing the CONNECT-443 lane and matching ONLY :443 — closing
//     the bare-entry "matches any port" permissiveness for SSH hosts.
//   - ADO SCM lane: a git_pat grant for an Azure DevOps host needs
//     dev.azure.com + *.visualstudio.com reachable (see adoEgressDomains) —
//     nothing else adds these for ADO. Mirrors the SSH lane.
//
// wsRefs is the run's referenced onboarded workspaces, resolved by the caller
// (it also feeds the workspace cred binding + image resolution) — this used to
// resolve them itself. Extracted verbatim from handleCreateRun.
func (s *Server) unionRunEgress(ctx context.Context, runID uuid.UUID, spec *types.RunPolicySpec, gw grantWiring, wsRefs []types.Workspace) {
	if added := unionWorkspaceEgress(spec, wsRefs); len(added) > 0 {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.workspace.egress",
			runID.String(), "success", mustJSON(map[string]any{"added_domains": added})))
	}
	if added := s.unionSiteConfigScmHosts(ctx, spec); len(added) > 0 {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.site_config.egress",
			runID.String(), "success", mustJSON(map[string]any{"added_domains": added})))
	}
	if added := unionAllowedDomains(spec, gw.sshEgress); len(added) > 0 {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.ssh.egress",
			runID.String(), "success", mustJSON(map[string]any{"added_domains": added})))
	}
	if added := unionAllowedDomains(spec, gw.gitPATEgress); len(added) > 0 {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.git_pat.egress",
			runID.String(), "success", mustJSON(map[string]any{"added_domains": added})))
	}
}

// resolveCreateRunImage resolves the sandbox image. Default: the agent
// convention image. Precedence: a BYOI wrap (top) > a request-level
// devcontainer build > the primary onboarded workspace's profile. A BYOI or
// DEVCONTAINER-REPO build failure marks the run FAILED (observable, never a
// 500) and WRITES the 201 response itself (responded=true — the handler must
// stop); a WORKSPACE image build failure is fail-open (keeps the convention
// image), since onboarding is a convenience, not a gate. The resolved image is
// persisted for provenance (best-effort: a failed write must not block dispatch
// — the audit trail still carries build events). Extracted verbatim from
// handleCreateRun; ctx is already detached from client cancellation.
func (s *Server) resolveCreateRunImage(ctx context.Context, w http.ResponseWriter, req createRunRequest, runID uuid.UUID, created types.AgentRun, warnings []string, wsRefs []types.Workspace) (string, bool) {
	image := agentImage(req.Agent, s.cfg.AgentImages)

	// Shared FAILED path for the two explicit build lanes (BYOI + devcontainer):
	// CAS from PENDING so a run a concurrent kill already moved to KILLED is not
	// silently clobbered back to FAILED (was: unconditional write), audit, and
	// answer 201 with the refreshed (FAILED) run + warnings.
	buildFailed := func(auditData map[string]any) {
		s.failAndRevoke(ctx, runID, types.RunPending)
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.build",
			runID.String(), "failure", mustJSON(auditData)))
		created = s.refreshRun(ctx, runID, created)
		writeJSON(w, http.StatusCreated, createRunResponse{AgentRun: created, Warnings: warnings})
	}

	switch {
	case req.Image != "": // validated up front: ImageBuilder is non-nil, not XOR'd
		// The wardyn-byoi/ output tag is also the discriminator dispatch keys the
		// runtime selftest preflight off (a wrapped arbitrary image may still lack a
		// shell or the harness binary).
		outTag := "wardyn-byoi/" + runID.String() + ":latest"
		buildCtx, cancelBuild := context.WithTimeout(ctx, imageBuildTimeout)
		built, berr := s.cfg.ImageBuilder.FinalizeBase(buildCtx, req.Image, outTag)
		cancelBuild()
		if berr != nil {
			buildFailed(map[string]any{"byoi_base": req.Image, "error": berr.Error()})
			return "", true
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
			buildFailed(map[string]any{"devcontainer_repo": req.DevcontainerRepo, "error": berr.Error()})
			return "", true
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
	return image, false
}
