// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	neturl "net/url"
	"slices"
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
// workspace id. The lookup is by workspaceSourceIndex (workspace_refs.go),
// built from EVERY workspace's Sources — not the single-source Kind/Source
// mirror, which is empty for a multi-source workspace.
func (s *Server) referencedWorkspaces(ctx context.Context, spec types.RunPolicySpec) []types.Workspace {
	if s.cfg.Store == nil {
		return nil
	}
	all, err := s.cfg.Store.ListWorkspaces(ctx)
	if err != nil {
		return nil
	}
	idx := indexWorkspacesBySource(all)
	var out []types.Workspace
	seen := map[uuid.UUID]bool{}
	add := func(ws types.Workspace, ok bool) {
		if !ok || seen[ws.ID] {
			return
		}
		seen[ws.ID] = true
		out = append(out, ws)
	}
	for _, wm := range spec.WorkspaceMounts {
		if systemMountTargets[wm.Target] {
			continue
		}
		ws, ok := idx.localDir[wm.Source]
		add(ws, ok)
	}
	for _, wr := range spec.WorkspaceRepos {
		ws, ok := idx.repo[wr.Repo]
		add(ws, ok)
	}
	return out
}

// workspaceSourcesOfType filters ws.Sources down to entries of typ, preserving
// order.
func workspaceSourcesOfType(ws types.Workspace, typ types.WorkspaceSourceType) []types.WorkspaceSource {
	var out []types.WorkspaceSource
	for _, src := range ws.Sources {
		if src.Type == typ {
			out = append(out, src)
		}
	}
	return out
}

// firstRepoSource returns the workspace's first repo-type source. A workspace
// can carry more than one; the governed scan run — one clone, one uploaded
// profile via SetWorkspaceScanResult's wholesale replace — only ever scans ONE
// repo per run today.
// ponytail: multi-repo scan-and-merge (like the local_dir aggregate scan in
// handleScanWorkspace) is the upgrade path if a workspace with several repo
// sources ever needs each one profiled.
func firstRepoSource(ws types.Workspace) (types.WorkspaceSource, bool) {
	for _, src := range ws.Sources {
		if src.Type == types.WorkspaceSourceTypeRepo {
			return src, true
		}
	}
	return types.WorkspaceSource{}, false
}

