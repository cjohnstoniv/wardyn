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

	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
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
// seededImageOwner is the ownership half of denyUserSeededImage's fix: the
// OwnedBy of the workspace whose base_image just set req.Image, and "" in
// every other case — including a member-owned workspace that set no image and an
// operator-owned one that did. The callers' capability re-check keys on exactly
// that emptiness, so returning the owner unconditionally would turn an
// ownership-scoped guard into the unconditional variant denyUserSeededImage
// exists to prevent — a catastrophic regression.
func (s *Server) seedRequestWorkspace(ctx context.Context, spec *types.RunPolicySpec, req *createRunRequest) (ephemeralDirs []string, seededImageOwner string, code int, err error) {
	if req.WorkspaceID == nil {
		return nil, "", 0, nil
	}
	if s.cfg.Store == nil {
		return nil, "", http.StatusUnprocessableEntity, fmt.Errorf("workspace_id requires a store, but none is configured")
	}
	ws, gerr := s.cfg.Store.GetWorkspace(ctx, *req.WorkspaceID)
	if gerr != nil {
		return nil, "", http.StatusUnprocessableEntity, fmt.Errorf("workspace %s: %w", *req.WorkspaceID, gerr)
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
				return nil, "", http.StatusUnprocessableEntity, fmt.Errorf("workspace %s source target: %w", ws.ID, verr)
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
		return nil, "", http.StatusUnprocessableEntity, fmt.Errorf("workspace %s conflicts with the policy: %w", ws.ID, verr)
	}
	return ephemeralDirs, seededImageOwner, 0, nil
}

// authorizeSpecWorkspaceSources is the RESOLVED-SPEC half of the onboarding
// gate: every onboarded workspace the spec's mount sources and repos resolve to must be one
// the caller may launch against (mayLaunchWorkspace).
//
// The workspace_id door is authorized by getWorkspaceLaunchable before any
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
// un-onboarded source), so the index lookups below can only miss for a source
// that gate deliberately let past — a blessed system mount, whose source is the
// operator's own staged creds dir and belongs to no workspace.
func (s *Server) authorizeSpecWorkspaceSources(ctx context.Context, r *http.Request, spec types.RunPolicySpec) (int, error) {
	if s.cfg.Store == nil || (len(spec.WorkspaceMounts) == 0 && len(spec.WorkspaceRepos) == 0) {
		return 0, nil
	}
	all, err := s.cfg.Store.ListWorkspaces(ctx)
	if err != nil {
		return http.StatusUnprocessableEntity, fmt.Errorf("list workspaces: %w", err)
	}
	idx := indexWorkspacesBySource(all)
	for _, wm := range spec.WorkspaceMounts {
		if systemMountTargets[wm.Target] {
			continue // operator-blessed system creds mount — source already vetted against the ceiling
		}
		if ws, ok := idx.localDir[wm.Source]; ok && !s.mayLaunchWorkspace(r, ws) {
			return http.StatusUnprocessableEntity, fmt.Errorf(
				"mount source %q is not an onboarded local directory (onboard it first via the workspaces API)", wm.Source)
		}
	}
	for _, wr := range spec.WorkspaceRepos {
		if ws, ok := idx.repo[wr.Repo]; ok && !s.mayLaunchWorkspace(r, ws) {
			return http.StatusUnprocessableEntity, fmt.Errorf(
				"repo %q is not an onboarded repository (onboard it first via the workspaces API)", wr.Repo)
		}
	}
	return 0, nil
}

