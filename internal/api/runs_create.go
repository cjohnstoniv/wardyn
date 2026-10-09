// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// composerWorkspaceTarget is the in-sandbox path a local-directory workspace
// source is bind-mounted at when its own Target is unset — the agent's working
// dir (matches the New Run wizard). Despite the name (a holdover from the
// deleted AI Run Composer, which first introduced this default), it is the
// general fallback every workspace-source resolution path uses (here and
// workspace_run.go), not a composer-specific concern.
const composerWorkspaceTarget = "/home/agent/work"

// seedRequestWorkspace attaches the workspace named by req.WorkspaceID to the
// RESOLVED spec. Without it the only way to launch against an onboarded
// workspace is to hand-reproduce its exact source string in a policy file — the
// console hides that by synthesizing an inline policy, the CLI and SDK could not.
//
// It walks the workspace's Sources in order, collecting each one's
// mount/repo entry, then PREPENDS the whole batch onto the resolved spec in
// ONE shot — so wsRefs[0] (referencedWorkspaces) stays this workspace even
// when the caller also passed a policy (that first ref is what drives the
// run's model/harness cred binding and its built image), AND this
// workspace's own sources keep their relative order ahead of any
// pre-existing spec entries. Everything downstream (validateWorkspaceSources,
// primaryWorkspacePath, unionRunEgress, resolveWorkspaceImage) then works
// unchanged off the spec, which is why this must run BEFORE the onboarding
// gate rather than beside it.
//
//   - repo source    → collected into spec.WorkspaceRepos; the FIRST repo
//     source also sets req.Repo (run-row label only — the gate + wsRefs read
//     WorkspaceRepos).
//   - local_dir source → collected into spec.WorkspaceMounts (read-only
//     unless the source itself is Writable).
//   - ephemeral source → NO policy entry (there is no host/repo source for
//     one) — its target is returned in ephemeralDirs for the caller to surface
//     as WARDYN_EPHEMERAL_DIRS at dispatch (runs_dispatch.go); the sandbox
//     mkdirs it.
//
// A COMPOSED launch never sets req.WorkspaceID, so this function does not run
// for one — compose.go's applyWorkspaces seeds the SAME mount/repo entries
// directly from the proposal's workspace descriptor instead (a different route
// to the identical spec.WorkspaceMounts/WorkspaceRepos shape). That parallel
// route is deliberately narrower, not an oversight: it never resolves a stored
// types.Workspace record, so it cannot mirror this function's base_image ->
// req.Image step or its ephemeral-target -> WARDYN_EPHEMERAL_DIRS step. Nor can
// this function simply be re-run afterward to cover the gap (e.g. by deriving a
// workspace_id for the primary composed selection): it would PREPEND the same
// sources a second time — harmless for a git repo with no explicit source
// target (buildRepoRecords dedupes on the shared default clone dest) but a
// genuine double-clone for one WITH an explicit target, and a hard 422 for a
// local_dir source (the unique-target invariant re-checked below would then see
// the identical target twice). A composed run's workspace's base_image and
// ephemeral source targets are therefore a known, currently-unclosed gap, not
// something this function covers.
//
// base_image REPLACES the old container-kind image resolution: when the
// workspace carries one (and the caller didn't already set an explicit
// --image), it sets req.Image — mirroring exactly what a user passing
// --image <that ref> would have done, so resolveCreateRunImage's ordinary
// BYOI-wrap path picks it up. "recommended" is not a fixed ref (it just means
// "no override"), so it never sets req.Image.
//
// It deliberately does NOT set run.WorkspaceID: that column is the TRUSTED
// run→workspace linkage the scan/verify/record uploads authorize on, so a user
// run must never claim it.
//
// seededImageOwner is the ownership half of seededImageRefusal's fix: the
// OwnedBy of the workspace whose base_image just set req.Image, and "" in
// every other case — including a member-owned workspace that set no image and an
// operator-owned one that did. The callers' capability re-check keys on exactly
// that emptiness, so returning the owner unconditionally would turn an
// ownership-scoped guard into the unconditional variant seededImageRefusal
// exists to prevent — a catastrophic regression.
// #656 M1: five distinct causes used to share one reason (reasonWorkspaceSeedFailed);
// each return below now names its own, since a caller who gets one back cannot
// otherwise tell "no store" from "no base image" from "conflicts with the policy".
func (s *Server) seedRequestWorkspace(ctx context.Context, spec *types.RunPolicySpec, req *createRunRequest) (ephemeralDirs []string, seededImageOwner string, code int, reason string, err error) {
	if req.WorkspaceID == nil {
		return nil, "", 0, "", nil
	}
	if s.cfg.Store == nil {
		return nil, "", http.StatusUnprocessableEntity, reasonWorkspaceSeedStoreUnavailable,
			fmt.Errorf("workspace_id requires a store, but none is configured")
	}
	ws, gerr := s.cfg.Store.GetWorkspace(ctx, *req.WorkspaceID)
	if gerr != nil {
		return nil, "", http.StatusUnprocessableEntity, reasonWorkspaceSeedUnreadable,
			fmt.Errorf("workspace %s: %w", *req.WorkspaceID, gerr)
	}
	var newMounts []types.WorkspaceMount
	var newRepos []types.WorkspaceRepo
	for _, src := range ws.Sources {
		// The stored target becomes an in-container mount/clone/scratch target
		// here, so re-validate it against the same deny-list
		// runner.ValidateAuthoredTarget applies at onboarding — a row written before that check existed must not
		// ride straight past the gate onto a system path (e.g. /home/agent/.claude).
		target := src.Target
		if target != "" {
			if verr := runner.ValidateAuthoredTarget(target); verr != nil {
				return nil, "", http.StatusUnprocessableEntity, reasonWorkspaceSeedSourceTargetInvalid,
					fmt.Errorf("workspace %s source target: %w", ws.ID, verr)
			}
		}
		switch src.Type {
		case types.WorkspaceSourceTypeRepo:
			newRepos = append(newRepos, types.WorkspaceRepo{Repo: src.Source, Target: target, Ref: src.Ref})
		case types.WorkspaceSourceTypeLocalDir:
			// Read-only unless the operator ticked Writable on this source — the
			// same per-source opt-in wireWorkspaceSource honors for import runs.
			ro := !src.Writable
			if target == "" {
				target = composerWorkspaceTarget
			}
			newMounts = append(newMounts, types.WorkspaceMount{Source: src.Path, Target: target, ReadOnly: &ro})
		case types.WorkspaceSourceTypeEphemeral:
			// No policy entry — it's a mkdir inside the sandbox, not a mount/clone.
			// ponytail: a plain directory has no size cap; a tmpfs mount (with a
			// size limit) is the upgrade path if an unbounded scratch dir ever
			// needs one.
			if target != "" {
				ephemeralDirs = append(ephemeralDirs, target)
			}
		}
	}
	spec.WorkspaceMounts = append(newMounts, spec.WorkspaceMounts...)
	spec.WorkspaceRepos = append(newRepos, spec.WorkspaceRepos...)
	if req.Repo == "" && len(newRepos) > 0 {
		req.Repo = newRepos[0].Repo // run-row label only; the gate + wsRefs read WorkspaceRepos
	}
	// base_image replaces the old container-kind image resolution. An explicit
	// user --image always wins (never silently overridden); "recommended"
	// just means "no override", so only a real build choice sets req.Image.
	if req.Image == "" && ws.BaseImage != nil && ws.BaseImage.Kind != "recommended" {
		req.Image = ws.BaseImage.Image
		seededImageOwner = ws.OwnedBy
	}
	// agentRequirementError deferred exactly this question to here: an exec
	// run naming a workspace but no agent needed SOME image to run the command
	// in, and now that base_image has had its one chance to supply req.Image,
	// still having neither is the caller's request, resolved. Never "attach a
	// workspace" — one already is attached; what it lacks is a base image.
	if req.TaskMode == "exec" && req.Agent == "" && req.Image == "" {
		return nil, "", http.StatusBadRequest, reasonWorkspaceSeedNoBaseImage, fmt.Errorf(
			"workspace %s has no base image to run a command in: pass --image or --agent", ws.ID)
	}
	// The seed mutated an ALREADY-validated spec, so re-run the one invariant it
	// can break: the unique in-container target across workspace_mounts +
	// workspace_repos (policy.go). Without this, a workspace whose target
	// collides with a policy mount reaches the driver as a duplicate mount point
	// (opaque dispatch failure) — or a repo clone targets the EXACT path of a
	// writable bind. Exact-target only, like the invariant itself: a repo target
	// nested INSIDE a writable mount still clones into the host bind — permitted,
	// since clone-into-mounted-workspace is how the legacy default dest
	// ~/work/<name> already behaves when ~/work is a mounted dir.
	if verr := validatePolicyWorkspaces(*spec); verr != nil {
		return nil, "", http.StatusUnprocessableEntity, reasonWorkspaceSeedPolicyConflict,
			fmt.Errorf("workspace %s conflicts with the policy: %w", ws.ID, verr)
	}
	return ephemeralDirs, seededImageOwner, 0, "", nil
}