// mergeWorkspaceProfiles combines N local_dir sources' individually-scanned
// profiles into ONE profile for the workspace: union the set-like fields
// (languages, package managers, egress, tools, required secrets, services,
// suggested egress, secret-file paths), concatenate leak findings and setup
// commands (never drop a suspected secret or an install step), take the
// largest build-memory hint, and take the LOWEST confidence (one ambiguous
// source makes the whole workspace's profile suspect). Empty input returns
// the zero profile; a single profile is returned unchanged.
func mergeWorkspaceProfiles(profiles []workspacescan.WorkspaceProfile) workspacescan.WorkspaceProfile {
	if len(profiles) == 0 {
		return workspacescan.WorkspaceProfile{}
	}
	if len(profiles) == 1 {
		return profiles[0]
	}
	langs, pkgMgrs, egress, tools := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	services, suggested, secretFiles := map[string]struct{}{}, map[string]struct{}{}, map[string]struct{}{}
	github, otherHosts := map[string]struct{}{}, map[string]struct{}{}
	secretByName := map[string]workspacescan.SecretNeed{}
	var leaks []workspacescan.LeakFinding
	var setupCmds []workspacescan.SetupCommand
	hasDevcontainer, hasDockerfile, needsReview := false, false, false
	buildMemMiB := 0
	confidenceRank := map[string]int{
		workspacescan.ConfidenceHigh: 3, workspacescan.ConfidenceMedium: 2, workspacescan.ConfidenceLow: 1,
	}
	lowest := workspacescan.ConfidenceHigh

	addAll := func(dst map[string]struct{}, xs []string) {
		for _, x := range xs {
			dst[x] = struct{}{}
		}
	}
	for _, p := range profiles {
		addAll(langs, p.Languages)
		addAll(pkgMgrs, p.PackageManagers)
		addAll(egress, p.EgressDomains)
		addAll(tools, p.Tools)
		addAll(services, p.ServicesNeeded)
		addAll(suggested, p.SuggestedEgress)
		addAll(secretFiles, p.SecretFilesPresent)
		addAll(github, p.GitRemotes.GitHub)
		addAll(otherHosts, p.GitRemotes.OtherHosts)
		for _, n := range p.RequiredSecrets {
			if _, dup := secretByName[n.Name]; !dup {
				secretByName[n.Name] = n
			}
		}
		leaks = append(leaks, p.LeakFindings...)
		setupCmds = append(setupCmds, p.SetupCommands...)
		hasDevcontainer = hasDevcontainer || p.HasDevcontainer
		hasDockerfile = hasDockerfile || p.HasDockerfile
		needsReview = needsReview || p.NeedsReview
		if p.BuildMemoryMiB > buildMemMiB {
			buildMemMiB = p.BuildMemoryMiB
		}
		if confidenceRank[p.Confidence] < confidenceRank[lowest] {
			lowest = p.Confidence
		}
	}
	requiredSecrets := make([]workspacescan.SecretNeed, 0, len(secretByName))
	for _, n := range secretByName {
		requiredSecrets = append(requiredSecrets, n)
	}
	slices.SortFunc(requiredSecrets, func(a, b workspacescan.SecretNeed) int { return strings.Compare(a.Name, b.Name) })

	return workspacescan.WorkspaceProfile{
		Languages:          sortedKeys(langs),
		PackageManagers:    sortedKeys(pkgMgrs),
		EgressDomains:      sortedKeys(egress),
		Tools:              sortedKeys(tools),
		GitRemotes:         workspacescan.GitRemotes{GitHub: sortedKeys(github), OtherHosts: sortedKeys(otherHosts)},
		HasDevcontainer:    hasDevcontainer,
		HasDockerfile:      hasDockerfile,
		RequiredSecrets:    requiredSecrets,
		ServicesNeeded:     sortedKeys(services),
		SuggestedEgress:    sortedKeys(suggested),
		SecretFilesPresent: sortedKeys(secretFiles),
		BuildMemoryMiB:     buildMemMiB,
		LeakFindings:       leaks,
		SetupCommands:      setupCmds,
		Confidence:         lowest,
		NeedsReview:        needsReview,
		Source:             workspacescan.SourceDeterministic,
	}
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

// resolveWorkspaceImage returns the sandbox image for a run driven by its PRIMARY
// onboarded workspace, or ok=false to fall through to the convention image.
// Order (all fail-OPEN — any failure returns ok=false + convention image, never
// blocks the run):
//   - an explicit BaseImage CHOICE on the workspace ("registry"/"byo"/"custom") →
//     use it (see below);
//   - a REPO PRIMARY source (Sources[0]) whose profile HasDevcontainer → build
//     the repo's own devcontainer;
//   - a cached generated image still valid for the current profile hash → reuse;
//   - else generate a devcontainer for the detected toolchain, build it, and cache
//     image_ref + built_profile_hash on the workspace for reuse.
//
// It audits its own build success/failure against runID.
func (s *Server) resolveWorkspaceImage(ctx context.Context, runID uuid.UUID, primary types.Workspace) (string, bool) {
	if s.cfg.ImageBuilder == nil {
		return "", false
	}

	buildAudit := func(outcome string, extra map[string]any) {
		extra["workspace_id"] = primary.ID.String()
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.build",
			runID.String(), outcome, mustJSON(extra)))
	}

	// An explicit base-image CHOICE takes precedence over everything below.
	// "recommended" (Wardyn's own convention image for the detected stack) —
	// like a nil BaseImage — falls through to the devcontainer/generated path
	// instead of a fixed ref.
	if b := primary.BaseImage; b != nil && b.Kind != "recommended" && strings.TrimSpace(b.Image) != "" {
		// "custom" additionally layers b.Steps on the base via a Dockerfile
		// build. ponytail: no builder method layers Dockerfile lines on a base
		// image yet, so this falls back to Image verbatim (Steps ignored) —
		// same as "registry"/"byo" ("used verbatim, no layering") — until one
		// exists.
		buildAudit("success", map[string]any{"source": "base_image:" + b.Kind, "image": b.Image})
		return b.Image, true
	}

	p, ok := workspaceProfile(primary)
	if !ok {
		return "", false // unscanned/malformed → convention image
	}

	// A repo PRIMARY source (Sources[0]) carrying its OWN devcontainer: respect
	// it, build from the repo — UNLESS the source is an SSH URL. The image
	// builder (envbuilder) clones with no minted key / known_hosts / :443
	// ProxyCommand — only the agent-run sandbox has that wiring — so an SSH
	// devcontainer build would fail auth. Fall through to a generated toolchain
	// image; agent-run still clones the repo itself using the run's ssh_key
	// grant (the repo's own devcontainer is just not built in v1).
	if len(primary.Sources) > 0 && primary.Sources[0].Type == types.WorkspaceSourceTypeRepo && p.HasDevcontainer {
		repoSrc := primary.Sources[0]
		if _, ssh := sshCloneHost(repoSrc.Source); ssh {
			buildAudit("skipped", map[string]any{"source": "repo-devcontainer", "reason": "ssh-source-not-buildable-by-image-builder"})
		} else if url := repoCloneURL(repoSrc.Source); url != "" {
			tag := "wardyn-workspace/" + primary.ID.String() + ":devcontainer"
			if built, err := s.cfg.ImageBuilder.BuildDevcontainer(ctx, url, repoSrc.Ref, tag); err == nil {
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

// claimImportStep atomically claims the workspace's serial import-step slot for
// runID, CAS-ing active_run_id from the value the caller observed (M1/H14): two
// concurrent step launches that both saw the slot free cannot both dispatch —
// the loser gets errImportStepBusy. It returns the CLAIMED workspace row (the
// base any pre-dispatch status write must build on so it preserves the claim)
// and the release compensator EVERY pre-dispatch failure path must return
// through — it fails the persisted-but-undispatched run and frees the slot, so a
// failed launch never bricks the next step (failAndRevoke's CAS is conditional
// on PENDING, so it no-ops pre-CreateRun).
func (s *Server) claimImportStep(ctx context.Context, ws types.Workspace, runID uuid.UUID) (types.Workspace, func(error) error, error) {
	claimed, ok, err := s.cfg.Store.ClaimWorkspaceActiveRun(ctx, ws.ID, runID, ws.ActiveRunID)
	if err != nil {
		return types.Workspace{}, nil, fmt.Errorf("claim import-step slot: %w", err)
	}
	if !ok {
		return types.Workspace{}, nil, errImportStepBusy
	}
	return claimed, func(e error) error {
		s.failAndRevoke(ctx, runID, types.RunPending)
		_, _ = s.cfg.Store.ClearWorkspaceActiveRun(ctx, ws.ID, runID)
		return e
	}, nil
}

// newWorkspaceStepRun mints the run identity and builds the run row every
// workspace step run (scan/verify/record) shares: PENDING, claude-code, and
// linked to ws through WorkspaceID — the TRUSTED linkage each step's upload
// authorises on, never sandbox input. Callers set only what differs (Repo,
// Interactive, AutoStopAfterSec) and take run.CreatedAt as the launch clock.
func (s *Server) newWorkspaceStepRun(ctx context.Context, runID uuid.UUID, actor, task string, ws types.Workspace, cc types.ConfinementClass) (types.AgentRun, string, error) {
	id, err := s.cfg.Identity.MintRunIdentity(ctx, runID, actor, actor, internalAudience)
	if err != nil {
		return types.AgentRun{}, "", fmt.Errorf("mint run identity: %w", err)
	}
	now := s.cfg.Now().UTC()
	wsID := ws.ID
	return types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: actor,
		Agent: "claude-code", Task: task,
		ConfinementClass: cc, State: types.RunPending, SPIFFEID: id.SPIFFEID,
		RunnerTarget: s.cfg.RunnerTarget,
		WorkspaceID:  &wsID,
	}, id.Token, nil
}

// defaultFloorClass is the operator's configured confinement floor (CC1 when
// unset) — what a scan/verify inherits, as opposed to record's strongest-class.
func (s *Server) defaultFloorClass() types.ConfinementClass {
	if cc := s.cfg.DefaultPolicy.MinConfinementClass; cc != "" {
		return cc
	}
	return types.CC1
}

// workspaceRunImage is the image a verify/record run executes in: the
// workspace's BUILT devcontainer image (built now if needed), falling back to
// the convention agent image when no builder is configured.
func (s *Server) workspaceRunImage(ctx context.Context, runID uuid.UUID, ws types.Workspace) string {
	if built, ok := s.resolveWorkspaceImage(ctx, runID, ws); ok {
		return built
	}
	return agentImage("claude-code", s.cfg.AgentImages)
}

// dispatchAndSettle is the shared launch tail: dispatch, re-read the run so the
// caller returns the store's freshest row, and settle a launch that already
// reached a terminal state (see settleTerminalLaunch).
func (s *Server) dispatchAndSettle(ctx context.Context, created types.AgentRun, p dispatchParams) types.AgentRun {
	s.dispatchRun(ctx, created, p)
	created = s.refreshRun(ctx, created.ID, created)
	s.settleTerminalLaunch(ctx, created.ID, created)
	return created
}

// launchRecordRun starts one session's interactive sandbox.
//   - run.Task = "workspace record" (the server-side discriminator: uploads and
//     reconciles branch on it, and it keys the trusted run→workspace linkage);
//   - egress depends on `confined`: a LEARNING session (confined=false) is OPEN
//     (AllowAllEgress=true) so every host the task dials is logged egress.allow
//     (complete capture, no per-domain approvals); a CONFINED REPLAY session
//     (confined=true) is default-deny, limited to AllowedDomains, so re-running
//     the same steps proves least privilege and off-policy hosts are denied live.
//     AllowedDomains keeps the confined-egress union anyway — credential
//     injection fires ONLY on exact allowlist entries even under allow-all, and
//     clone needs its git hosts; private/metadata IPs stay denied by the
//     unconditional guard;
//   - confinement = the STRONGEST class the wired runner supports (an open
//     sandbox deserves the best isolation available), never the policy floor.
//     weakCC reports when that best is still CC1 so callers warn loudly —
//     refusing would make record unusable on Docker Desktop boxes.
//
// Sessions are always interactive: the sandbox comes up idle for the attach
// terminal (bounded — an abandoned OPEN-egress sandbox must not live forever);
// the operator's "Done recording" is the normal run kill, and capture happens
// at termination from the audit events.
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
	// Detach from request cancellation before the durable launch work + image
	// build: a client that walks away mid-build must not cancel it (dispatch's
	// own WithoutCancel lands too late to protect the pre-dispatch work above it).
	ctx = context.WithoutCancel(ctx)
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
	_, release, err := s.claimImportStep(ctx, ws, runID)
	if err != nil {
		return types.AgentRun{}, false, err
	}
	startedAt := s.cfg.Now().UTC()
	if _, _, perr := s.putRecordResult(ctx, ws.ID, sessionKey, RecordTaskResult{
		RunID: runID, Label: sessionLabel, Mode: mode, Confined: confined, Status: recordStatusRecording, StartedAt: startedAt,
	}, ""); perr != nil {
		return types.AgentRun{}, false, release(fmt.Errorf("persist record state: %w", perr))
	}
	// A CreateGrant failure AFTER CreateRun (below) would otherwise orphan the
	// persisted RunPending run + leave its minted run token / eligible grants
	// un-revoked, so every failure path from here on returns through abort.
	abort := func(reason error) error {
		now := s.cfg.Now().UTC()
		_, _, _ = s.putRecordResult(ctx, ws.ID, sessionKey, RecordTaskResult{
			RunID: runID, Label: sessionLabel, Mode: mode, Confined: confined, Status: recordStatusFailed, StartedAt: now, FinishedAt: &now,
			FailureHint: "launch failed: " + reason.Error(),
		}, recordStatusRecording)
		return release(reason)
	}

	interactive := mode == recordModeInteractive
	run, runToken, err := s.newWorkspaceStepRun(ctx, runID, actor, "workspace record", ws, cc)
	if err != nil {
		return types.AgentRun{}, false, abort(err)
	}
	now := run.CreatedAt
	run.Interactive = interactive
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
		// A CONFINED REPLAY session is default-deny, limited to AllowedDomains
		// (baseline clone/registry hosts ∪ the workspace's approved egress) — so
		// re-running the same steps proves they work under least privilege. A
		// learning session (open) allows all egress so the capture is complete.
		// Same interactive attach either way.
		AllowAllEgress: !confined,
		AllowedDomains: confinedEgressDomains(ws),
		// In a confined replay, an off-policy host ESCALATES to the operator instead
		// of a silent hard-deny — so a "bad curl" surfaces an approve/reject decision
		// in the record panel as it happens. Inert under allow-all, so it's a no-op
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
	run.AutoStopAfterSec = policy.AutoStopAfterSec // reaper reads the run row
	cloneURLs := wireWorkspaceSource(&run, &policy, ws)
	created, err := s.cfg.Store.CreateRun(ctx, run)
	if err != nil {
		return types.AgentRun{}, false, abort(fmt.Errorf("create record run: %w", err))
	}
	// Clone grants only AFTER the run row exists — credential_grants.run_id has an
	// immediate FK to agent_runs(id). Only the FIRST repo source's clone gets an
	// auto-minted credential (see wireWorkspaceSource's doc comment) — matches
	// the pre-composition-model single-source behavior; an additional repo
	// source needs its own pre-existing access until multi-repo grant minting
	// is wired.
	var primaryCloneURL string
	if len(cloneURLs) > 0 {
		primaryCloneURL = cloneURLs[0]
	}
	ghGrantID, sshGrants, gerr := s.workspaceSourceGrants(ctx, runID, now, primaryCloneURL)
	if gerr != nil {
		return types.AgentRun{}, false, abort(fmt.Errorf("create record clone grants: %w", gerr))
	}

	// Record in the built devcontainer image so the task actually runs (its
	// toolchain isn't in the convention agent image) — same lane as verify.
	image := s.workspaceRunImage(ctx, runID, ws)

	// The model provider is part of the HARNESS the operator configured (getting
	// started), not per-workspace app egress they approve — so its host must be
	// reachable in EVERY agent session, confined replay included. A learning session
	// is AllowAllEgress so it's fine; a confined replay's AllowedDomains is
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
	// interactive run is never a scan run, so modelRun is true).
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
		ensureLLMGrant(&policy, "claude-code", s.presentSecretNames(ctx), false)
		for _, g := range policy.EligibleGrants {
			if g.Kind != types.GrantAPIKey || g.RequiresApproval {
				continue
			}
			grantID := uuid.New()
			if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
				ID: grantID, RunID: runID, CreatedAt: now, Spec: g,
			}); gerr != nil {
				return types.AgentRun{}, false, abort(fmt.Errorf("create llm grant: %w", gerr))
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
	// later confined replay reflects the SAME provider the operator configured (not a
	// guess). Guarded on `recording`: a superseding re-record must not resurrect this
	// entry.
	_, _, _ = s.putRecordResult(ctx, ws.ID, sessionKey, RecordTaskResult{
		RunID: runID, Label: sessionLabel, Mode: mode, Confined: confined,
		Status: recordStatusRecording, StartedAt: startedAt,
		LLMMode: llmMode, Model: s.cfg.AgentAnthropicModel,
	}, recordStatusRecording)

	// Sessions are interactive (the operator drives the activity in the attach
	// shell); no auto command plan. The `--idle` path clones the repo + attaches.
	return s.dispatchAndSettle(ctx, created, dispatchParams{
		RunToken:           runToken,
		Image:              image,
		Policy:             policy,
		FirstGitHubGrantID: ghGrantID,
		GitGrants:          gitBrokerGrant(primaryCloneURL, ghGrantID),
		SSHGrants:          sshGrants,
		Injections:         injections,
		Interactive:        interactive,
	}), weakCC, nil
}

// wireWorkspaceSource points run+policy at EVERY one of the workspace's
// sources: each repo source clones (added to policy.WorkspaceRepos; the FIRST
// also sets run.Repo, the run-row label); each local_dir source is
// bind-mounted at its own target (falling back to the composer workspace
// target when unset). An ephemeral source gets NO policy entry (a mkdir
// inside the sandbox, not a mount/clone — mirrors runs_create.go's
// WARDYN_EPHEMERAL_DIRS handling for the ordinary create-run path); this
// import-flow launch does not thread that env var, so an ephemeral source
// here is silently inert.
//
// It returns every repo source's clone URL, in Sources order, for
// workspaceSourceGrants — which today only auto-mints a clone credential for
// the FIRST one (see launchRecordRun's call site); an additional repo source
// needs its own pre-existing access until multi-repo grant minting is wired.
//
// ORDERING (load-bearing): this is a PURE run/policy mutation, so it must run
// BEFORE Store.CreateRun persists the row — whereas the clone grants it used to
// also create must run AFTER it, since credential_grants.run_id REFERENCES
// agent_runs(id) with an immediate FK. Doing both halves here forced one of the
// two orders to be wrong; the grant half is split out for that reason.
func wireWorkspaceSource(run *types.AgentRun, policy *types.RunPolicySpec, ws types.Workspace) (cloneURLs []string) {
	for _, src := range ws.Sources {
		switch src.Type {
		case types.WorkspaceSourceTypeRepo:
			if run.Repo == "" {
				run.Repo = src.Source
			}
			policy.WorkspaceRepos = append(policy.WorkspaceRepos, types.WorkspaceRepo{Repo: src.Source, Target: src.Target})
			if url := repoCloneURL(src.Source); url != "" {
				cloneURLs = append(cloneURLs, url)
			}
		case types.WorkspaceSourceTypeLocalDir:
			target := src.Target
			if target == "" {
				target = composerWorkspaceTarget
			}
			// ReadOnly is a *bool whose SAFE DEFAULT is read-only when omitted.
			// Omitting it mounted every imported source read-only, which made the
			// Record step's own promise ("so the agent can make changes")
			// impossible to keep: `pnpm install` cannot write node_modules, a build
			// cannot emit artifacts, and no source file can be edited. Honor the
			// operator's explicit per-source opt-in instead; the default is still
			// read-only, so this widens nothing unless a human ticked the box.
			ro := !src.Writable
			if run.WorkspacePath == "" {
				run.WorkspacePath = src.Path
			}
			policy.WorkspaceMounts = append(policy.WorkspaceMounts, types.WorkspaceMount{
				Source: src.Path, Target: target, ReadOnly: &ro,
			})
		case types.WorkspaceSourceTypeEphemeral:
			// no policy entry — see doc comment above.
		}
	}
	return cloneURLs
}

// workspaceSourceGrants creates the repo clone's read credentials. It MUST be
// called AFTER Store.CreateRun: credential_grants.run_id REFERENCES
// agent_runs(id) (non-deferrable), so a grant written before the run row is
// rejected by the FK. A local dir (cloneURL "") needs no grant.
func (s *Server) workspaceSourceGrants(ctx context.Context, runID uuid.UUID, now time.Time, cloneURL string) (*uuid.UUID, map[string]string, error) {
	if cloneURL == "" {
		return nil, nil, nil
	}
	ghGrantID, err := s.maybeGitHubReadGrant(ctx, runID, now, cloneURL)
	if err != nil {
		return nil, nil, err
	}
	sshGrants, err := s.maybeSSHKeyGrant(ctx, runID, now, cloneURL)
	if err != nil {
		return nil, nil, err
	}
	return ghGrantID, sshGrants, nil
}

// maybeGitHubReadGrant creates a read-only github_token grant for a github.com
// clone URL (nil for any other host) — extracted from launchScanRun's
// private-repo clone support so launchRecordRun can reuse it too. A CreateGrant
// failure is returned, never swallowed: the clone cannot authenticate without
// the grant, so the launch must fail loudly rather than dispatch a sandbox
// whose private-repo clone is guaranteed to 403.
//
// SCOPED TO THE CLONE'S OWN REPO — the SAME key gitBrokerGrant uses for the
// broker map, so the grant and the route it is reached through can never
// disagree. It used to write `"repos": []`, which the real minter refuses
// outright (githubMinter.MintInstallationToken: an installation token is
// per-installation and the owner comes from the first repo), so every
// scan/record clone of a GitHub HTTPS repo 502'd at handleGitBroker the
// moment a real GitHub App was configured. No test saw it because
// FakeGitHubMinter did not reproduce that precondition; it does now.
//
// A github.com URL with no derivable "<org>/<repo>" (a deeper path) yields NO
// grant: there is nothing a token could be scoped to, and an unmintable grant is
// worse than none — it also sets WARDYN_GITHUB_GRANT_ID, pointing the in-sandbox
// helper at a mint that can only fail.
func (s *Server) maybeGitHubReadGrant(ctx context.Context, runID uuid.UUID, now time.Time, cloneURL string) (*uuid.UUID, error) {
	repo := gitBrokerKey(cloneURL) // "" for non-github, non-HTTPS, or a non-repo path
	if repo == "" {
		return nil, nil
	}
	gid := uuid.New()
	scope, _ := json.Marshal(map[string]any{
		"repos": []string{repo}, "permissions": map[string]string{"contents": "read"},
	})
	if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
		ID: gid, RunID: runID, CreatedAt: now,
		Spec: types.GrantSpec{Kind: types.GrantGitHubToken, Scope: scope, TTLSeconds: 600},
	}); gerr != nil {
		return nil, fmt.Errorf("create github read grant: %w", gerr)
	}
	return &gid, nil
}

