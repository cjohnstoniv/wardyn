// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	neturl "net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recordmode"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// referencedWorkspaces resolves the onboarded workspaces a RESOLVED spec uses —
// local-dir mount sources (skipping system credential mounts) + repos, in
// selection order. The first entry is the PRIMARY (its profile drives image
// selection). Best-effort + never errors: a source with no matching onboarded row
// is skipped (the onboarding gate already rejected non-onboarded sources at
// run-create, so in practice every user source resolves here). Deduped by
// (kind, source).
func (s *Server) referencedWorkspaces(ctx context.Context, spec types.RunPolicySpec) []types.Workspace {
	if s.cfg.Store == nil {
		return nil
	}
	var out []types.Workspace
	seen := map[string]bool{}
	add := func(kind types.WorkspaceKind, source string) {
		if source == "" {
			return
		}
		key := string(kind) + "\x00" + source
		if seen[key] {
			return
		}
		seen[key] = true
		if ws, err := s.cfg.Store.GetWorkspaceBySource(ctx, kind, source); err == nil {
			out = append(out, ws)
		}
	}
	for _, wm := range spec.WorkspaceMounts {
		if systemMountTargets[wm.Target] {
			continue
		}
		add(types.WorkspaceKindLocalDir, wm.Source)
	}
	for _, wr := range spec.WorkspaceRepos {
		add(types.WorkspaceKindRepo, wr.Repo)
	}
	return out
}

// workspaceProfile decodes a workspace's opaque profile blob into the scanner's
// WorkspaceProfile. Returns ok=false when there is no profile yet (unscanned) or
// it is malformed.
func workspaceProfile(ws types.Workspace) (workspacescan.WorkspaceProfile, bool) {
	if len(ws.Profile) == 0 {
		return workspacescan.WorkspaceProfile{}, false
	}
	var p workspacescan.WorkspaceProfile
	if err := json.Unmarshal(ws.Profile, &p); err != nil {
		return workspacescan.WorkspaceProfile{}, false
	}
	return p, true
}

// unionWorkspaceEgress adds every referenced workspace's trusted egress to the
// spec's AllowedDomains (deduped, in-place) and returns what it added. Two
// sources, both operator-sanctioned: the scanned profile's EgressDomains
// (filename-keyed marker table — never file content) and the workspace's
// ApprovedEgress (content-derived suggestions the operator explicitly
// promoted). The scanner's raw SuggestedEgress is deliberately NOT here — a
// hostile file must never widen an allowlist without a human approval. The
// deny-list and confinement floor are unaffected.
func unionWorkspaceEgress(spec *types.RunPolicySpec, workspaces []types.Workspace) []string {
	var added []string
	for _, ws := range workspaces {
		if p, ok := workspaceProfile(ws); ok {
			added = append(added, unionAllowedDomains(spec, p.EgressDomains)...)
		}
		added = append(added, unionAllowedDomains(spec, ws.ApprovedEgress)...)
	}
	return added
}