// authorizeSpecWorkspaceSources is the RESOLVED-SPEC half of the onboarding
// gate: every onboarded workspace the spec's mount sources and repos resolve to must be one
// the caller may launch against (mayLaunchWorkspace).
//
// The workspace_id door is authorized by workspaceLaunchSelection before any
// source is folded, but a source can also reach the spec WITHOUT naming an id —
// a hand-authored inline or stored policy naming the host path directly. That
// second door landed in the same room: validateWorkspaceSources admits the
// source because it IS onboarded, and userMountAllowed then re-checks it
// against the OWNING member's roots, so a per-principal root map constrains the
// caller not at all. This closes it at the same chokepoint the onboarding gate
// uses, over the RESOLVED spec, so no authoring surface can route around it.
//
// The refusal is BYTE-IDENTICAL to validateWorkspaceSources' not-onboarded
// refusal, deliberately: a distinct "another member owns this" sentence would be
// the cross-member existence oracle denyForeignWorkspace exists to close, told
// about a host path instead of an id. From the caller's side another member's
// workspace simply is not onboarded.
//
// Runs AFTER validateWorkspaceSources (which has already refused every
// un-onboarded source), so the index lookups below always find the owning
// workspace.
func (s *Server) authorizeSpecWorkspaceSources(ctx context.Context, r *http.Request, spec types.RunPolicySpec) (int, string, error) {
	if s.cfg.Store == nil || (len(spec.WorkspaceMounts) == 0 && len(spec.WorkspaceRepos) == 0) {
		return 0, "", nil
	}
	all, err := s.cfg.Store.ListWorkspaces(ctx)
	if err != nil {
		return http.StatusUnprocessableEntity, reasonWorkspaceSourcesListUnavailable, fmt.Errorf("list workspaces: %w", err)
	}
	idx := indexWorkspacesBySource(all)
	for _, wm := range spec.WorkspaceMounts {
		if ws, ok := idx.localDir[wm.Source]; ok && !s.mayLaunchWorkspace(r, ws) {
			return http.StatusUnprocessableEntity, reasonWorkspaceSourceNotOnboarded, fmt.Errorf(
				"mount source %q is not an onboarded local directory (onboard it first via the workspaces API)", wm.Source)
		}
	}
	for _, wr := range spec.WorkspaceRepos {
		if ws, ok := idx.repo[wr.Repo]; ok && !s.mayLaunchWorkspace(r, ws) {
			return http.StatusUnprocessableEntity, reasonWorkspaceSourceNotOnboarded, fmt.Errorf(
				"repo %q is not an onboarded repository (onboard it first via the workspaces API)", wr.Repo)
		}
	}
	return 0, "", nil
}