// maybeSSHKeyGrant synthesizes a run-scoped ssh_key grant for an SSH/scp clone
// URL — the SSH analog of maybeGitHubReadGrant — so onboarding an SSH workspace
// needs no manually-created grant. It returns the host→grant-id map dispatch
// marshals into WARDYN_SSH_GRANTS (nil when the source isn't an SSH URL to a
// supported provider, or the operator hasn't stored the ssh-key-<host> secret).
// The grant's host is the host git actually dials (so the sandbox ssh_config
// stanza matches), while key_secret_ref is the CANONICAL ssh-key-<host> secret
// (so either the github.com or ssh.github.com URL form resolves one stored key).
func (s *Server) maybeSSHKeyGrant(ctx context.Context, runID uuid.UUID, now time.Time, cloneURL string) (map[string]string, error) {
	host, ok := sshCloneHost(cloneURL)
	if !ok {
		return nil, nil
	}
	if _, ok := sshOver443Endpoint(host); !ok {
		return nil, nil
	}
	secretName, ok := canonicalSSHKeySecret(host)
	if !ok {
		return nil, nil
	}
	// Only synthesize a grant when the key is actually present — otherwise the
	// clone would fail; the onboarding guard (below) rejects that case up front.
	names, err := s.listUserSecretNames(ctx)
	if err != nil {
		return nil, nil
	}
	if !slices.Contains(names, secretName) {
		return nil, nil
	}
	gid := uuid.New()
	scope, _ := json.Marshal(map[string]any{"host": host, "key_secret_ref": secretName})
	if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
		ID: gid, RunID: runID, CreatedAt: now,
		Spec: types.GrantSpec{Kind: types.GrantSSHKey, Scope: scope, TTLSeconds: 600},
	}); gerr != nil {
		return nil, fmt.Errorf("create ssh key grant: %w", gerr)
	}
	return map[string]string{host: gid.String()}, nil
}

