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
	"github.com/cjohnstoniv/wardyn/pkg/client"
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
// ephemeral source targets are therefore a known, currently-unclosed gap — see
// reconcile-workspace-first.md item 2 — not something this function covers.
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
func (s *Server) seedRequestWorkspace(ctx context.Context, spec *types.RunPolicySpec, req *createRunRequest) (ephemeralDirs []string, code int, err error) {
	if req.WorkspaceID == nil {
		return nil, 0, nil
	}
	if s.cfg.Store == nil {
		return nil, http.StatusUnprocessableEntity, fmt.Errorf("workspace_id requires a store, but none is configured")
	}
	ws, gerr := s.cfg.Store.GetWorkspace(ctx, *req.WorkspaceID)
	if gerr != nil {
		return nil, http.StatusUnprocessableEntity, fmt.Errorf("workspace %s: %w", *req.WorkspaceID, gerr)
	}
	var newMounts []types.WorkspaceMount
	var newRepos []types.WorkspaceRepo
	for _, src := range ws.Sources {
		// The stored target becomes an in-container mount/clone/scratch target
		// here, so re-validate it against the same deny-list runner.ValidateTarget
		// applies at onboarding — a row written before that check existed must not
		// ride straight past the gate onto a system path (e.g. /home/agent/.claude).
		target := src.Target
		if target != "" {
			if verr := runner.ValidateTarget(target); verr != nil {
				return nil, http.StatusUnprocessableEntity, fmt.Errorf("workspace %s source target: %w", ws.ID, verr)
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
		return nil, http.StatusUnprocessableEntity, fmt.Errorf("workspace %s conflicts with the policy: %w", ws.ID, verr)
	}
	return ephemeralDirs, 0, nil
}

// requirementAuditEntry is one audit-worthy fact applyWorkspaceRequirements
// produced (an auto-attached secret grant, or a non-empty egress addition).
// applyWorkspaceRequirements itself never audits — like foldRunIntegration, the
// "fold" is separated from "audit" (the create path emits the events once the
// run id is minted) so preflight, which persists nothing, can call the SAME
// fold and simply discard these.
type requirementAuditEntry struct {
	action string
	target string
	data   map[string]any
}

// resolveWorkspaceSelections builds the per-workspace selection map
// applyWorkspaceRequirements consults, keyed by workspace id string, from
// CreateRunRequest.Workspaces PLUS the legacy singular WorkspaceID — kept a
// "working single-selection alias": a caller using ONLY workspace_id (every
// pre-existing SDK/CLI/policy caller) must fold IDENTICALLY to explicitly
// selecting that one workspace with no optional requirement enabled and no
// write narrowing, which is exactly the zero-value WorkspaceSelection. An
// explicit Workspaces[] entry for the same id wins over the synthesized alias.
// Shared by create (runs.go) and preflight so the two can never disagree about
// which selection a workspace resolves to.
func resolveWorkspaceSelections(req createRunRequest) map[string]client.WorkspaceSelection {
	out := selectionsByWorkspaceID(req.Workspaces)
	if req.WorkspaceID != nil {
		id := req.WorkspaceID.String()
		if _, exists := out[id]; !exists {
			out[id] = client.WorkspaceSelection{WorkspaceID: id}
		}
	}
	return out
}

// selectionsByWorkspaceID indexes a WorkspaceSelection list by id — the part of
// resolveWorkspaceSelections that has a second caller: the compose pipeline's
// preview fold (compose.go), which has req.WorkspaceSelections (the same
// []client.WorkspaceSelection shape) but no legacy singular workspace_id to
// alias in, so it calls this directly instead of resolveWorkspaceSelections'
// createRunRequest-shaped wrapper.
func selectionsByWorkspaceID(sels []client.WorkspaceSelection) map[string]client.WorkspaceSelection {
	out := make(map[string]client.WorkspaceSelection, len(sels))
	for _, sel := range sels {
		if sel.WorkspaceID != "" {
			out[sel.WorkspaceID] = sel
		}
	}
	return out
}

// applyWorkspaceRequirements folds each referenced workspace's requirements
// contract (types.Workspace.Requirements) into the run's RESOLVED policy — the
// per-workspace analogue of unionWorkspaceEgress/applyWorkspaceCreds, kept as
// its OWN function and never inlined into either of those (nor into
// ensureLLMGrant): a reviewer specifically flagged that a fourth folder
// inlined among those three is where double-grants and dropped hosts hide.
//
// Owner's contract, verbatim: "Any setting that is required always comes by
// default with the workspace whenever it is used; anything optional is then
// configurable / enabled when necessary." Concretely, per requirement type:
//
//   - egress:<host>  Required unions host into AllowedDomains unconditionally
//     (mirrors unionWorkspaceEgress's own unconditional append). Optional
//     unions it ONLY when the run's selection lists the key in
//     EnabledOptional.
//   - secret:<NAME>  Required+operator_set mints the SAME api_key-style grant
//     shape the pre-Integration applyWorkspaceCreds used for a workspace's
//     api_key binding (git history: `git show ecc1903~1:internal/api/llmcred.go`,
//     the pre-Integration api_key case), scoped to the run's agent's own
//     model-provider host (applyRequiredSecretGrant). Required+scan_seeded
//     NEVER auto-grants — see the TRUST BOUNDARY comment below. An optional
//     secret follows the identical rule, gated additionally on the run's
//     selection enabling the key.
//   - write:<path>   Resolves the SOURCE's effective writability onto every
//     already-seeded mount at that path: Required defaults writable, Optional
//     defaults read-only unless enabled — and the per-run ReadOnly selection
//     may only NARROW that default (force read-only), never widen it (see
//     applyWriteNarrowing).
//   - integration:<id>  Unions the named integration's hosts into
//     AllowedDomains and, when it delivers a credential by header, authors the
//     proxy-side injection grant for each of them
//     (applyIntegrationRequirement, integrations_run.go). This is the ONLY way
//     an integration reaches a run: configuring one grants nothing by itself.
//
// A workspace with NO requirements declared is architecturally a no-op here —
// the loop below only ever visits declared keys — which is exactly what keeps
// every pre-contract (or simply unconfigured) workspace's resolved spec
// byte-identical to today's behavior.
//
// Must run BEFORE persistRunGrants, which snapshots spec.EligibleGrants into
// persisted grants + proxy injections, and AFTER the workspace's own
// mounts/repos are already seeded onto spec (seedRequestWorkspace / an
// inline/stored policy) so the write-path narrowing below has a mount to
// adjust.
//
// Returns one entry per auto-attached secret grant and one per workspace with
// a non-empty egress addition, for the CALLER to audit (launch does; preflight
// discards them — see requirementAuditEntry).
func (s *Server) applyWorkspaceRequirements(ctx context.Context, spec *types.RunPolicySpec, agent string, wsRefs []types.Workspace, selections map[string]client.WorkspaceSelection) []requirementAuditEntry {
	return s.applyWorkspaceRequirementsFor(ctx, nil, spec, agent, wsRefs, selections)
}

// applyWorkspaceRequirementsFor is applyWorkspaceRequirements with the
// CALLER's presence map. A request handler passes the owner-scoped map it
// already computed (presentSecretNamesFor with secretOwnerFromRequest), so a
// workspace's integration requirement resolves a member's own key exactly as
// their run will; nil means the operator namespace, computed lazily — the
// record route and every existing test keep that.
func (s *Server) applyWorkspaceRequirementsFor(ctx context.Context, present map[string]bool, spec *types.RunPolicySpec, agent string, wsRefs []types.Workspace, selections map[string]client.WorkspaceSelection) []requirementAuditEntry {
	var events []requirementAuditEntry
	// Resolved AT MOST ONCE per call, lazily on the first integration: key
	// found (PLATFORM-API-8) — effectiveIntegrations reads the site-config
	// store, a full secret listing, and peeks the subscription/Bedrock state,
	// so recomputing it per requirement (a workspace with 6 integration
	// requirements = 6 full derivations) multiplied that I/O by the
	// requirement count on every POST /runs and /runs/preflight.
	var integrationRows []integrationRow
	integrationRowsLoaded := false
	for _, ws := range wsRefs {
		if len(effectiveRequirements(ws)) == 0 {
			continue
		}
		sel := selections[ws.ID.String()] // zero value when absent: nothing optional enabled, no narrowing
		var addedEgress []string
		// Sorted iteration: map order is otherwise nondeterministic, and this
		// drives grant-creation and audit-event ordering.
		reqs := effectiveRequirements(ws)
		for _, key := range sortedKeys(reqs) {
			req := reqs[key]
			typ, name, ok := splitRequirementKey(key)
			if !ok {
				continue // defense only: the write endpoint already rejects a bad key
			}
			enabled := req.Level == "required" || slices.Contains(sel.EnabledOptional, key)
			switch typ {
			case "egress":
				if !enabled {
					continue
				}
				// #12: same TRUST BOUNDARY as the secret case below, gated
				// behind RequireOperatorSetEgress (default off — see the
				// Config field doc). When enabled, a scan_seeded egress host
				// (the workspace scanner reading untrusted repo content) is
				// skipped; only an operator's DIRECT declaration auto-adds.
				if s.cfg.RequireOperatorSetEgress && req.Provenance != "operator_set" {
					continue
				}
				addedEgress = append(addedEgress, unionAllowedDomains(spec, []string{name})...)
			case "secret":
				if !enabled {
					continue
				}
				// TRUST BOUNDARY (security-critical — do not relax): a
				// scan_seeded secret requirement comes from the WORKSPACE
				// SCANNER reading UNTRUSTED repo content (e.g. a committed
				// .env template naming a var), never from an operator's own
				// action. Auto-minting a grant from it would let a hostile,
				// or simply never-reviewed, workspace route the OPERATOR's
				// own stored secrets into a run just by naming them in a
				// dotenv. Only an operator's DIRECT declaration
				// (operator_set) may ever auto-grant.
				if req.Provenance != "operator_set" {
					continue
				}
				if present == nil {
					present = s.presentSecretNames(ctx)
				}
				if ev, ok := s.applyRequiredSecretGrant(present, spec, agent, name); ok {
					events = append(events, ev)
				}
			case "integration":
				if !enabled {
					continue
				}
				// No provenance gate, unlike secret: above. A scan can propose a
				// HOST it saw, but it cannot propose an INTEGRATION — an
				// integration id only exists because an operator configured that
				// row and then named it here, so naming one is already a direct
				// operator act. The trust boundary that rule protects (untrusted
				// repo content routing the operator's stored secrets into a run)
				// has no path here.
				if !integrationRowsLoaded {
					if present == nil {
						present = s.presentSecretNames(ctx)
					}
					integrationRows = s.effectiveIntegrations(ctx, present, s.setupBedrock(ctx, present))
					integrationRowsLoaded = true
				}
				if ev, ok := s.applyIntegrationRequirement(ctx, present, integrationRows, spec, name); ok {
					events = append(events, ev)
				}
			case "write":
				applyWriteNarrowing(spec, name, enabled, sel.ReadOnly)
			}
		}
		if len(addedEgress) > 0 {
			events = append(events, requirementAuditEntry{
				action: "run.workspace.requirement.egress", target: ws.ID.String(),
				data: map[string]any{"workspace_id": ws.ID.String(), "added_domains": addedEgress},
			})
		}
	}
	return events
}

// effectiveRequirements is the contract a run actually consumes: the store's
// hydrate pass folds attachments + source contracts + the overlay into
// EffectiveRequirements. A workspace that never passed through hydration (a
// hand-built fixture, a fake store) carries none — and for it the fold's own
// zero-source identity says the overlay IS the contract, so falling back to
// Requirements is the correct semantic, not a compatibility shim.
func effectiveRequirements(ws types.Workspace) map[string]types.WorkspaceRequirement {
	if ws.EffectiveRequirements != nil {
		return ws.EffectiveRequirements
	}
	return ws.Requirements
}

// applyRequiredSecretGrant mints the api_key-style grant an operator-declared
// required (or enabled-optional) secret requirement promises, coupling it to
// an exact egress allowlist entry — the SAME grant/injection/egress shape the
// pre-Integration applyWorkspaceCreds used for a workspace's api_key binding
// (git history: `git show ecc1903~1:internal/api/llmcred.go`,
// pre-Integration api_key case), scoped to the run's AGENT's own
// model-provider host (agentLLMProvider) — the only host this generic
// requirement key has any deterministic binding to. ok=false — no grant, no
// mutation — when: the agent has no LLM-provider convention (nothing to bind
// to); a grant for that host is already proposed (never double-grant the same
// host — whichever caller proposed it first wins, mirroring
// ensureLLMGrant/applyWorkspaceCreds); or the named secret is not actually
// stored (an auto-mint grant with no resolvable secret would fail the proxy
// CLOSED at startup — degrade silently to no-model-access instead of bricking
// the run; compose_setup.go's checklist escalates the gap to blocking styling).
func (s *Server) applyRequiredSecretGrant(present map[string]bool, spec *types.RunPolicySpec, agent, secretName string) (requirementAuditEntry, bool) {
	p, ok := s.llmProviderFor(agent)
	if !ok {
		return requirementAuditEntry{}, false
	}
	if _, exists := apiKeyGrantForHost(spec, p.host); exists {
		return requirementAuditEntry{}, false
	}
	if !present[secretName] {
		return requirementAuditEntry{}, false
	}
	scope, _ := json.Marshal(map[string]string{
		"host": p.host, "header": p.header, "format": p.format, "secret_name": secretName,
	})
	spec.EligibleGrants = append(spec.EligibleGrants, types.GrantSpec{
		Kind: types.GrantAPIKey, Scope: scope, TTLSeconds: 3600, RequiresApproval: false,
	})
	// Couple the exact-host egress entry UNCONDITIONALLY, even under allow-all
	// (SPINE-4): AllowedExactHost does not honor allow-all, so a grant whose host
	// is missing from AllowedDomains fails the proxy injector closed at startup
	// (zero egress) — e.g. a required operator_set secret on an allow_all_egress
	// ceiling would otherwise brick every run granted it.
	if !domainAllowedExact(spec.AllowedDomains, p.host) {
		spec.AllowedDomains = append(spec.AllowedDomains, p.host)
	}
	return requirementAuditEntry{
		action: "run.workspace.requirement.secret", target: secretName,
		data: map[string]any{"secret_name": secretName, "host": p.host},
	}, true
}

// applyWriteNarrowing resolves ONE write:<path> requirement's effective mount
// writability onto every spec.WorkspaceMounts entry whose Source is path (a
// no-op when path isn't actually mounted this run — e.g. a multi-source
// workspace attached only partially). grantedDefault is the contract's own
// default BEFORE narrowing (true for Required, or an enabled Optional); narrow
// is the run's OPTIONAL per-source override and is NARROW-ONLY: narrow=true
// forces read-only regardless of grantedDefault (a run may drop a Required
// write); narrow=false or nil never WIDENS past grantedDefault (a run may not
// add an Optional write it never enabled by putting the key in
// EnabledOptional — only Level/EnabledOptional can grant write, never this
// field alone).
func applyWriteNarrowing(spec *types.RunPolicySpec, path string, grantedDefault bool, narrow *bool) {
	granted := grantedDefault
	if narrow != nil && *narrow {
		granted = false
	}
	for i := range spec.WorkspaceMounts {
		if spec.WorkspaceMounts[i].Source != path {
			continue
		}
		ro := !granted
		spec.WorkspaceMounts[i].ReadOnly = &ro
	}
}

// enforcedConfinement is the PURE confinement math both the launch path and the
// preflight dry-run run: the requested class when set (never WEAKER than the
// policy minimum), else the policy minimum, then the deterministic BLAST-RADIUS
// floor.
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
func enforcedConfinement(spec types.RunPolicySpec, reqCC types.ConfinementClass) (types.ConfinementClass, error) {
	enforced := spec.MinConfinementClass
	if reqCC != "" {
		if !confinementGE(reqCC, spec.MinConfinementClass) {
			return "", fmt.Errorf("confinement_class %s is weaker than the policy minimum %s",
				reqCC, spec.MinConfinementClass)
		}
		enforced = reqCC
	}
	if composer.RequiredConfinementFloor(spec) == types.CC3 && !confinementGE(enforced, types.CC3) {
		enforced = types.CC3
	}
	return enforced, nil
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
	enforced, err := enforcedConfinement(spec, reqCC)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return "", false
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
// (it also feeds the workspace cred binding + image resolution) — this used to
// resolve them itself. legacyRepo is the request's single `repo` field, which
// the declaresRepo gate below needs and grantWiring cannot supply. Extracted
// verbatim from handleCreateRun.
func (s *Server) unionRunEgress(ctx context.Context, runID uuid.UUID, spec *types.RunPolicySpec, gw grantWiring, wsRefs []types.Workspace, legacyRepo string) {
	if added := unionWorkspaceEgress(spec, wsRefs); len(added) > 0 {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.workspace.egress",
			runID.String(), "success", mustJSON(map[string]any{"added_domains": added})))
	}
	// Repo clone host(s) each referenced workspace needs (GAP-EGRESS-1): a
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
	// only when this run actually declares a repo (GAP-EGRESS-3): a sealed
	// local-dir-only analysis run must not inherit an unauthenticated HTTPS lane to
	// every internal SCM host the operator declared for GHES clone runs. A repo is
	// declared via spec.WorkspaceRepos, the legacy single `repo` field, or any
	// git-clone grant lane.
	//
	// legacyRepo is checked DIRECTLY (eca43861 gated on gw.gitGrants instead, on
	// the comment's claim that "the legacy run.Repo already folded into
	// gw.gitGrants by augmentGitBrokerGrants" — which is false: that fold is a
	// no-op when gw.firstGitHubGrantID == nil, i.e. whenever the run has no
	// github grant). The hole is user-visible: repoCloneURL (runs_scm.go) accepts
	// a full https:// URL, so `--repo https://ghes.corp.example/team/app` is an
	// ordinary credential-free HTTPS clone whose ONLY egress source is this
	// union — with no grant minted, declaresRepo went false and the proxy denied
	// the clone. That is the same GAP-EGRESS-1 class eca43861 fixed for
	// workspaces one hunk above. The gate's actual intent (a sealed
	// local-dir-only run inherits no SCM lane) is unchanged: no repo, no union.
	declaresRepo := len(spec.WorkspaceRepos) > 0 || strings.TrimSpace(legacyRepo) != "" ||
		gw.firstGitHubGrantID != nil ||
		len(gw.gitGrants) > 0 || len(gw.gitPATGrants) > 0 || len(gw.sshGrants) > 0
	if declaresRepo {
		if added := s.unionSiteConfigScmHosts(ctx, spec); len(added) > 0 {
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
		// D9: surface the build failure under the FAILED badge, not only in the
		// run.build audit row. The error text is already in auditData["error"].
		hint := "the run's sandbox image could not be built"
		if e, ok := auditData["error"].(string); ok && e != "" {
			hint += ": " + e
		}
		s.failAndRevoke(ctx, runID, types.RunPending, hint)
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
		// PARITY-4: a base_image workspace reached here via mounts/repos — a composed
		// or UI run that did NOT send workspace_id (the workspace_id door sets req.Image
		// and takes the case above). If it declares an explicit base_image CHOICE but no
		// builder is wired, FAIL the run the same way the workspace_id door 400s
		// (validateImageBuildRequest), rather than silently launching on the convention
		// image and dropping the operator's chosen base image (the door divergence
		// PARITY-4 flags). resolveWorkspaceImage is fail-open by design (shared with the
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