// unionRunEgress widens the RESOLVED spec's egress allowlist from the
// deterministic, operator-trusted sources (never the LLM), auditing each
// addition under its own event so provenance stays per-source:
//
//   - Onboarded-workspace profiles: union each referenced workspace's detected
//     package registries (the operator onboarded + reviewed these workspaces)
//     AND each workspace's permanent DeniedEgress into spec.DeniedDomains
//     (Phase 4, unionWorkspaceEgress) — the confinement floor alone is
//     unaffected now; the deny-list is deliberately not, or a workspace's
//     `deny · always` decision would never reach a real run.
//   - Site-config SCM hosts: the operator's declared enterprise SCM hosts
//     (GHES / ADO Server — see unionSiteConfigScmHosts), which unlike
//     github.com/dev.azure.com have no built-in egress bundle.
//   - SSH SCM lane: each ssh_key grant's PORT-QUALIFIED (":443") SSH-over-443
//     endpoint, reusing the CONNECT-443 lane and matching ONLY :443 — closing
//     the bare-entry "matches any port" permissiveness for SSH hosts.
//   - ADO SCM lane: a git_pat grant for an Azure DevOps host needs
//     dev.azure.com + *.visualstudio.com reachable (see adoEgressDomains) —
//     nothing else adds these for ADO. Mirrors the SSH lane.
//   - Components: nothing is unioned here — the component gate did, before
//     the run was graded (applyRunComponents) — but each one's addition is
//     audited here with the rest, under kind `component`.
//
// wsRefs is the run's referenced onboarded workspaces, resolved by the caller
// (it also feeds the workspace cred binding + image resolution). legacyRepo is
// the request's single `repo` field, which the declaresRepo gate below needs
// and grantWiring cannot supply. scmSite is the site-config snapshot the
// autonomy gate graded the SCM-host lane from (resolveRunAutonomy), so the
// hosts dispatched here are the hosts that were graded. Extracted verbatim
// from handleCreateRun.
func (s *Server) unionRunEgress(ctx context.Context, runID uuid.UUID, spec *types.RunPolicySpec, gw grantWiring, wsRefs []types.Workspace, legacyRepo string,
	scmSite types.SiteConfig, directGitHubAdded []string, comps runComponents,
) {
	// Direct GitHub candidates were bounded before grading; audit them only now
	// that a run exists, alongside the dispatch-only additions.
	auditAdded := func(kind string, added []string) {
		if len(added) > 0 {
			s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.egress.add",
				runID.String(), "success", mustJSON(map[string]any{"kind": kind, "added_domains": added})))
		}
	}
	auditAdded("github_direct", directGitHubAdded)
	// A component's hosts were bounded and unioned by the gate, before grading,
	// like the direct GitHub candidates; the rows name what each one added.
	for _, data := range comps.egressAudit() {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.egress.add",
			runID.String(), "success", mustJSON(data)))
	}
	auditAdded("workspace", unionWorkspaceEgress(spec, wsRefs))
	// Repo clone host(s) each referenced workspace needs: a
	// non-GitHub HTTPS clone (GitLab, self-hosted git) reaches its forge as an
	// ordinary egress host, and nothing else in this union adds it — so a real run
	// of a workspace whose only access is anonymous read got NO clone host and the
	// clone was proxy-denied, even though the confined Verify (which unions
	// workspaceCloneEgress via confinedEgressDomains) proved it. Direct GitHub
	// candidates were bounded before grading; brokered clones need no host. This makes
	// promoteSkipHosts' "wired into every scan/verify for free" comment true for a
	// real run too.
	var cloneAdded []string
	for _, ws := range wsRefs {
		cloneAdded = append(cloneAdded, unionAllowedDomains(spec, workspaceCloneEgress(ws))...)
	}
	auditAdded("workspace_clone", cloneAdded)
	// Site-config SCM hosts (GHES / ADO Server) are a CLONE lane, so union them
	// only when this run actually declares a repo: a sealed
	// local-dir-only analysis run must not inherit an unauthenticated HTTPS lane to
	// every internal SCM host the operator declared for GHES clone runs. A repo is
	// declared via spec.WorkspaceRepos, the legacy single `repo` field, or any
	// git-clone grant lane.
	//
	// legacyRepo is checked DIRECTLY, not via gw.gitGrants: that fold is a
	// no-op when gw.firstGitHubGrantID == nil, i.e. whenever the run has no
	// github grant, and repoCloneURL (runs_scm.go) accepts a full https:// URL, so
	// `--repo https://ghes.corp.example/team/app` is an
	// ordinary credential-free HTTPS clone whose ONLY egress source is this
	// union — without checking legacyRepo directly, declaresRepo would read false
	// and the proxy would deny the clone. The gate's actual intent (a sealed
	// local-dir-only run inherits no SCM lane) is unchanged: no repo, no union.
	declaresRepo := len(spec.WorkspaceRepos) > 0 || strings.TrimSpace(legacyRepo) != "" ||
		gw.firstGitHubGrantID != nil ||
		len(gw.gitGrants) > 0 || len(gw.gitPATGrants) > 0 || len(gw.sshGrants) > 0
	if declaresRepo {
		auditAdded("site_config", unionSiteConfigScmHosts(spec, scmSite))
	}
	auditAdded("ssh", unionAllowedDomains(spec, gw.sshEgress))
	auditAdded("git_pat", unionAllowedDomains(spec, gw.gitPATEgress))
}