// unionSiteConfigScmHosts adds the operator's site-config default SCM hosts
// (types.SiteConfig.ScmHosts, set via PUT /api/v1/site-config) to the spec's
// AllowedDomains (deduped, in-place) and returns what it added. Unlike GitHub/
// ADO — whose egress bundles are either baked into the example policies or
// derived from a git_pat/ssh_key grant's host (adoEgressDomains,
// sshOver443Endpoint) — a self-hosted GHES or ADO Server has no such built-in
// bundle, so the operator declares its host(s) once in site-config and every
// cloning run inherits them. Non-secret, additive: it only ever widens the
// allowlist with hosts the operator explicitly declared, never anything
// content-derived. No SiteConfig row / no Store configured / no ScmHosts set
// are all the common "unconfigured" case and add nothing.
func (s *Server) unionSiteConfigScmHosts(ctx context.Context, spec *types.RunPolicySpec) []string {
	if s.cfg.Store == nil {
		return nil
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil || len(sc.ScmHosts) == 0 {
		return nil
	}
	return unionAllowedDomains(spec, sc.ScmHosts)
}

// substituteArtifactEgress applies the operator's artifact-registry redirects to
// a run's AllowedDomains: for each CONFIGURED ecosystem it DROPS that language's
// public-registry hosts (markers.go's egress* literals, via
// workspacescan.PublicRegistryHosts) and ADDS the corporate mirror host (the host
// of the override base URL). Corp REPLACES public for configured langs;
// unconfigured langs are untouched, and markers.go stays universally correct —
// the substitution lives here, at the composition layer that reads site-config,
// never in the marker literals. Returns a FRESH slice (never mutates the input's
// backing array); a no-op (returns the input) when nothing is configured. A
// malformed override base URL leaves that ecosystem's public hosts in place
// (fail safe: never silently drop egress a build still needs).
func substituteArtifactEgress(domains []string, sc types.SiteConfig) []string {
	if len(sc.ArtifactOverrides) == 0 {
		return domains
	}
	drop := map[string]bool{}
	var add []string
	added := map[string]bool{}
	ecos := make([]string, 0, len(sc.ArtifactOverrides))
	for eco := range sc.ArtifactOverrides {
		ecos = append(ecos, eco)
	}
	sort.Strings(ecos) // deterministic add order
	for _, eco := range ecos {
		corp := strings.ToLower(workspacescan.HostOf(sc.ArtifactOverrides[eco].BaseURL))
		if corp == "" {
			continue
		}
		for _, h := range workspacescan.PublicRegistryHosts(eco) {
			drop[strings.ToLower(h)] = true
		}
		if !added[corp] {
			added[corp] = true
			add = append(add, corp)
		}
	}
	out := make([]string, 0, len(domains)+len(add))
	have := map[string]bool{}
	for _, d := range domains {
		key := strings.ToLower(d)
		if drop[key] || have[key] {
			continue
		}
		have[key] = true
		out = append(out, d)
	}
	for _, a := range add {
		if !have[a] {
			have[a] = true
			out = append(out, a)
		}
	}
	return out
}

// workspaceSuggestedEgress collects the referenced workspaces' content-derived
// suggested hosts (deduped, order-preserving), minus anything the profiles/
// approvals already allow. Display-only input for the setup checklist — it
// must never feed AllowedDomains.
func workspaceSuggestedEgress(workspaces []types.Workspace) []string {
	allowed := map[string]bool{}
	for _, ws := range workspaces {
		if p, ok := workspaceProfile(ws); ok {
			for _, d := range p.EgressDomains {
				allowed[d] = true
			}
		}
		for _, d := range ws.ApprovedEgress {
			allowed[d] = true
		}
	}
	var out []string
	seen := map[string]bool{}
	for _, ws := range workspaces {
		p, ok := workspaceProfile(ws)
		if !ok {
			continue
		}
		for _, d := range p.SuggestedEgress {
			if !seen[d] && !allowed[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}

// resolveWorkspaceImage returns the sandbox image for a run driven by its PRIMARY
// onboarded workspace, or ok=false to fall through to the convention image.
// Order (all fail-OPEN — any failure returns ok=false + convention image, never
// blocks the run):
//   - a REPO whose profile HasDevcontainer → build the repo's own devcontainer;
//   - a cached generated image still valid for the current profile hash → reuse;
//   - else generate a devcontainer for the detected toolchain, build it, and cache
//     image_ref + built_profile_hash on the workspace for reuse.
//
// It audits its own build success/failure against runID.
func (s *Server) resolveWorkspaceImage(ctx context.Context, runID uuid.UUID, primary types.Workspace) (string, bool) {
	if s.cfg.ImageBuilder == nil {
		return "", false
	}
	p, ok := workspaceProfile(primary)
	if !ok {
		return "", false // unscanned/malformed → convention image
	}

	buildAudit := func(outcome string, extra map[string]any) {
		extra["workspace_id"] = primary.ID.String()
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.build",
			runID.String(), outcome, mustJSON(extra)))
	}

	// A repo carrying its OWN devcontainer: respect it, build from the repo — UNLESS
	// the source is an SSH URL. The image builder (envbuilder) clones with no minted
	// key / known_hosts / :443 ProxyCommand — only the agent-run sandbox has that
	// wiring — so an SSH devcontainer build would fail auth. Fall through to a
	// generated toolchain image; agent-run still clones the repo itself using the
	// run's ssh_key grant (the repo's own devcontainer is just not built in v1).
	if primary.Kind == types.WorkspaceKindRepo && p.HasDevcontainer {
		if _, ssh := sshCloneHost(primary.Source); ssh {
			buildAudit("skipped", map[string]any{"source": "repo-devcontainer", "reason": "ssh-source-not-buildable-by-image-builder"})
		} else if url := repoCloneURL(primary.Source); url != "" {
			tag := "wardyn-workspace/" + primary.ID.String() + ":devcontainer"
			if built, err := s.cfg.ImageBuilder.BuildDevcontainer(ctx, url, primary.Ref, tag); err == nil {
				buildAudit("success", map[string]any{"source": "repo-devcontainer", "image": built})
				return built, true
			} else {
				buildAudit("failure", map[string]any{"source": "repo-devcontainer", "error": err.Error()})
				return "", false
			}
		}
	}

	hash := p.ProfileHash()
	// Reuse a cached generated image when the profile is unchanged.
	if primary.ImageRef != "" && primary.BuiltProfileHash == hash {
		return primary.ImageRef, true
	}

	// Generate a devcontainer for the detected toolchain and build it.
	files, gerr := workspacescan.GenerateDevcontainer(p)
	if gerr != nil {
		buildAudit("failure", map[string]any{"source": "generated-devcontainer", "error": gerr.Error()})
		return "", false
	}
	tag := "wardyn-workspace/" + primary.ID.String() + ":" + hash[:12]
	built, berr := s.cfg.ImageBuilder.BuildFromDevcontainerFiles(ctx, files, tag)
	if berr != nil {
		buildAudit("failure", map[string]any{"source": "generated-devcontainer", "error": berr.Error()})
		return "", false
	}
	// Cache the built image on the workspace for reuse by later runs — a SCOPED
	// write: the previous full-row UpdateWorkspace here replayed a stale
	// pre-launch snapshot over every concurrently-persisted async column
	// (active_run_id, record_results, verify state).
	if _, uerr := s.cfg.Store.SetWorkspaceBuiltImage(ctx, primary.ID, built, hash); uerr != nil {
		// Non-fatal: the image built and is usable now; caching just missed.
		buildAudit("success", map[string]any{"source": "generated-devcontainer", "image": built, "cache_warn": uerr.Error()})
		return built, true
	}
	buildAudit("success", map[string]any{"source": "generated-devcontainer", "image": built})
	return built, true
}

// verifyEgressDomains is the two-phase SETUP allowlist for a verify run:
// deliberately WIDER than a scan's narrow git-only set, so install commands can
// reach their registries. Base = git hosts (for clone) unioned with the
// workspace's trusted egress (profile.EgressDomains, filename-keyed marker
// registries) + operator ApprovedEgress. Content-derived SuggestedEgress is NOT
// included — a build that needs one surfaces as an observed-egress denial the
// operator can promote (least-privilege, honest).
func verifyEgressDomains(ws types.Workspace) []string {
	base := &types.RunPolicySpec{AllowedDomains: scanEgressDomains(repoCloneURL(ws.Source))}
	unionWorkspaceEgress(base, []types.Workspace{ws})
	return base.AllowedDomains
}

// launchVerifyRun starts a throwaway GOVERNED run that runs wardyn-verify — it
// executes the workspace's OPERATOR-APPROVED SetupCommands (install/build/test)
// in the BUILT devcontainer image (resolveWorkspaceImage) under confinement,
// captures per-step results, and uploads a VerifyResult via the trusted
// run→workspace linkage. Wider (setup-phase) egress than a scan. The commands
// are the discriminator passed to dispatchWithVerify (both scan and verify set
// WorkspaceID). Returns the created run; the caller has already flipped the
// workspace to `verifying` + set active_run_id.
func (s *Server) launchVerifyRun(ctx context.Context, actor string, ws types.Workspace, commands json.RawMessage) (types.AgentRun, error) {
	if s.cfg.Runner == nil {
		return types.AgentRun{}, fmt.Errorf("no runner configured")
	}
	runID := uuid.New()
	// Atomically claim the workspace's serial import-step slot BEFORE dispatch (M1):
	// CAS active_run_id from the value the caller observed (ws.ActiveRunID) to this
	// run. Two concurrent verifies (or a verify racing a record) that both saw the
	// slot free cannot both launch — the loser gets errImportStepBusy. This also
	// establishes the fence the verify-result upload enforces (H6). Released on any
	// pre-dispatch failure so a failed launch never bricks re-verify.
	if _, claimed, cerr := s.cfg.Store.ClaimWorkspaceActiveRun(ctx, ws.ID, runID, ws.ActiveRunID); cerr != nil {
		return types.AgentRun{}, fmt.Errorf("claim import-step slot: %w", cerr)
	} else if !claimed {
		return types.AgentRun{}, errImportStepBusy
	}
	release := func(e error) (types.AgentRun, error) {
		_, _ = s.cfg.Store.ClearWorkspaceActiveRun(ctx, ws.ID, runID)
		return types.AgentRun{}, e
	}
	id, err := s.cfg.Identity.MintRunIdentity(ctx, runID, actor, actor, internalAudience)
	if err != nil {
		return release(fmt.Errorf("mint run identity: %w", err))
	}
	cc := s.cfg.DefaultPolicy.MinConfinementClass
	if cc == "" {
		cc = types.CC1
	}
	now := s.cfg.Now().UTC()
	wsID := ws.ID
	run := types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: actor,
		Agent: "claude-code", Task: "workspace verify",
		ConfinementClass: cc, State: types.RunPending, SPIFFEID: id.SPIFFEID,
		RunnerTarget: s.cfg.RunnerTarget,
		WorkspaceID:  &wsID, // trusted linkage for the verify-result upload
	}
	// Source wiring: a repo clones (run.Repo); a local dir is bind-mounted.
	policy := types.RunPolicySpec{
		MinConfinementClass: cc,
		AllowedDomains:      verifyEgressDomains(ws),
		// Above wardyn-verify's 40-min total budget so a legitimately long build
		// isn't idle-reaped mid-run (which would strand the workspace in
		// `verifying`). The verify binary self-bounds the actual work.
		AutoStopAfterSec: 3600,
	}
	ghGrantID, sshGrants := s.wireWorkspaceSource(ctx, runID, now, &run, &policy, ws)
	created, err := s.cfg.Store.CreateRun(ctx, run)
	if err != nil {
		return release(fmt.Errorf("create verify run: %w", err))
	}

	// Run IN the built devcontainer image (build it now if needed — this is
	// Stage 5 folded into verify). Fall back to the convention image if no
	// builder is configured.
	image := agentImage("claude-code", s.cfg.AgentImages)
	if built, ok := s.resolveWorkspaceImage(ctx, runID, ws); ok {
		image = built
	}
	s.dispatchWithVerify(ctx, created, id.Token, image, policy, ghGrantID, nil, sshGrants, nil, false, "", commands)
	created = s.refreshRun(ctx, runID, created)
	return created, nil
}

// launchRecordRun starts one session's interactive sandbox — launchVerifyRun
// with three deltas:
//   - run.Task = "workspace record" (the server-side discriminator: uploads and
//     reconciles branch on it, and it keys the trusted run→workspace linkage);
//   - egress depends on `confined`: a LEARNING session (confined=false) is OPEN
//     (AllowAllEgress=true) so every host the task dials is logged egress.allow
//     (complete capture, no per-domain approvals); a VERIFY session
//     (confined=true) is default-deny, limited to AllowedDomains, so re-running
//     the same steps proves least privilege and off-policy hosts are denied live.
//     AllowedDomains keeps the verify union anyway — credential injection fires
//     ONLY on exact allowlist entries even under allow-all, and clone needs its
//     git hosts; private/metadata IPs stay denied by the unconditional guard;
//   - confinement = the STRONGEST class the wired runner supports (an open
//     sandbox deserves the best isolation available), never the policy floor.
//     weakCC reports when that best is still CC1 so callers warn loudly —
//     refusing would make record unusable on Docker Desktop boxes.
//
// Sessions are always interactive: dispatch passes a nil verify plan, so no
// record run ever execs wardyn-verify or uploads a verify result. The
// sandbox comes up idle for the attach terminal (bounded — an abandoned
// OPEN-egress sandbox must not live forever); the operator's "Done
// recording" is the normal run kill, and capture happens at termination from
// the audit events.
//
// LAUNCH ORDER (concurrency-load-bearing): (1) CAS-claim active_run_id — the
// atomic serial gate; a concurrent step launch that also saw the slot free
// loses the CAS and never launches a sandbox. (2) Upsert the task's
// `recording` entry — BEFORE dispatch, so even a run that dies instantly has
// the entry its terminal capture keys on. (3) Create + dispatch.
func (s *Server) launchRecordRun(ctx context.Context, actor string, ws types.Workspace, sessionKey, sessionLabel, mode string, confined bool) (types.AgentRun, bool, error) {
	if s.cfg.Runner == nil {
		return types.AgentRun{}, false, fmt.Errorf("no runner configured")
	}
	caps, cerr := s.cfg.Runner.Capabilities(ctx)
	if cerr != nil {
		return types.AgentRun{}, false, fmt.Errorf("runner capabilities unavailable: %w", cerr)
	}
	cc := bestClass(caps.ConfinementClasses)
	if cc == "" {
		return types.AgentRun{}, false, fmt.Errorf("runner declares no confinement class")
	}
	weakCC := cc == types.CC1

	runID := uuid.New()
	if _, claimed, cerr := s.cfg.Store.ClaimWorkspaceActiveRun(ctx, ws.ID, runID, ws.ActiveRunID); cerr != nil {
		return types.AgentRun{}, false, fmt.Errorf("claim import-step slot: %w", cerr)
	} else if !claimed {
		return types.AgentRun{}, false, errImportStepBusy
	}
	startedAt := s.cfg.Now().UTC()
	if _, _, perr := s.putRecordResult(ctx, ws.ID, sessionKey, RecordTaskResult{
		RunID: runID, Label: sessionLabel, Mode: mode, Confined: confined, Status: recordStatusRecording, StartedAt: startedAt,
	}, ""); perr != nil {
		_, _ = s.cfg.Store.ClearWorkspaceActiveRun(ctx, ws.ID, runID)
		return types.AgentRun{}, false, fmt.Errorf("persist record state: %w", perr)
	}
	abort := func(reason error) (types.AgentRun, bool, error) {
		now := s.cfg.Now().UTC()
		_, _, _ = s.putRecordResult(ctx, ws.ID, sessionKey, RecordTaskResult{
			RunID: runID, Label: sessionLabel, Mode: mode, Confined: confined, Status: recordStatusFailed, StartedAt: now, FinishedAt: &now,
			FailureHint: "launch failed: " + reason.Error(),
		}, recordStatusRecording)
		_, _ = s.cfg.Store.ClearWorkspaceActiveRun(ctx, ws.ID, runID)
		return types.AgentRun{}, false, reason
	}

	id, err := s.cfg.Identity.MintRunIdentity(ctx, runID, actor, actor, internalAudience)
	if err != nil {
		return abort(fmt.Errorf("mint run identity: %w", err))
	}
	now := s.cfg.Now().UTC()
	wsID := ws.ID
	interactive := mode == recordModeInteractive
	run := types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: actor,
		Agent: "claude-code", Task: "workspace record",
		ConfinementClass: cc, State: types.RunPending, SPIFFEID: id.SPIFFEID,
		RunnerTarget: s.cfg.RunnerTarget,
		WorkspaceID:  &wsID, // trusted linkage for uploads + capture
		Interactive:  interactive,
	}
	// A confined verify escalates an off-policy host to the operator (deny_with_review:
	// raise a pending approval, deny the in-flight probe, and let a retry through once
	// approved) — the direct successor to the legacy forced-true. Deliberately NOT
	// wait_for_review: an unattended verify probe must fail fast, not hang up to the
	// hold deadline. The verify panel's live-approval strip decides it either way. A
	// learning session (allow-all) makes this inert.
	verifyFirstUse := types.FirstUseAlwaysDeny
	if confined {
		verifyFirstUse = types.FirstUseDenyWithReview
	}
	policy := types.RunPolicySpec{
		MinConfinementClass: cc,
		// A VERIFY session (confined) is default-deny, limited to AllowedDomains
		// (baseline clone/registry hosts ∪ the workspace's approved egress) — so
		// re-running the same steps proves they work under least privilege. A
		// learning session (open) allows all egress so the capture is complete.
		// Same interactive attach either way.
		AllowAllEgress: !confined,
		AllowedDomains: verifyEgressDomains(ws),
		// In a confined verify, an off-policy host ESCALATES to the operator instead
		// of a silent hard-deny — so a "bad curl" surfaces an approve/reject decision
		// in the verify panel as it happens. Inert under allow-all, so it's a no-op
		// for a learning session. (Cloud-metadata / private IPs stay unconditionally
		// blocked regardless.)
		FirstUseApproval: verifyFirstUse,
		// Auto: above wardyn-verify's 40-min budget (same rationale as verify).
		// Interactive: generous but FINITE idle cap — an abandoned open-egress
		// recording self-terminates (and revokes) instead of living forever.
		AutoStopAfterSec: 3600,
	}
	if interactive {
		policy.AutoStopAfterSec = int(recordInteractiveIdleCap.Seconds())
	}
	ghGrantID, sshGrants := s.wireWorkspaceSource(ctx, runID, now, &run, &policy, ws)
	created, err := s.cfg.Store.CreateRun(ctx, run)
	if err != nil {
		return abort(fmt.Errorf("create record run: %w", err))
	}

	// Record in the built devcontainer image so the task actually runs (its
	// toolchain isn't in the convention agent image) — same lane as verify.
	image := agentImage("claude-code", s.cfg.AgentImages)
	if built, ok := s.resolveWorkspaceImage(ctx, runID, ws); ok {
		image = built
	}

	// The model provider is part of the HARNESS the operator configured (getting
	// started), not per-workspace app egress they approve — so its host must be
	// reachable in EVERY agent session, confined verify included. A learning session
	// is AllowAllEgress so it's fine; a confined verify's AllowedDomains is
	// baseline+approved and would NOT list api.anthropic.com, which makes
	// applyLLMCredMount refuse the subscription mount (anthropicReachable=false) and
	// silently fall back to a broken api-key path. Union the ceiling's model-provider
	// egress in first so subscription/api-key wiring below attaches in both modes.
	unionAllowedDomains(&policy, modelProviderEgress(s.cfg.DefaultPolicy))

	// Model access for the session: a recording session can drive the agent, so wire
	// the operator's CONFIGURED provider into the run. Reuse the composer's helpers —
	// dispatch then auto-provisions the rest from the resulting policy (no runs.go
	// change): subscription is detected from the /home/agent/.claude mount, and
	// Bedrock is wired by dispatch's resolveBedrockAuth when modelRun is true (an
	// interactive record run has verifyPlan==nil, so modelRun is true).
	var injections []runner.InjectionGrant
	subMounted, _ := applyLLMCredMount(&policy, s.cfg.DefaultPolicy, "claude-code", true)
	llmMode := "none"
	if subMounted {
		llmMode = "subscription"
	}
	if !subMounted {
		// No subscription ceiling: fall back to a brokered api-key grant when the
		// provider secret exists (ensureLLMGrant is a no-op otherwise). Then build
		// the injection from that grant — mirrors handleCreateRun's api_key branch —
		// and hand it to dispatch (record otherwise passes no injections).
		presentSecrets := map[string]bool{}
		if s.cfg.Secrets != nil {
			if names, nerr := s.listUserSecretNames(ctx); nerr == nil {
				for _, n := range names {
					presentSecrets[n] = true
				}
			}
		}
		ensureLLMGrant(&policy, "claude-code", presentSecrets, false)
		for _, g := range policy.EligibleGrants {
			if g.Kind != types.GrantAPIKey || g.RequiresApproval {
				continue
			}
			grantID := uuid.New()
			if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
				ID: grantID, RunID: runID, CreatedAt: now, Spec: g,
			}); gerr != nil {
				return abort(fmt.Errorf("create llm grant: %w", gerr))
			}
			if rule, rerr := injectionRuleFromScope(g.Scope); rerr == nil {
				injections = append(injections, runner.InjectionGrant{GrantID: grantID, Rule: rule})
			}
		}
		if len(injections) > 0 {
			llmMode = "api-key"
		}
	}

	// Save the resolved auth mode + model onto the session entry so it's visible and a
	// verify replay reflects the SAME provider the operator configured (not a guess).
	// Guarded on `recording`: a superseding re-record must not resurrect this entry.
	_, _, _ = s.putRecordResult(ctx, ws.ID, sessionKey, RecordTaskResult{
		RunID: runID, Label: sessionLabel, Mode: mode, Confined: confined,
		Status: recordStatusRecording, StartedAt: startedAt,
		LLMMode: llmMode, Model: s.cfg.AgentAnthropicModel,
	}, recordStatusRecording)

	// Sessions are interactive (the operator drives the activity in the attach
	// shell); no auto command plan. The `--idle` path clones the repo + attaches.
	var plan json.RawMessage
	s.dispatchWithVerify(ctx, created, id.Token, image, policy, ghGrantID, nil, sshGrants, injections, interactive, "", plan)
	created = s.refreshRun(ctx, runID, created)
	return created, weakCC, nil
}