// reconcileWorkspaceRun is called when a governed scan run reaches a terminal
// state. If the workspace is STILL in the in-flight state pointing at this run
// — meaning the run's result upload never arrived (the scan binary uploads
// BEFORE the process exits, so by the time the completion watcher fires a
// successful upload has already advanced the workspace) — it reconciles the
// workspace out of the stuck state instead of leaving it hung. The common
// cause is a sandbox that cannot reach the control plane (e.g. Docker-Desktop/
// WSL2 NAT networking).
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
	case types.WorkspaceScanning:
		// A repo scan run ended without uploading facts — leave a clear error via a
		// SCOPED write: touch only status + clear the in-flight pointer. The
		// previous full-row UpdateWorkspace replayed a stale pre-read snapshot over
		// EVERY column, clobbering any concurrently-persisted async field (profile,
		// record_results, approvals). Fenced on THIS run still owning the import
		// step: a newer scan that already claimed the slot must not be reverted by
		// this late reconcile.
		_, _, _ = s.cfg.Store.SetWorkspaceImportState(ctx, ws.ID, types.WorkspaceError, nil, &runID)
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "workspace.scan",
			ws.ID.String(), "failure", mustJSON(map[string]any{"reason": "no_facts_uploaded"})))
	}
}

// settleTerminalLaunch settles a workspace/record run that reached a TERMINAL
// state DURING dispatch — a CreateSandbox error or a STARTING→FAILED grant/MITM
// path CAS's the run to FAILED before Exec runs, so the completion watcher (which
// starts only after a successful Exec) never fires and no reconcile hook would
// otherwise settle the workspace. Without this, a scan strands in `scanning`
// forever and a record capture is never taken.
// Both reconcilers are idempotent + self-scoping — reconcileRecordRun no-ops for a
// non-record run, reconcileWorkspaceRun no-ops unless the workspace still points at
// this run in `scanning` — so calling both is safe for any launch kind.
func (s *Server) settleTerminalLaunch(ctx context.Context, runID uuid.UUID, created types.AgentRun) {
	if !isTerminalRunState(created.State) {
		return
	}
	s.reconcileWorkspaceRun(ctx, runID)
	s.reconcileRecordRun(ctx, runID)
}