// warnCeilingDeniedWorkspaceEgress names, at CREATE time, every host an operator
// APPROVED for a referenced workspace that the caller's own governance ceiling
// denies.
//
// Both halves are legitimate and neither yields: unionWorkspaceEgress unions the
// approval into the run's allowlist because an operator vetted that host for that
// workspace, and the ceiling's deny beats every allow at the proxy. The run
// launches and that one host is refused mid-run — which, without this, is a
// support ticket rather than a sentence on the 201.
//
// Warn, never refuse (the same asymmetry the profile CRUD's
// omission warnings take): the member did not author the workspace, cannot edit
// the ceiling, and has nothing to correct — refusing would make an admin's two
// independent decisions into a launch failure the member cannot resolve.
//
// Scoped to an ASSIGNED profile: the deployment ceiling's own denies are what
// unionWorkspaceEgress has always run against, so warning about them would fire
// on every run of every workspace on upgrade day and say nothing new.
//
// The verdict comes from ceilingDenies (runs_dispatch_ceiling.go) — the SAME
// predicate the dispatch re-assertion decides credential lanes with, so the
// sentence this warning promises a member and the enforcement they actually get
// cannot drift apart. A second matcher here (a string compare, or a
// freshly-compiled proxy policy) would disagree on exactly the entries that
// matter: a wildcard, or a port qualifier.
//
// The ceiling is the one decodeAndValidateCreateRun already resolved for this
// request, threaded rather than re-read: a warning composed against a DIFFERENT
// ceiling than the run was gated under would name a profile that is not the one
// bounding the run.
func warnCeilingDeniedWorkspaceEgress(ceiling governanceCeiling, wsRefs []types.Workspace) []string {
	if len(wsRefs) == 0 || ceiling.Profile == nil || len(ceiling.Spec.DeniedDomains) == 0 {
		return nil
	}
	var warns []string
	seen := map[string]bool{}
	for _, ws := range wsRefs {
		for _, host := range ws.ApprovedEgress {
			h := strings.ToLower(strings.TrimSpace(host))
			if h == "" || seen[h] {
				continue
			}
			seen[h] = true
			if !ceilingDenies(ceiling.Spec.DeniedDomains, h) {
				continue
			}
			warns = append(warns, fmt.Sprintf(
				"workspace host %q is denied by your governance profile %q — the run launches, but that host is refused at the proxy",
				host, ceiling.Profile.Name))
		}
	}
	return warns
}

