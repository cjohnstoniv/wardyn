// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// composerWorkspaceTarget is the in-sandbox path a local-directory workspace
// source is bind-mounted at when its own Target is unset — the agent's working
// dir (matches the New Run wizard). Despite the name (a holdover from the
// deleted AI Run Composer, which first introduced this default), it is the
// general fallback every workspace-source resolution path uses (here and
// workspace_run.go), not a composer-specific concern.
const composerWorkspaceTarget = "/home/agent/work"

// seedRequestWorkspace attaches every workspace the request names
// (attachedWorkspaceIDs: workspaces[] in order, else workspace_id) to the
// RESOLVED spec. Without it the only way to launch against an onboarded
// workspace is to hand-reproduce its exact source string in a policy file — the
// console hides that by synthesizing an inline policy, the CLI and SDK could not.
//
// It walks each workspace's Sources in order, collecting each one's
// mount/repo entry, then PREPENDS the whole batch onto the resolved spec in
// ONE shot — so the primary (workspaces[0]) leads wsRefs even
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
// Per workspace, the selection's Repos picks which repo sources clone and at
// which ref (an unknown repo is 422 workspace_repo_unknown), and its Target
// replaces the one repository or folder it then attaches. A workspaces[] entry
// the resolved policy already attaches adds no sources a second time; a target
// or repos on it is 422, since the policy sets its paths. When the caller named no
// image, every attached workspace must agree on a base image (422
// image_conflict; stepWorkspaces holds the policy's to it too, after the
// authorization gate); "recommended" is no choice.
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
// seed.imageOwner is the ownership half of seededImageRefusal's fix: the
// OwnedBy of a member-owned workspace whose base_image just set req.Image, and "" in
// every other case — including a member-owned workspace that set no image and an
// operator-owned one that did. The callers' capability re-check keys on exactly
// that emptiness, so returning the owner unconditionally would turn an
// ownership-scoped guard into the unconditional variant seededImageRefusal
// exists to prevent — a catastrophic regression.
// #656 M1: five distinct causes used to share one reason (reasonWorkspaceSeedFailed);
// each return below now names its own, since a caller who gets one back cannot
// otherwise tell "no store" from "no base image" from "conflicts with the policy".
func (s *Server) seedRequestWorkspace(ctx context.Context, spec *types.RunPolicySpec, req *createRunRequest) (seed workspaceSeed, refusal *runRefusal) {
	// The older refusals keep their "workspace_id: " prefix; the multi-workspace
	// ones are New Run sentences and go out verbatim.
	seedErr := func(code int, reason string, err error) *runRefusal {
		return runError(code, reason, "workspace_id: "+err.Error())
	}
	ids := attachedWorkspaceIDs(*req)
	if len(ids) == 0 {
		return workspaceSeed{}, nil
	}
	if s.cfg.Store == nil {
		return workspaceSeed{}, seedErr(http.StatusUnprocessableEntity, reasonWorkspaceSeedStoreUnavailable, fmt.Errorf("workspace_id requires a store, but none is configured"))
	}
	all, lerr := s.cfg.Store.ListWorkspaces(ctx)
	if lerr != nil {
		return workspaceSeed{}, seedErr(http.StatusUnprocessableEntity, reasonWorkspaceSeedUnreadable, fmt.Errorf("list workspaces: %w", lerr))
	}
	byPolicy := referencedWorkspacesIn(all, *spec)
	sels := resolveWorkspaceSelections(*req)
	var newMounts []types.WorkspaceMount
	var newRepos []types.WorkspaceRepo
	for _, id := range ids {
		ws, gerr := s.cfg.Store.GetWorkspace(ctx, id)
		if gerr != nil {
			return workspaceSeed{}, seedErr(http.StatusUnprocessableEntity, reasonWorkspaceSeedUnreadable, fmt.Errorf("workspace %s: %w", id, gerr))
		}
		seed.attached = append(seed.attached, ws)
		sel := sels[id.String()]
		// workspace_id keeps its older rule and seeds every source; a workspaces[]
		// entry the policy already attaches only adds its options (the console's
		// custom policy carries the sources itself).
		if (req.WorkspaceID == nil || *req.WorkspaceID != id) && slices.ContainsFunc(byPolicy, func(p types.Workspace) bool { return p.ID == id }) {
			if sel.Target != "" || sel.Repos != nil {
				return workspaceSeed{}, seedErr(http.StatusUnprocessableEntity, reasonWorkspaceSeedPolicyConflict, fmt.Errorf(
					"%s is attached by the run's policy, which sets its paths and repositories: drop target and repos", ws.Name))
			}
			continue
		}
		mounts, repos, dirs, refusal := seedWorkspaceSources(ws, sel)
		if refusal != nil {
			return workspaceSeed{}, refusal
		}
		newMounts, newRepos = append(newMounts, mounts...), append(newRepos, repos...)
		for range mounts {
			seed.mountOwners = append(seed.mountOwners, ws)
		}
		for range repos {
			seed.repoOwners = append(seed.repoOwners, ws)
		}
		seed.ephemeralDirs = append(seed.ephemeralDirs, dirs...)
	}
	spec.WorkspaceMounts = append(newMounts, spec.WorkspaceMounts...)
	spec.WorkspaceRepos = append(newRepos, spec.WorkspaceRepos...)
	if req.Repo == "" && len(newRepos) > 0 {
		req.Repo = newRepos[0].Repo // run-row label only; the gate + wsRefs read WorkspaceRepos
	}
	// base_image replaces the old container-kind image resolution. An explicit
	// user --image always wins (never silently overridden); "recommended"
	// just means "no override", so only a real build choice sets req.Image.
	//
	// Only the attached workspaces are compared here: each passed the caller's
	// launch check above, while a workspace the policy names has not been held
	// to it yet (authorizeSpecWorkspaceSources), so naming it in a conflict
	// would tell a member another member's workspace exists. The policy's are
	// compared in stepWorkspaces, after that gate (imageOpen).
	if req.Image == "" {
		seed.imageOpen = true
		image, owner, refusal := workspacesBaseImage(seed.attached, seed.attached)
		if refusal != nil {
			return workspaceSeed{}, refusal
		}
		req.Image, seed.imageOwner = image, owner
	}
	// agentRequirementError deferred exactly this question to here: an exec
	// run naming a workspace but no agent needed SOME image to run the command
	// in, and now that base_image has had its one chance to supply req.Image,
	// still having neither is the caller's request, resolved. Never "attach a
	// workspace" — one already is attached; what it lacks is a base image.
	if req.TaskMode == "exec" && req.Agent == "" && req.Image == "" {
		return workspaceSeed{}, seedErr(http.StatusBadRequest, reasonWorkspaceSeedNoBaseImage, fmt.Errorf(
			"workspace %s has no base image to run a command in: pass --image or --agent", ids[0]))
	}
	// The seed mutated an ALREADY-validated spec, so re-run the one invariant it
	// can break: the unique in-container target across workspace_mounts +
	// workspace_repos (policy.go). Without this, a workspace whose target
	// collides with a policy mount reaches the driver as a duplicate mount point
	// (opaque dispatch failure) — or a repo clone targets the EXACT path of a
	// writable bind. Equal or nested targets across workspaces, defaults
	// included, are the overlap check's, over every resolved target.
	if verr := validatePolicyWorkspaces(*spec); verr != nil {
		return workspaceSeed{}, seedErr(http.StatusUnprocessableEntity, reasonWorkspaceSeedPolicyConflict, fmt.Errorf("workspace %s conflicts with the policy: %w", ids[0], verr))
	}
	return seed, nil
}