// enforcedConfinement is the PURE confinement math both the launch path and the
// preflight dry-run run: the requested class when set (never WEAKER than the
// policy minimum), else the STRONGEST class advertised is that meets
// the policy minimum (never the minimum itself; see strongestAdvertisedAtOrAbove),
// then the deterministic BLAST-RADIUS floor.
//
// advertised is the runner's advertised confinement set, passed IN rather than
// fetched here: this function stays pure (no runner, no context) so preflight
// can share it verbatim (TestPreflightMirrorsLaunchGates encodes that split).
// Callers with no runner (or an unread one) pass nil — strongestAdvertisedAtOrAbove
// then leaves the default at the policy minimum, byte-identical to before this
// rule existed.
//
// That floor is defense-in-depth and applies to EVERY run (manual wizard and
// direct API callers, not just composed ones): a run holding powerful
// credentials (write-capable, or a third-party/production api_key) MUST run in
// the strongest sandbox so a sandbox escape can't carry those credentials out to
// your host. A host that cannot provide CC3 then fails closed at the caller's
// capability check rather than running the workload under-confined (invariant 5).
//
// It writes no HTTP so preflight can share it: the returned error's text IS the
// 422 body both callers send (the wizard test hardcodes that string), and
// preflight deliberately skips the caller-side gates that follow it.
func enforcedConfinement(spec types.RunPolicySpec, reqCC types.ConfinementClass, advertised []types.ConfinementClass) (types.ConfinementClass, error) {
	// Assigned in BOTH branches below — declared without a value so the dead
	// store staticcheck flags (SA4006) cannot come back: the floor is no longer
	// the default, it is only the lower bound the default is chosen at or above.
	var enforced types.ConfinementClass
	if reqCC != "" {
		if !confinementGE(reqCC, spec.MinConfinementClass) {
			return "", fmt.Errorf("confinement_class %s is weaker than the policy minimum %s",
				reqCC, spec.MinConfinementClass)
		}
		enforced = reqCC
	} else {
		enforced = strongestAdvertisedAtOrAbove(advertised, spec.MinConfinementClass)
	}
	if composer.RequiredConfinementFloor(spec) == types.CC3 && !confinementGE(enforced, types.CC3) {
		enforced = types.CC3
	}
	return enforced, nil
}

// credentialConfinementBelowFloor is the ONE value the run.create audit row's
// credential_confinement field carries today — a closed vocabulary, like
// confinement_source's requested/defaulted, so an incident review can GROUP on
// it instead of parsing free text. Absent (field omitted) covers everything
// else: no SSO-delivered credential, or one whose enforced confinement already
// meets CC3.
const credentialConfinementBelowFloor = "below_floor"

// credentialConfinementAdvisory is the WARN-never-refuse counterpart to the
// blast-radius floor above (0.8 #150). A stored AWS SSO credential is
// delivered to the sandbox at DISPATCH — after enforcedConfinement has already
// resolved the class above — so it is never itself an eligible grant and
// composer.RequiredConfinementFloor never sees it: folding it in there would
// change ENFORCEMENT, which is explicitly out of scope here. Silence would
// leave a run holding a captured AWS identity under a confinement class
// nothing chose for that reason; this says so instead of raising the floor for
// it, because a host that can only ever offer the weakest class must still be
// able to launch — adding a refusal here would break every single-class
// deployment.
//
// ssoDelivered is the caller's own answer to "did this run's model credential
// resolve to the captured-AWS-SSO lane" (selectedMechanism ==
// types.AgentMechanismBedrockSSO, i.e. resolveBedrockAuth's ssoInject arm) —
// resolved once by the caller from the SAME lane resolution the
// model-credential grade already ran (modelCredentialFacts.Mechanism), never
// re-derived here. Pure, with the same purity contract as enforcedConfinement,
// so it is called from both the launch path and the preflight path off the
// same resolved body — the two can never disagree about whether a run carries
// the advisory.
//
// spec is currently unread: it rides along for the same reason
// enforcedConfinement takes the whole spec rather than just the fields it
// needs today — a future policy-level exception would have somewhere to read
// from without a signature change.
func credentialConfinementAdvisory(spec types.RunPolicySpec, enforced types.ConfinementClass, ssoDelivered bool) string {
	if !ssoDelivered || confinementGE(enforced, types.CC3) {
		return ""
	}
	return fmt.Sprintf(credentialConfinementAdvisorySentence, enforced)
}