// wireWorkspaceSource points run+policy at the workspace source: a repo
// clones (run.Repo, with best-effort GitHub/SSH read grants); a local dir is
// bind-mounted at the composer workspace target. Shared by launchVerifyRun
// and launchRecordRun.
func (s *Server) wireWorkspaceSource(ctx context.Context, runID uuid.UUID, now time.Time, run *types.AgentRun, policy *types.RunPolicySpec, ws types.Workspace) (ghGrantID *uuid.UUID, sshGrants map[string]string) {
	if ws.Kind == types.WorkspaceKindRepo {
		run.Repo = ws.Source
		if u := repoCloneURL(ws.Source); u != "" {
			if grant := s.maybeGitHubReadGrant(ctx, runID, now, u); grant != nil {
				ghGrantID = grant
			}
			sshGrants = s.maybeSSHKeyGrant(ctx, runID, now, u)
		}
	} else {
		// ReadOnly is a *bool whose SAFE DEFAULT is read-only when omitted. Omitting
		// it here mounted every imported workspace read-only, which made the Record
		// step's own promise ("so the agent can make changes") impossible to keep:
		// `pnpm install` cannot write node_modules, a build cannot emit artifacts,
		// and no source file can be edited — i.e. no contribution, from the very flow
		// that exists to record one. Honor the operator's explicit per-workspace
		// opt-in instead, mirroring the composer's ws.ReadWrite; the default is still
		// read-only, so this widens nothing unless a human ticked the box.
		ro := !ws.Writable
		policy.WorkspaceMounts = []types.WorkspaceMount{
			{Source: ws.Source, Target: composerWorkspaceTarget, ReadOnly: &ro},
		}
		run.WorkspacePath = ws.Source
	}
	return ghGrantID, sshGrants
}