// recordEmptyCaptureHint explains a capture that produced ZERO egress evidence.
// This is a FAILURE, never "the task needs no egress": the (open) recording
// exists to observe, and the common cause of observing nothing is the proxy's
// decision callbacks not reaching the control plane, not a network-free task.
const recordEmptyCaptureHint = "no egress evidence was captured for this recording — most commonly the " +
	"proxy sidecar's decision callbacks cannot reach the control plane (Docker Desktop + WSL2 NAT needs " +
	"mirrored networking or the compose stack, `make setup`). Treat this recording as failed, NOT as " +
	"proof the task needs no egress."

// maxCaptureAuditEvents bounds a single server-side record/synthesis capture's
// audit-event read. It is a DoS ceiling far ABOVE any realistic recording — its
// only job is to replace QueryAuditEvents' SILENT 1000-row default, which
// truncated a long recording's later egress out of the derived least-privilege
// profile (a scanned-clean-but-actually-incomplete result). It never caps a
// legitimate run; a capture that somehow reaches it stamps captureAuditTruncatedNote
// so the truncation is VISIBLE, never silent.
const maxCaptureAuditEvents = 100000

// captureAuditTruncatedNote is the honest signal stamped on a capture / synthesis
// whose audit read reached maxCaptureAuditEvents — its observations/profile may be
// incomplete for an exceptionally long run.
const captureAuditTruncatedNote = "audit-event capture reached its ceiling; the derived observations/profile " +
	"may be incomplete for an exceptionally long run"

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

	events, err := s.cfg.Store.QueryAuditEvents(ctx, runID, maxCaptureAuditEvents)
	if err != nil {
		return // transient store failure: leave `recording`; a later reconcile retries
	}
	obs := recordmode.Capture(events)
	now := s.cfg.Now().UTC()
	res.FinishedAt = &now
	res.Observations = &obs
	res.KernelSensorBlind = run.ConfinementClass == types.CC3
	res.Caveats = []string{recordMaskingCaveat}
	if len(events) >= maxCaptureAuditEvents {
		res.Caveats = append(res.Caveats, captureAuditTruncatedNote)
	}
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