// appendCredentialConfinementAdvisory is the two call sites' shared plumbing
// around credentialConfinementAdvisory (runs.go's handleCreateRun and
// preflight.go's handlePreflightRun): append the sentence to warnings when it
// fires, and report whether it did, since the create path's audit row needs
// that same answer for credential_confinement. mechanism is the resolved
// modelCredentialFacts.Mechanism both callers already have in hand.
func appendCredentialConfinementAdvisory(warnings []string, spec types.RunPolicySpec, enforced types.ConfinementClass, mechanism string) ([]string, bool) {
	advisory := credentialConfinementAdvisory(spec, enforced, mechanism == string(types.AgentMechanismBedrockSSO))
	if advisory == "" {
		return warnings, false
	}
	return append(warnings, advisory), true
}

// resolveEnforcedConfinement resolves the run's confinement class and gates it
// against what the runner and identity provider can actually deliver (invariant
// 5, fail closed). The request value wins when set (never WEAKER than the
// policy minimum); an unspecified request defaults to the STRONGEST class the
// runner advertises at or above the policy minimum (strongestAdvertisedAtOrAbove).
// The deterministic BLAST-RADIUS floor then raises powerful-credential
// runs to CC3, and the runner must advertise the EXACT enforced class
// (membership, not rank: a Kata-only host advertises [CC1, CC3] with no
// CC2, so a rank check would pass a CC2 demand and fail later with a raw docker
// error). Writes the HTTP error itself and returns ok=false on any refusal.
// Extracted verbatim from handleCreateRun.
func (s *Server) resolveEnforcedConfinement(ctx context.Context, w http.ResponseWriter, spec types.RunPolicySpec, reqCC types.ConfinementClass) (types.ConfinementClass, bool) {
	// Capabilities are read BEFORE the pure math now, not after: the default
	// branch needs the advertised set to pick the strongest class rather than
	// just the policy minimum. Still one read, still fail-closed on a Capabilities
	// error — only its place in the function moved.
	var caps runner.Capabilities
	if s.cfg.Runner != nil {
		var cerr error
		caps, cerr = s.cfg.Runner.Capabilities(ctx)
		if cerr != nil {
			writeError(w, http.StatusServiceUnavailable, loggedMsg(ctx, "runner capabilities unavailable", cerr))
			return "", false
		}
	}

	enforced, err := enforcedConfinement(spec, reqCC, caps.ConfinementClasses)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return "", false
	}

	// Confinement gating: refuse to schedule a run whose confinement class the
	// runner cannot structurally enforce (invariant 5, fail closed).
	if s.cfg.Runner != nil {
		// Membership, not rank: CC2 (gVisor/runsc) and CC3 (Kata/krun) resolve to
		// INDEPENDENT runtimes, so a host can advertise a non-contiguous set (e.g. a
		// Kata-only host advertises [CC1, CC3], no CC2). A rank check —
		// confinementGE(best, enforced) — would let a CC2 demand pass on that host
		// because CC3 outranks CC2, then fail at sandbox create with a raw docker
		// error. Require the exact enforced class to be advertised. enforced=="" means
		// no class is required (policy floor unset, no request) ⇒ any runner passes.
		// Also the fail-closed net for a floor (explicit or defaulted) the runner
		// cannot enforce at all — strongestAdvertisedAtOrAbove falls back to the
		// floor unchanged when nothing advertised meets it, so THIS check is what
		// still 422s that case exactly as it did before the default rule existed.
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
	// warnings are the sentences the 201 has to carry about wiring this function
	// DECLINED to build — today only the provider lane vetoes. Collected
	// on the struct rather than returned separately for applySSHLaneWarnings'
	// reason: a lane dropped silently fails mid-clone, inside the sandbox, where
	// nobody is reading.
	warnings []string
}