// workspaceSeed is what seeding the request's workspaces decided.
type workspaceSeed struct {
	// attached are the workspaces the request names, in order: [0] is the primary.
	attached []types.Workspace
	// mountOwners and repoOwners are the workspace that seeded each entry the
	// seed prepended to spec.WorkspaceMounts and WorkspaceRepos, in order, so a
	// source two attached workspaces carry is attributed to the right one.
	mountOwners, repoOwners []types.Workspace
	ephemeralDirs           []string
	imageOwner              string
	// imageOpen: the request named no image, so the workspaces decide it.
	imageOpen bool
}

// attachedWorkspaceIDs is the request's attach list: workspaces[] in order, else
// the workspace_id alias. workspaceTargetsRefusal already held each id to a
// uuid, once, and workspace_id to workspaces[0].
func attachedWorkspaceIDs(req createRunRequest) []uuid.UUID {
	if len(req.Workspaces) == 0 {
		if req.WorkspaceID == nil {
			return nil
		}
		return []uuid.UUID{*req.WorkspaceID}
	}
	ids := make([]uuid.UUID, 0, len(req.Workspaces))
	for _, sel := range req.Workspaces {
		if id, err := uuid.Parse(sel.WorkspaceID); err == nil {
			ids = append(ids, id)
		}
	}
	return ids
}