// outcomeBool renders a bool as the audit outcome string convention
// ("success"/"failure"). Its only remaining caller is reconcileRecordRun; it
// used to be shared with the (now-removed) verify-result upload handler.
func outcomeBool(ok bool) string {
	if ok {
		return "success"
	}
	return "failure"
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
	return slices.Sorted(maps.Keys(seen))
}

// launchScanRun starts a throwaway GOVERNED run that clones a repo workspace and
// runs wardyn-scan (scan-only mode) instead of an agent. wardyn-scan uploads
// ScanFacts to the brokered scan-results route; that endpoint derives + persists
// the profile onto this workspace via the TRUSTED run→workspace linkage
// (run.WorkspaceID) — never from sandbox input. Mirrors handleCreateRun's
// mint → CreateRun → dispatch flow, minus the request surface: no grants, no user
// mounts, a minimal git-egress policy, no model call. Returns the created run.

// scanIdleCapSec bounds an idle scan run: short, since wardyn-scan clones + scans
// and uploads promptly. Written to both the run row (for the reaper) and the
// dispatch scanPolicy so the two never drift.
const scanIdleCapSec = 600

func (s *Server) launchScanRun(ctx context.Context, actor string, ws types.Workspace) (types.AgentRun, error) {
	if s.cfg.Runner == nil {
		return types.AgentRun{}, fmt.Errorf("no runner configured")
	}
	repoSrc, ok := firstRepoSource(ws)
	if !ok {
		return types.AgentRun{}, fmt.Errorf("workspace %s has no repo source to scan", ws.ID)
	}
	url := repoCloneURL(repoSrc.Source)
	if url == "" {
		return types.AgentRun{}, fmt.Errorf("repo %q has no derivable clone URL", repoSrc.Source)
	}
	// Detach from request cancellation before the durable launch work: a client
	// that walks away must not cancel it (same rationale as launchRecordRun).
	ctx = context.WithoutCancel(ctx)

	runID := uuid.New()
	// The scan self-heal in reconcileWorkspaceRun keys on ws.ActiveRunID == runID,
	// so a scan that never claims the slot leaves that branch DEAD — a scan whose
	// facts upload is lost (e.g. sandbox can't reach the control plane) would then
	// strand the workspace in `scanning` forever.
	claimedWS, release, err := s.claimImportStep(ctx, ws, runID)
	if err != nil {
		return types.AgentRun{}, err
	}
	// Confinement: inherit the operator's default floor. A scan is read-only,
	// ephemeral, and holds no credentials; the operator's floor still governs.
	cc := s.defaultFloorClass()
	run, runToken, err := s.newWorkspaceStepRun(ctx, runID, actor, "workspace scan", ws, cc)
	if err != nil {
		return types.AgentRun{}, release(err)
	}
	now := run.CreatedAt
	run.Repo = repoSrc.Source             // wardyn-scan clones it; no model call in scan-only mode
	run.AutoStopAfterSec = scanIdleCapSec // reaper reads the run row; == scanPolicy below
	created, err := s.cfg.Store.CreateRun(ctx, run)
	if err != nil {
		return types.AgentRun{}, release(fmt.Errorf("create scan run: %w", err))
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
	// SSH clone URL: synthesize the ssh_key grant + surface it as WARDYN_SSH_GRANTS.
	ghGrantID, sshGrants, gerr := s.workspaceSourceGrants(ctx, runID, now, url)
	if gerr != nil {
		return types.AgentRun{}, release(fmt.Errorf("create scan clone grants: %w", gerr))
	}

	// Minimal scan policy: allow only the git host(s) the clone needs + a short
	// auto-stop. No workspace mounts, no subscription — wardyn-scan uploads to the
	// proxy's brokered route (not egress).
	scanPolicy := types.RunPolicySpec{
		MinConfinementClass: cc,
		AllowedDomains:      scanEgressDomains(url),
		AutoStopAfterSec:    scanIdleCapSec,
	}

	return s.dispatchAndSettle(ctx, created, dispatchParams{
		RunToken:           runToken,
		Image:              agentImage("claude-code", s.cfg.AgentImages),
		Policy:             scanPolicy,
		FirstGitHubGrantID: ghGrantID,
		GitGrants:          gitBrokerGrant(url, ghGrantID),
		SSHGrants:          sshGrants,
	}), nil
}

// gitBrokerManagedHosts are the GitHub clone/API/content hosts that Option C's
// git-broker manages: they are routed through wardyn-proxy (never a run's egress
// allowlist) and must never be promoted into a permanent ApprovedEgress entry (that
// would re-open host-level github egress and defeat the broker's repo-scoping).
var gitBrokerManagedHosts = []string{"github.com", "api.github.com", "codeload.github.com", "*.githubusercontent.com"}

// gitBrokerKey returns the canonical lowercased "<org>/<repo>" git-broker allowlist
// key for a GitHub HTTPS clone URL, or "" when cloneURL isn't a bare github repo
// path (SSH, non-github, or a deeper path). Used to build the per-run GitGrants
// map for the scan/verify/record clone.
func gitBrokerKey(cloneURL string) string {
	u, err := neturl.Parse(cloneURL)
	if err != nil || u.Hostname() != "github.com" || !isHTTPScheme(u.Scheme) {
		return ""
	}
	p := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if parts := strings.Split(p, "/"); len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return strings.ToLower(p)
	}
	return ""
}