// persistRunGrants persists each eligible grant as an eligibility record (NOT
// issuance) and derives the sandbox wiring above. Approval-gated api_key grants
// are deliberately excluded from injections: an unmet approval would fail the
// proxy's startup mint and brick the sandbox's egress (fail closed, but a
// footgun as a default). A grant write failure is fatal (the run would be
// ungovernable): the HTTP error is written here and ok=false returned — through
// writeServerError, so the driver text behind it reaches the LOG and not the
// member who called POST /runs. That chokepoint is why the request is a
// parameter beside the writer: it is what names the method and path in the log
// line an operator is already reading.
// Extracted verbatim from handleCreateRun.
func (s *Server) persistRunGrants(ctx context.Context, w http.ResponseWriter, r *http.Request, runID uuid.UUID, now time.Time, spec types.RunPolicySpec) (grantWiring, bool) {
	gw := grantWiring{
		gitPATGrants: map[string]string{},
		sshGrants:    map[string]string{},
		gitGrants:    map[string]uuid.UUID{},
	}
	// The provider policy the three git arms below ask about their lane, read ONCE
	// for the whole grant set: the answer cannot be allowed to change between two
	// grants of one run. Zero value in legacy open mode, where laneVetoed is a
	// no-op before it reads anything.
	sc, scErr := s.siteConfigForLaneVeto(ctx, spec)
	if scErr != nil {
		writeServerError(w, r, "get site config", scErr)
		return gw, false
	}
	// This run's own repositories, so a grant whose scope names only a HOST is
	// still decided by the row that ADMITTED the repository the run will clone
	// there — two rows of one kind on one host are a supported configuration, and
	// whichever of them is listed first is not an answer.
	specRepos := repoLocatorsOf(spec.WorkspaceRepos)
	for _, g := range spec.EligibleGrants {
		grantID := uuid.New()
		if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
			ID:        grantID,
			RunID:     runID,
			CreatedAt: now,
			Spec:      g,
		}); gerr != nil {
			// A grant write failure is fatal: the run would be ungovernable.
			writeServerError(w, r, "create grant", gerr)
			return gw, false
		}
		if g.Kind == types.GrantGitHubToken {
			// The `app` lane, vetoed: no broker entry, no WARDYN_GITHUB_GRANT_ID,
			// no /wardyn/gh/ route. The grant ROW stays (an eligibility record
			// nothing will mint), which is applySSHLaneWarnings' own rule.
			// Decided by the row that ADMITTED the grant's own repository — the scope
			// names it, so there is no need to guess from the host, and a GHES row and
			// a cloud row are both `kind: github`. A scope whose repo list is the empty
			// TEMPLATE (the common case for `--repo` and the example policies) falls
			// back to this run's own repositories on github.com, and only then to the
			// union of the rows claiming it. The broker mints per <org>/<repo> on
			// github.com alone (gitBrokerKey answers for no other host), which is what
			// GitLaneApp's own doc says.
			if msg, vetoed := s.laneVetoedForGrantHost(ctx, sc, runID, types.GitLaneApp, g.Kind,
				"github.com", append(githubScopeRepos(g.Scope), specRepos...)); vetoed {
				gw.warnings = append(gw.warnings, msg)
				continue
			}
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
				// The `pat` lane, vetoed: no WARDYN_GIT_PAT_GRANTS entry — AND no ADO
				// egress bundle, which is the half a veto written anywhere else would
				// have left behind. Nothing but this arm adds those domains, so
				// dropping the lane has to drop them in the same breath or the run
				// carries reachability for a forge it can no longer authenticate to.
				if msg, vetoed := s.laneVetoedForGrantHost(ctx, sc, runID, types.GitLanePAT, g.Kind, host, specRepos); vetoed {
					gw.warnings = append(gw.warnings, msg)
					continue
				}
				gw.gitPATGrants[host] = grantID.String()
				gw.gitPATEgress = append(gw.gitPATEgress, grantLaneEgress(g)...)
			}
		}
		if g.Kind == types.GrantSSHKey {
			// validatePolicySpec already vetted the host is a supported SSH-over-443
			// provider, so sshOver443Endpoint is expected to resolve here.
			if host, _, _, _, derr := sshKeyScopeFields(g.Scope); derr == nil {
				// The `ssh` lane, vetoed: no WARDYN_SSH_GRANTS entry and no :443
				// endpoint added — so no private key is ever written into the sandbox
				// for a clone the admin said must not use one.
				if msg, vetoed := s.laneVetoedForGrantHost(ctx, sc, runID, types.GitLaneSSH, g.Kind, host, specRepos); vetoed {
					gw.warnings = append(gw.warnings, msg)
					continue
				}
				gw.sshGrants[host] = grantID.String()
				gw.sshEgress = append(gw.sshEgress, grantLaneEgress(g)...)
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

// grantLaneEgress is the egress one grant's SCM lane needs beyond its own
// host: a git_pat to an Azure DevOps host needs the dev.azure.com /
// *.visualstudio.com bundle (adoEgressDomains), an ssh_key its PORT-QUALIFIED
// SSH-over-443 endpoint (sshOver443Endpoint). Every other kind, and a scope
// that does not parse, needs nothing.
//
// A function of the grant alone, BEFORE any veto, because two callers need
// the same answer at different times: persistRunGrants builds the lanes from
// it at launch, and the autonomy posture grades them on both doors before any
// lane exists (autonomyPostureSpec).
func grantLaneEgress(g types.GrantSpec) []string {
	switch g.Kind {
	case types.GrantGitPAT:
		if host, _, _, err := gitPATScopeFields(g.Scope); err == nil {
			return adoEgressDomains(host)
		}
	case types.GrantSSHKey:
		if host, _, _, _, err := sshKeyScopeFields(g.Scope); err == nil {
			if ep, ok := sshOver443Endpoint(host); ok {
				return []string{ep}
			}
		}
	}
	return nil
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
//
// wsRefs is the run's referenced onboarded workspaces, resolved by the caller
// (it also feeds the workspace cred binding + image resolution). legacyRepo is
// the request's single `repo` field, which the declaresRepo gate below needs
// and grantWiring cannot supply. scmSite is the site-config snapshot the
// autonomy gate graded the SCM-host lane from (resolveRunAutonomy), so the
// hosts dispatched here are the hosts that were graded. Extracted verbatim
// from handleCreateRun.
func (s *Server) unionRunEgress(ctx context.Context, runID uuid.UUID, spec *types.RunPolicySpec, gw grantWiring, wsRefs []types.Workspace, legacyRepo string,
	scmSite types.SiteConfig,
) {
	if added := unionWorkspaceEgress(spec, wsRefs); len(added) > 0 {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.workspace.egress",
			runID.String(), "success", mustJSON(map[string]any{"added_domains": added})))
	}
	// Repo clone host(s) each referenced workspace needs: a
	// non-GitHub HTTPS clone (GitLab, self-hosted git) reaches its forge as an
	// ordinary egress host, and nothing else in this union adds it — so a real run
	// of a workspace whose only access is anonymous read got NO clone host and the
	// clone was proxy-denied, even though the confined Verify (which unions
	// workspaceCloneEgress via confinedEgressDomains) proved it. GitHub sources add
	// nothing here by design (they route through the on-segment broker). This makes
	// promoteSkipHosts' "wired into every scan/verify for free" comment true for a
	// real run too.
	var cloneAdded []string
	for _, ws := range wsRefs {
		cloneAdded = append(cloneAdded, unionAllowedDomains(spec, workspaceCloneEgress(ws))...)
	}
	if len(cloneAdded) > 0 {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.workspace.clone_egress",
			runID.String(), "success", mustJSON(map[string]any{"added_domains": cloneAdded})))
	}
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
		if added := unionSiteConfigScmHosts(spec, scmSite); len(added) > 0 {
			s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.site_config.egress",
				runID.String(), "success", mustJSON(map[string]any{"added_domains": added})))
		}
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
		if built, ok := s.resolveWorkspaceImage(buildCtx, runID, wsRefs[0], nil); ok {
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