// seedWorkspaceSources is one workspace's mount, repo and scratch entries under
// its selection: the repos it picks at their refs, and its target on the one
// repository or folder left.
func seedWorkspaceSources(ws types.Workspace, sel client.WorkspaceSelection) (
	mounts []types.WorkspaceMount, repos []types.WorkspaceRepo, ephemeralDirs []string, refusal *runRefusal,
) {
	var picked map[string]string // repo -> ref; nil keeps every repo source
	if sel.Repos != nil {
		picked = make(map[string]string, len(*sel.Repos))
		for _, rs := range *sel.Repos {
			if !slices.ContainsFunc(workspaceSourcesOfType(ws, types.WorkspaceSourceTypeRepo), func(src types.WorkspaceSource) bool { return src.Source == rs.Repo }) {
				return nil, nil, nil, runError(http.StatusUnprocessableEntity, reasonWorkspaceRepoUnknown, workspaceRepoUnknownMsg(rs.Repo, ws.Name))
			}
			picked[rs.Repo] = rs.Ref
		}
	}
	for _, src := range ws.Sources {
		// The stored target becomes an in-container mount/clone/scratch target
		// here, so re-validate it against the same deny-list
		// runner.ValidateAuthoredTarget applies at onboarding — a row written before that check existed must not
		// ride straight past the gate onto a system path (e.g. /home/agent/.claude).
		target := src.Target
		if target != "" {
			if verr := runner.ValidateAuthoredTarget(target); verr != nil {
				return nil, nil, nil, runError(http.StatusUnprocessableEntity, reasonWorkspaceSeedSourceTargetInvalid,
					fmt.Sprintf("workspace_id: workspace %s source target: %v", ws.ID, verr))
			}
			// A repo's target also rides the clone record, which a space cannot.
			if src.Type == types.WorkspaceSourceTypeRepo && !repoFieldSafe(target) {
				return nil, nil, nil, runError(http.StatusUnprocessableEntity, reasonWorkspaceSeedSourceTargetInvalid,
					fmt.Sprintf("workspace_id: workspace %s source target: "+repoField400Charset, ws.ID, "target"))
			}
		}
		switch src.Type {
		case types.WorkspaceSourceTypeRepo:
			ref, ok := src.Ref, true
			if picked != nil {
				var runRef string
				if runRef, ok = picked[src.Source]; ok {
					ref = cmp.Or(runRef, src.Ref)
				}
			}
			if ok {
				repos = append(repos, types.WorkspaceRepo{Repo: src.Source, Target: target, Ref: ref})
			}
		case types.WorkspaceSourceTypeLocalDir:
			// Read-only unless the operator ticked Writable on this source — the
			// same per-source opt-in wireWorkspaceSource honors for import runs.
			ro := !src.Writable
			if target == "" {
				target = composerWorkspaceTarget
			}
			mounts = append(mounts, types.WorkspaceMount{Source: src.Path, Target: target, ReadOnly: &ro})
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
	if sel.Target != "" {
		switch {
		case len(mounts) == 1 && len(repos) == 0:
			mounts[0].Target = sel.Target
		case len(repos) == 1 && len(mounts) == 0:
			repos[0].Target = sel.Target
		default:
			return nil, nil, nil, runError(http.StatusUnprocessableEntity, reasonWorkspaceTargetInvalid, fmt.Sprintf(
				"%s attaches %d repositories and folders, and a target names one place: pick one with repos, or leave target empty.",
				ws.Name, len(mounts)+len(repos)))
		}
	}
	return mounts, repos, ephemeralDirs, nil
}

// workspacesBaseImage is the one base image workspaces all agree on:
// "recommended" is no choice, and two different real choices are 422
// image_conflict. Every workspace in all must already be one the caller may
// launch, since the refusal names them. It returns the image only when an
// ATTACHED workspace carries it (a policy's alone is resolveCreateRunImage's,
// as before), and owner is the first member owner among those, so
// seededImageRefusal re-checks the image whenever a member chose it.
func workspacesBaseImage(all, attached []types.Workspace) (image, owner string, refusal *runRefusal) {
	var from string
	for _, ws := range all {
		switch b := workspaceBaseImage(ws); {
		case b == "":
		case image == "":
			image, from = b, ws.Name
		case b != image:
			return "", "", runError(http.StatusUnprocessableEntity, reasonImageConflict, imageConflictMsg(from, ws.Name))
		}
	}
	carried := false
	for _, ws := range attached {
		if image != "" && workspaceBaseImage(ws) == image {
			carried = true
			if owner == "" {
				owner = ws.OwnedBy
			}
		}
	}
	if !carried {
		return "", "", nil
	}
	return image, owner, nil
}

// workspaceBaseImage is a workspace's real base-image choice, "" for none or
// "recommended".
func workspaceBaseImage(ws types.Workspace) string {
	if b := ws.BaseImage; b != nil && b.Kind != "recommended" {
		return strings.TrimSpace(b.Image)
	}
	return ""
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
// the request's single `repo` field, which deriveRunEgress's repository test
// needs and grantWiring cannot supply. scmSite is the site-config snapshot the
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
	// The dispatch-only lanes come from deriveRunEgress, the function the dry
	// doors' stepDerivedEgress calls, so Review names the hosts launched here.
	for _, d := range deriveRunEgress(spec, wsRefs, legacyRepo, scmSite, wiredLaneGrants(spec.EligibleGrants, gw)) {
		auditAdded(d.kind, d.added)
	}
}

// wiredLaneGrants is grants without the ones whose lane persistRunGrants or
// applySSHLaneWarnings declined to wire: a vetoed lane keeps its grant row (an
// eligibility record nothing mints) but must add no host and declare no repo.
// A github_token counts while any github lane is wired, which is all the
// repository test asks of it.
func wiredLaneGrants(grants []types.GrantSpec, gw grantWiring) []types.GrantSpec {
	return slices.DeleteFunc(slices.Clone(grants), func(g types.GrantSpec) bool {
		switch g.Kind {
		case types.GrantGitHubToken:
			return gw.firstGitHubGrantID == nil && len(gw.gitGrants) == 0
		case types.GrantGitPAT:
			sc, err := types.DecodeGitPATScope(g.Scope)
			return err != nil || gw.gitPATGrants[sc.Host] == ""
		case types.GrantSSHKey:
			host, _, _, _, err := sshKeyScopeFields(g.Scope)
			return err != nil || gw.sshGrants[host] == ""
		}
		return false
	})
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
// devcontainer build > the onboarded workspace that chose a base image, else
// the primary's profile. A BYOI or
// DEVCONTAINER-REPO build failure marks the run FAILED (observable, never a
// 500) and returns failed=true — the caller (one frame up, off-request-capable)
// owns refreshing the run and answering the response; a WORKSPACE image build
// failure is fail-open (keeps the convention image), since onboarding is a
// convenience, not a gate. Each build is one a kill of the run stops
// (trackRunBuild); a run already terminal builds nothing and also returns
// failed=true, with its state left as it is. The resolved image is persisted for provenance
// (best-effort: a failed write must not block dispatch — the audit trail
// still carries build events). Extracted verbatim from handleCreateRun; ctx is
// already detached from client cancellation.
func (s *Server) resolveCreateRunImage(ctx context.Context, req createRunRequest, runID uuid.UUID, wsRefs []types.Workspace) (string, bool) {
	image := agentImage(req.Agent, s.cfg.AgentImages)
	if req.UsesRunMode() {
		// The set's image (the fold already refused a set no image carries), so a
		// run with no harness starts from the agent base image. The cases below
		// keep their precedence over it.
		fact, _ := harnessSetImage(harnessSet(req), s.cfg.AgentImages, customImageRequested(req, wsRefs))
		image = fact.Ref
	}

	// Shared FAILED path for the two explicit build lanes (BYOI + devcontainer):
	// CAS from PENDING so a run a concurrent kill already moved to KILLED is not
	// silently clobbered back to FAILED (was: unconditional write), and audit.
	buildFailed := func(auditData map[string]any) {
		// Bounded, and not on the build's context: a kill that stopped the build does not stop this.
		ctx, cancel := context.WithTimeout(ctx, killCascadeTimeout)
		defer cancel()
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
		buildCtx, endBuild, live := s.trackRunBuild(ctx, runID)
		if !live {
			return "", true
		}
		s.announceImageBuild(ctx, runID)()
		built, berr := s.cfg.ImageBuilder.FinalizeBase(buildCtx, req.Image, outTag, nil, false)
		endBuild()
		if berr != nil {
			buildFailed(map[string]any{"byoi_base": req.Image, "error": berr.Error()})
			return "", true
		}
		image = built.Ref
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.build",
			runID.String(), "success", mustJSON(buildSuccessData(map[string]any{
				"byoi_base": req.Image, "image": built.Ref,
			}, built.ID))))
	case req.DevcontainerRepo != "" && s.cfg.ImageBuilder != nil:
		outTag := "wardyn-devcontainer/" + runID.String() + ":latest"
		buildCtx, endBuild, live := s.trackRunBuild(ctx, runID)
		if !live {
			return "", true
		}
		s.announceImageBuild(ctx, runID)()
		built, berr := s.cfg.ImageBuilder.BuildDevcontainer(buildCtx, req.DevcontainerRepo, req.DevcontainerRef, outTag, nil)
		endBuild()
		if berr != nil {
			buildFailed(map[string]any{"devcontainer_repo": req.DevcontainerRepo, "error": berr.Error()})
			return "", true
		}
		image = built.Ref
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.build",
			runID.String(), "success", mustJSON(buildSuccessData(map[string]any{
				"devcontainer_repo": req.DevcontainerRepo, "image": built.Ref,
			}, built.ID))))
	case len(wsRefs) > 0:
		// The workspace that chose the image, else the primary: the seed already
		// refused two different choices (image_conflict).
		imageWS := wsRefs[0]
		if i := slices.IndexFunc(wsRefs, func(ws types.Workspace) bool { return workspaceBaseImage(ws) != "" }); i >= 0 {
			imageWS = wsRefs[i]
		}
		// A base_image workspace reached here via mounts/repos — a composed
		// or UI run that did NOT send workspace_id (the workspace_id door sets req.Image
		// and takes the case above). If it declares an explicit base_image CHOICE but no
		// builder is wired, FAIL the run the same way the workspace_id door 400s
		// (validateImageBuildRequest), rather than silently launching on the convention
		// image and dropping the operator's chosen base image (the door divergence this
		// creates). resolveWorkspaceImage is fail-open by design (shared with the
		// wizard/record paths), so the fail-closed decision for the CHOSEN base image
		// lives here, on the create door, not in it.
		if b := imageWS.BaseImage; b != nil && b.Kind != "recommended" &&
			strings.TrimSpace(b.Image) != "" && s.cfg.ImageBuilder == nil {
			buildFailed(map[string]any{
				"base_image": b.Image,
				"error": "no image builder is wired, so the workspace's chosen base image cannot be wrapped with the " +
					"agent runtime; refusing to silently substitute the convention image (wire an image builder or drop the base image)",
			})
			return "", true
		}
		buildCtx, endBuild, live := s.trackRunBuild(ctx, runID)
		if !live {
			return "", true
		}
		if built, ok := s.resolveWorkspaceImage(buildCtx, runID, imageWS, nil, s.announceImageBuild(ctx, runID)); ok {
			image = built
		}
		endBuild()
	}

	// Persist the resolved image for provenance (best-effort: a failed write
	// must not block dispatch — the audit trail still carries build events).
	if err := s.cfg.Store.SetRunImage(ctx, runID, image); err != nil {
		slog.ErrorContext(ctx, "wardynd: persist run image failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
	}
	return image, false
}