// maybeGitHubReadGrant creates a read-only github_token grant for a github.com
// clone URL (nil otherwise / on failure) — extracted from launchScanRun's
// private-repo clone support so launchVerifyRun can reuse it.
func (s *Server) maybeGitHubReadGrant(ctx context.Context, runID uuid.UUID, now time.Time, cloneURL string) *uuid.UUID {
	u, perr := neturl.Parse(cloneURL)
	if perr != nil || u.Hostname() != "github.com" {
		return nil
	}
	gid := uuid.New()
	scope, _ := json.Marshal(map[string]any{
		"repos": []string{}, "permissions": map[string]string{"contents": "read"},
	})
	if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
		ID: gid, RunID: runID, CreatedAt: now,
		Spec: types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, TTLSeconds: 600},
	}); gerr != nil {
		return nil
	}
	return &gid
}

// maybeSSHKeyGrant synthesizes a run-scoped ssh_key grant for an SSH/scp clone
// URL — the SSH analog of maybeGitHubReadGrant — so onboarding an SSH workspace
// needs no manually-created grant. It returns the host→grant-id map dispatch
// marshals into WARDYN_SSH_GRANTS (nil when the source isn't an SSH URL to a
// supported provider, or the operator hasn't stored the ssh-key-<host> secret).
// The grant's host is the host git actually dials (so the sandbox ssh_config
// stanza matches), while key_secret_ref is the CANONICAL ssh-key-<host> secret
// (so either the github.com or ssh.github.com URL form resolves one stored key).
func (s *Server) maybeSSHKeyGrant(ctx context.Context, runID uuid.UUID, now time.Time, cloneURL string) map[string]string {
	host, ok := sshCloneHost(cloneURL)
	if !ok {
		return nil
	}
	if _, ok := sshOver443Endpoint(host); !ok {
		return nil
	}
	secretName, ok := canonicalSSHKeySecret(host)
	if !ok {
		return nil
	}
	// Only synthesize a grant when the key is actually present — otherwise the
	// clone would fail; the onboarding guard (below) rejects that case up front.
	names, err := s.listUserSecretNames(ctx)
	if err != nil {
		return nil
	}
	if !slices.Contains(names, secretName) {
		return nil
	}
	gid := uuid.New()
	scope, _ := json.Marshal(map[string]any{"host": host, "key_secret_ref": secretName})
	if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
		ID: gid, RunID: runID, CreatedAt: now,
		Spec: types.GrantSpec{Kind: types.GrantSSHKey, Scope: scope, TTLSeconds: 600},
	}); gerr != nil {
		return nil
	}
	return map[string]string{host: gid.String()}
}