// resolveCreateRunImage resolves the sandbox image. Default: the agent
// convention image. Precedence: a BYOI wrap (top) > a request-level
// devcontainer build > the primary onboarded workspace's profile. A BYOI or
// DEVCONTAINER-REPO build failure marks the run FAILED (observable, never a
// 500) and returns failed=true — the caller (one frame up, off-request-capable)
// owns refreshing the run and answering the response; a WORKSPACE image build
// failure is fail-open (keeps the convention image), since onboarding is a
// convenience, not a gate. The resolved image is persisted for provenance
// (best-effort: a failed write must not block dispatch — the audit trail
// still carries build events). Extracted verbatim from handleCreateRun; ctx is
// already detached from client cancellation.
func (s *Server) resolveCreateRunImage(ctx context.Context, req createRunRequest, runID uuid.UUID, wsRefs []types.Workspace) (string, bool) {
	image := agentImage(req.Agent, s.cfg.AgentImages)

	// Shared FAILED path for the two explicit build lanes (BYOI + devcontainer):
	// CAS from PENDING so a run a concurrent kill already moved to KILLED is not
	// silently clobbered back to FAILED (was: unconditional write), and audit.
	buildFailed := func(auditData map[string]any) {
		// Surface the build failure under the FAILED badge, not only in the
		// run.build audit row. The error text is already in auditData["error"].
		hint := "the run's sandbox image could not be built"
		if e, ok := auditData["error"].(string); ok && e != "" {
			hint += ": " + e
		}
		s.failAndRevoke(ctx, runID, types.RunPending, hint)
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.build",
			runID.String(), "failure", mustJSON(auditData)))
	}

	switch {
	case req.Image != "": // validated up front: ImageBuilder is non-nil, not XOR'd
		// The wardyn-byoi/ output tag is also the discriminator dispatch keys the
		// runtime selftest preflight off (a wrapped arbitrary image may still lack a
		// shell or the harness binary).
		outTag := "wardyn-byoi/" + runID.String() + ":latest"
		buildCtx, cancelBuild := context.WithTimeout(ctx, imageBuildTimeout)
		s.announceImageBuild(ctx, runID)()
		built, berr := s.cfg.ImageBuilder.FinalizeBase(buildCtx, req.Image, outTag, nil)
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
		s.announceImageBuild(ctx, runID)()
		built, berr := s.cfg.ImageBuilder.BuildDevcontainer(buildCtx, req.DevcontainerRepo, req.DevcontainerRef, outTag, nil)
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
		// A base_image workspace reached here via mounts/repos — a composed
		// or UI run that did NOT send workspace_id (the workspace_id door sets req.Image
		// and takes the case above). If it declares an explicit base_image CHOICE but no
		// builder is wired, FAIL the run the same way the workspace_id door 400s
		// (validateImageBuildRequest), rather than silently launching on the convention
		// image and dropping the operator's chosen base image (the door divergence this
		// creates). resolveWorkspaceImage is fail-open by design (shared with the
		// wizard/record paths), so the fail-closed decision for the CHOSEN base image
		// lives here, on the create door, not in it.
		if b := wsRefs[0].BaseImage; b != nil && b.Kind != "recommended" &&
			strings.TrimSpace(b.Image) != "" && s.cfg.ImageBuilder == nil {
			buildFailed(map[string]any{
				"base_image": b.Image,
				"error": "no image builder is wired, so the workspace's chosen base image cannot be wrapped with the " +
					"agent runtime; refusing to silently substitute the convention image (wire an image builder or drop the base image)",
			})
			return "", true
		}
		buildCtx, cancelBuild := context.WithTimeout(ctx, imageBuildTimeout)
		if built, ok := s.resolveWorkspaceImage(buildCtx, runID, wsRefs[0], nil, s.announceImageBuild(ctx, runID)); ok {
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