// gitBrokerKeyFromSlug returns the canonical "<org>/<repo>" git-broker key for a
// run's declared repo slug — a bare "<org>/<repo>" (the github_token clone form) or
// a full https github URL. SSH/scp forms return "" (they use the ssh-over-443 lane,
// not the HTTPS-only broker).
func gitBrokerKeyFromSlug(slug string) string {
	slug = strings.TrimSpace(slug)
	if strings.Contains(slug, "@") || strings.HasPrefix(strings.ToLower(slug), "ssh://") {
		return ""
	}
	if strings.Contains(slug, "://") {
		return gitBrokerKey(slug) // full URL: github https -> key, else ""
	}
	return gitBrokerKey(repoCloneURL(slug)) // bare slug -> github https -> key
}

// gitBrokerGrant builds the single-repo git-broker allowlist for a scan/verify/
// record clone: {"<org>/<repo>": *ghGrantID}, or nil when there's no github grant
// or the clone isn't a broker-covered github repo (ssh/non-github/local dir).
func gitBrokerGrant(cloneURL string, ghGrantID *uuid.UUID) map[string]uuid.UUID {
	if ghGrantID == nil {
		return nil
	}
	if key := gitBrokerKey(cloneURL); key != "" {
		return map[string]uuid.UUID{key: *ghGrantID}
	}
	return nil
}

// isGitHubCloneHost reports whether host is a GitHub clone/API host (github.com,
// ssh.github.com, or a *.github.com subdomain), case-insensitively.
func isGitHubCloneHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	return h == "github.com" || h == "ssh.github.com" || strings.HasSuffix(h, ".github.com")
}