// reconcileWorkspaceRun is called when a governed scan/verify run reaches a
// terminal state. If the workspace is STILL in the in-flight state pointing at
// this run — meaning the run's result upload never arrived (the scan/verify
// binary uploads BEFORE the process exits, so by the time the completion
// watcher fires a successful upload has already advanced the workspace) — it
// reconciles the workspace out of the stuck state instead of leaving it hung.
// The common cause is a sandbox that cannot reach the control plane (e.g.
// Docker-Desktop/WSL2 NAT networking).
func (s *Server) reconcileWorkspaceRun(ctx context.Context, runID uuid.UUID) {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || run.WorkspaceID == nil {
		return
	}
	ws, err := s.cfg.Store.GetWorkspace(ctx, *run.WorkspaceID)
	if err != nil {
		return
	}
	// Only reconcile when THIS run is still the in-flight one (a newer run or a
	// landed upload would have cleared/changed active_run_id or the status).
	if ws.ActiveRunID == nil || *ws.ActiveRunID != runID {
		return
	}
	switch ws.Status {
	case types.WorkspaceVerifying:
		// The verify run ended but no result arrived — record a synthetic failure
		// so the operator sees a clear reason instead of an endless spinner.
		vr := workspacescan.VerifyResult{
			Ran: true, OK: false, Done: true,
			Steps: []workspacescan.VerifyStepResult{{
				Stage: "run", Command: run.Task, ExitCode: -1,
				LogTail: "the verify sandbox finished but its result never reached the control plane — " +
					"the sandbox likely cannot reach wardynd (check sandbox→control-plane networking, " +
					"e.g. Docker Desktop + WSL2 requires mirrored networking or a containerized control plane).",
			}},
		}
		_, _ = s.cfg.Store.SetWorkspaceImportState(ctx, ws.ID, types.WorkspaceVerifyFailed, nil,
			mustJSON(vr), ws.VerifiedProfileHash, ws.VerifiedAt)
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "workspace.verify",
			ws.ID.String(), "failure", mustJSON(map[string]any{"reason": "no_result_uploaded"})))
	case types.WorkspaceScanning:
		// A repo scan run ended without uploading facts — leave a clear error.
		ws.Status = types.WorkspaceError
		ws.ActiveRunID = nil
		_, _ = s.cfg.Store.UpdateWorkspace(ctx, ws.ID, ws)
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "workspace.scan",
			ws.ID.String(), "failure", mustJSON(map[string]any{"reason": "no_facts_uploaded"})))
	}
}

// recordEmptyCaptureHint explains a capture that produced ZERO egress evidence.
// This is a FAILURE, never "the task needs no egress": the (open) recording
// exists to observe, and the common cause of observing nothing is the proxy's
// decision callbacks not reaching the control plane, not a network-free task.
const recordEmptyCaptureHint = "no egress evidence was captured for this recording — most commonly the " +
	"proxy sidecar's decision callbacks cannot reach the control plane (Docker Desktop + WSL2 NAT needs " +
	"mirrored networking or the compose stack, `make setup`). Treat this recording as failed, NOT as " +
	"proof the task needs no egress."

// reconcileRecordRun captures a record run's evidence when it reaches a
// terminal state — for ANY reason: auto completion, the operator's "Done
// recording" kill, or a boot reconcile. Capture is server-side and pure
// (recordmode.Capture over the run's already-persisted audit events — never a
// sandbox upload), so it works identically for auto and interactive runs and
// for killed ones. Idempotent: only the task entry still in `recording` and
// pointing at this run is finalized. Record NEVER touches workspace status or
// the verify fields; it only writes its own record_results entry and clears
// the active-run pointer.
func (s *Server) reconcileRecordRun(ctx context.Context, runID uuid.UUID) {
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil || run.WorkspaceID == nil || run.Task != "workspace record" {
		return
	}
	ws, err := s.cfg.Store.GetWorkspace(ctx, *run.WorkspaceID)
	if err != nil {
		return
	}
	taskKey := ""
	var res RecordTaskResult
	for k, v := range recordResultsMap(ws) {
		if v.RunID == runID {
			taskKey, res = k, v
			break
		}
	}
	if taskKey == "" || res.Status != recordStatusRecording {
		return // a newer recording superseded this run, or already finalized
	}

	events, err := s.cfg.Store.QueryAuditEvents(ctx, runID, 0)
	if err != nil {
		return // transient store failure: leave `recording`; a later reconcile retries
	}
	obs := recordmode.Capture(events)
	now := s.cfg.Now().UTC()
	res.FinishedAt = &now
	res.Observations = &obs
	res.KernelSensorBlind = run.ConfinementClass == types.CC3
	res.Caveats = []string{recordMaskingCaveat}
	if len(obs.Domains) == 0 {
		res.Status = recordStatusFailed
		res.FailureHint = recordEmptyCaptureHint
	} else {
		res.Status = recordStatusRecorded
		res.SecretNamesMinted = s.mintedSecretNames(ctx, runID, obs.MintedGrantIDs)
	}
	// Compare-and-set on `recording`: idempotent across the watcher/kill/boot/
	// read-repair triggers, and a capture that lost to a concurrent finalizer
	// (or a superseding re-record) writes and audits nothing.
	_, applied, perr := s.putRecordResult(ctx, ws.ID, taskKey, res, recordStatusRecording)
	if perr != nil || !applied {
		return
	}
	// Release the serial import-step slot — conditional, so a step that was
	// concurrently launched and now owns the pointer is never clobbered.
	_, _ = s.cfg.Store.ClearWorkspaceActiveRun(ctx, ws.ID, runID)
	// Counts-only audit — observations stay in the workspace row, never in audit.
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "workspace.record",
		ws.ID.String(), outcomeBool(res.Status == recordStatusRecorded), mustJSON(map[string]any{
			"task": taskKey, "mode": res.Mode, "domains": len(obs.Domains),
			"minted_grants": len(obs.MintedGrantIDs), "anomalies": len(obs.Anomalies),
			"kernel_sensor_blind": res.KernelSensorBlind,
		})))
}

// mintedSecretNames resolves minted grant ids to operator-meaningful names for
// the "proven used" render: an api_key grant's secret_name, otherwise the grant
// kind. Deduped, sorted (stable render), never values.
func (s *Server) mintedSecretNames(ctx context.Context, runID uuid.UUID, minted []uuid.UUID) []string {
	if len(minted) == 0 {
		return nil
	}
	grants, err := s.cfg.Store.ListGrantsByRun(ctx, runID)
	if err != nil {
		return nil
	}
	byID := make(map[uuid.UUID]types.GrantSpec, len(grants))
	for _, g := range grants {
		byID[g.ID] = g.Spec
	}
	seen := map[string]bool{}
	for _, id := range minted {
		spec, ok := byID[id]
		if !ok {
			continue
		}
		name := string(spec.Kind)
		if spec.Kind == types.GrantAPIKey {
			var scope struct {
				SecretName string `json:"secret_name"`
			}
			if json.Unmarshal(spec.Scope, &scope) == nil && scope.SecretName != "" {
				name = scope.SecretName
			}
		}
		seen[name] = true
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// launchScanRun starts a throwaway GOVERNED run that clones a repo workspace and
// runs wardyn-scan (scan-only mode) instead of an agent. wardyn-scan uploads
// ScanFacts to the brokered scan-results route; that endpoint derives + persists
// the profile onto this workspace via the TRUSTED run→workspace linkage
// (run.WorkspaceID) — never from sandbox input. Mirrors handleCreateRun's
// mint → CreateRun → dispatch flow, minus the request surface: no grants, no user
// mounts, a minimal git-egress policy, no model call. Returns the created run.
func (s *Server) launchScanRun(ctx context.Context, actor string, ws types.Workspace) (types.AgentRun, error) {
	if s.cfg.Runner == nil {
		return types.AgentRun{}, fmt.Errorf("no runner configured")
	}
	url := repoCloneURL(ws.Source)
	if url == "" {
		return types.AgentRun{}, fmt.Errorf("repo %q has no derivable clone URL", ws.Source)
	}

	runID := uuid.New()
	// Claim the workspace's serial import-step slot BEFORE dispatch (H14): the scan
	// self-heal in reconcileWorkspaceRun keys on ws.ActiveRunID == runID, so a scan
	// that never claims the slot leaves that branch DEAD — a scan whose facts upload
	// is lost (e.g. sandbox can't reach the control plane) then strands the workspace
	// in `scanning` forever. Mirrors record/verify. Released on any pre-dispatch fail.
	claimedWS, claimed, cerr := s.cfg.Store.ClaimWorkspaceActiveRun(ctx, ws.ID, runID, ws.ActiveRunID)
	if cerr != nil {
		return types.AgentRun{}, fmt.Errorf("claim import-step slot: %w", cerr)
	} else if !claimed {
		return types.AgentRun{}, errImportStepBusy
	}
	scanRelease := func(e error) (types.AgentRun, error) {
		_, _ = s.cfg.Store.ClearWorkspaceActiveRun(ctx, ws.ID, runID)
		return types.AgentRun{}, e
	}
	id, err := s.cfg.Identity.MintRunIdentity(ctx, runID, actor, actor, internalAudience)
	if err != nil {
		return scanRelease(fmt.Errorf("mint run identity: %w", err))
	}
	// Confinement: inherit the operator's default floor. A scan is read-only,
	// ephemeral, and holds no credentials; the operator's floor still governs.
	cc := s.cfg.DefaultPolicy.MinConfinementClass
	if cc == "" {
		cc = types.CC1
	}
	now := s.cfg.Now().UTC()
	wsID := ws.ID
	run := types.AgentRun{
		ID:               runID,
		CreatedAt:        now,
		UpdatedAt:        now,
		CreatedBy:        actor,
		Agent:            "claude-code", // carries wardyn-scan + git; no model call in scan-only mode
		Repo:             ws.Source,
		Task:             "workspace scan",
		ConfinementClass: cc,
		State:            types.RunPending,
		SPIFFEID:         id.SPIFFEID,
		RunnerTarget:     s.cfg.RunnerTarget,
		WorkspaceID:      &wsID, // marks this a scan run + the trusted linkage
	}
	created, err := s.cfg.Store.CreateRun(ctx, run)
	if err != nil {
		return scanRelease(fmt.Errorf("create scan run: %w", err))
	}

	// Flip the workspace to `scanning` so the import UI's poll (which watches only the
	// transient statuses) opens and clears its spinner when the async scan run uploads
	// its profile. Without this the workspace stays `pending_scan` for the whole run
	// and the UI never re-checks — the scan spinner hangs even after the scan finishes.
	// Set BEFORE dispatch so a fast scan's `scanned` upload can't be regressed. Best
	// effort: the scan still completes and sets `scanned` regardless of this update.
	// Use the post-claim workspace (active_run_id already == runID) as the base so
	// this status write preserves the slot we just claimed (H14).
	scanningWS := claimedWS
	scanningWS.Status = types.WorkspaceScanning
	_, _ = s.cfg.Store.UpdateWorkspace(ctx, ws.ID, scanningWS)

	// PRIVATE-repo support: for a GitHub repo, create a read-only github_token grant
	// so wardyn-git-helper can mint a clone credential at clone time. Non-approval +
	// contents:read (least privilege for a clone). It fails CLOSED when no
	// GitHubMinter is configured; a PUBLIC repo clones credential-free regardless.
	// Non-GitHub private hosts would need a git_pat grant (a further follow-up).
	ghGrantID := s.maybeGitHubReadGrant(ctx, runID, now, url)
	// SSH clone URL: synthesize the ssh_key grant + surface it as WARDYN_SSH_GRANTS.
	sshGrants := s.maybeSSHKeyGrant(ctx, runID, now, url)

	// Minimal scan policy: allow only the git host(s) the clone needs + a short
	// auto-stop. No workspace mounts, no subscription — wardyn-scan uploads to the
	// proxy's brokered route (not egress).
	scanPolicy := types.RunPolicySpec{
		MinConfinementClass: cc,
		AllowedDomains:      scanEgressDomains(url),
		AutoStopAfterSec:    600,
	}

	image := agentImage("claude-code", s.cfg.AgentImages)
	s.dispatch(ctx, created, id.Token, image, scanPolicy, ghGrantID, nil, sshGrants, nil, false, "")
	created = s.refreshRun(ctx, runID, created)
	return created, nil
}

// scanEgressDomains returns the egress allowlist a scan run needs to clone
// cloneURL. GitHub's clone hosts cover the bare-slug + github.com case; a full
// https URL to another host also gets that host. The proxy still enforces the
// deny-list underneath.
func scanEgressDomains(cloneURL string) []string {
	doms := []string{"github.com", "api.github.com", "codeload.github.com", "*.githubusercontent.com"}
	// SSH/scp clone URL: add the PORT-QUALIFIED SSH-over-443 endpoint
	// (ssh.github.com:443) so it matches ONLY :443 — not the bare host, which would
	// match any port. Mirrors handleCreateRun's sshEgress lane. neturl.Parse cannot
	// parse scp-form, so this must come first.
	if host, ok := sshCloneHost(cloneURL); ok {
		// A git-over-SSH clone needs ONLY the :443 endpoint — the ADO REST bundle
		// (dev.azure.com / *.visualstudio.com) is the git_pat HTTPS lane, not this
		// one. Matches handleCreateRun's sshEgress (endpoint only). Least privilege.
		if ep, ok := sshOver443Endpoint(host); ok {
			doms = append(doms, ep)
		}
		return doms
	}
	if u, err := neturl.Parse(cloneURL); err == nil && u.Hostname() != "" {
		host := u.Hostname() // strips any :port
		if !isGitHubCloneHost(host) {
			// M22: a non-GitHub HTTPS clone (ADO, GitLab, self-hosted git) needs ONLY
			// its own host — the default GitHub bundle above is an unnecessary
			// over-grant for a non-GitHub scan. GitHub hosts keep the full bundle
			// (codeload / LFS / *.githubusercontent.com).
			return []string{host}
		}
	}
	return doms
}

// isGitHubCloneHost reports whether host is a GitHub clone/API host (github.com,
// ssh.github.com, or a *.github.com subdomain), case-insensitively.
func isGitHubCloneHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	return h == "github.com" || h == "ssh.github.com" || strings.HasSuffix(h, ".github.com")
}
