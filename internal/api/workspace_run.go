// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
// logSink, when non-nil, receives the build's output as it happens (the
// wizard Build step's in-memory ring); every OTHER caller passes nil, which
// falls back to the ImageBuilder's own default (wardynd's slog) unchanged.
//
// It audits its own build success/failure against runID.
func (s *Server) resolveWorkspaceImage(ctx context.Context, runID uuid.UUID, primary types.Workspace, logSink io.Writer) (string, bool) {
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
		if s.cfg.ImageBuilder == nil {
			// PARITY-4: the workspace_id door hard-400s a base_image with no builder
			// wired (validateImageBuildRequest, re-run after the seed); the UI door has
			// no such gate, so at minimum AUDIT that the operator's chosen base image
			// was DROPPED for the convention image rather than swapping it silently.
			// (Fully closing the door = the UI sending workspace_id — later UI batch.)
			buildAudit("skipped", map[string]any{
				"source": "base_image:" + b.Kind, "base": b.Image,
				"reason": "no image builder wired; base image dropped for the convention image",
			})
			return "", false
		}
		// Wrap the operator's base image the SAME way the workspace_id door does
		// (PARITY-4): FinalizeBase copies the runner tools + agent-run in and tags it
		// wardyn-byoi/<runid>, which ALSO arms dispatch's fail-closed harness selftest
		// (runs_dispatch.go keys it off the wardyn-byoi/ prefix). Before this a UI-door
		// run launched the raw image with no agent-run and failed with an opaque exec
		// error, while the CLI wrapped + selftest-gated the identical workspace.
		// "custom" Steps are not layered here (no builder method layers Dockerfile
		// lines on a base yet) — same as the req.Image path's own verbatim wrap.
		outTag := "wardyn-byoi/" + runID.String() + ":latest"
		built, berr := s.cfg.ImageBuilder.FinalizeBase(ctx, b.Image, outTag, logSink)
		if berr != nil {
			buildAudit("failure", map[string]any{"source": "base_image:" + b.Kind, "base": b.Image, "error": berr.Error()})
			return "", false
		}
		buildAudit("success", map[string]any{"source": "base_image:" + b.Kind, "base": b.Image, "image": built})
		return built, true
	}

	if s.cfg.ImageBuilder == nil {
		return "", false
	}

	p, ok := workspaceProfile(primary)
	if !ok {
		return "", false // unscanned/malformed → convention image
	}

	// tools: the agent CLIs this workspace's NAMED integrations imply (gen.go's
	// AgentToolsForIntegrationTypes). Computed up front (not just below the
	// repo-devcontainer branch) so that branch can honestly RECORD what it is
	// NOT baking in its own audit entry, even though it never reaches the
	// generator.
	tools := workspacescan.AgentToolsForIntegrationTypes(s.namedIntegrationTypes(ctx, primary))

	// A repo PRIMARY source (Sources[0]) carrying its OWN devcontainer: respect
	// it, build from the repo — UNLESS the source is an SSH URL. The image
	// builder (envbuilder) clones with no minted key / known_hosts / :443
	// ProxyCommand — only the agent-run sandbox has that wiring — so an SSH
	// devcontainer build would fail auth. Fall through to a generated toolchain
	// image; agent-run still clones the repo itself using the run's ssh_key
	// grant (the repo's own devcontainer is just not built in v1).
	//
	// This lane never bakes `tools`: it builds the repo's own devcontainer
	// file(s) verbatim rather than rewriting them to point at the generated
	// Dockerfile gen.go's baseOrBuild would otherwise layer a RUN onto (see
	// its package comment — the two mechanisms that WOULD add one without
	// rewriting the operator's own devcontainer, a lifecycle hook and the
	// vendor's own devcontainer feature, were both tried against a real build
	// and rejected). A named integration's agent CLI is therefore silently
	// absent from this image unless the operator's own devcontainer happens to
	// install it — deliberate (see resolveBuildView's repoDevcontainerToolCaveat,
	// which surfaces that honestly instead of leaving it silent), not an
	// oversight. repoOwnDevcontainerURL is shared with that caveat so "does
	// this lane bake the tool" can never drift from the branch that decides it.
	if url := repoOwnDevcontainerURL(primary, p); url != "" {
		repoSrc := primary.Sources[0]
		tag := "wardyn-workspace/" + primary.ID.String() + ":devcontainer"
		if built, err := s.cfg.ImageBuilder.BuildDevcontainer(ctx, url, repoSrc.Ref, tag, logSink); err == nil {
			buildAudit("success", map[string]any{"source": "repo-devcontainer", "image": built, "tools_named_not_baked": tools})
			return built, true
		} else {
			buildAudit("failure", map[string]any{"source": "repo-devcontainer", "error": err.Error(), "tools_named_not_baked": tools})
			return "", false
		}
	}
	if len(primary.Sources) > 0 && primary.Sources[0].Type == types.WorkspaceSourceTypeRepo && p.HasDevcontainer {
		if _, ssh := sshCloneHost(primary.Sources[0].Source); ssh {
			buildAudit("skipped", map[string]any{"source": "repo-devcontainer", "reason": "ssh-source-not-buildable-by-image-builder"})
		}
	}

	hash := p.CacheKey(tools)
	// Reuse a cached generated image when the profile+tools are unchanged.
	if primary.ImageRef != "" && primary.BuiltProfileHash == hash {
		return primary.ImageRef, true
	}

	// Generate a devcontainer for the detected toolchain (+ any named agent
	// tool) and build it.
	files, gerr := workspacescan.GenerateDevcontainer(p, tools)
	if gerr != nil {
		buildAudit("failure", map[string]any{"source": "generated-devcontainer", "error": gerr.Error()})
		return "", false
	}
	tag := "wardyn-workspace/" + primary.ID.String() + ":" + hash[:12]
	built, berr := s.cfg.ImageBuilder.BuildFromDevcontainerFiles(ctx, files, tag, logSink)
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

// repoOwnDevcontainerURL returns the clone URL resolveWorkspaceImage builds
// FROM when ws's primary source is a repo carrying its own devcontainer
// (HasDevcontainer) — or "" when that lane does not apply: no repo primary,
// no devcontainer, an SSH source (the image builder cannot clone one — see
// resolveWorkspaceImage's comment), or an unparseable source. Extracted so
// resolveBuildView's repoDevcontainerToolCaveat can ask "is THIS the lane
// that will not bake the named tool" without a second, independently-drifting
// copy of the same four conditions.
func repoOwnDevcontainerURL(ws types.Workspace, p workspacescan.WorkspaceProfile) string {
	if len(ws.Sources) == 0 || ws.Sources[0].Type != types.WorkspaceSourceTypeRepo || !p.HasDevcontainer {
		return ""
	}
	repoSrc := ws.Sources[0]
	if _, ssh := sshCloneHost(repoSrc.Source); ssh {
		return ""
	}
	return repoCloneURL(repoSrc.Source)
}

// repoDevcontainerToolCaveat reports the honest caveat for a workspace whose
// primary source is a repo carrying its own devcontainer: resolveWorkspaceImage
// builds that devcontainer AS-IS (repoOwnDevcontainerURL), so a named
// integration's agent CLI is not baked into the resulting image. Returns ""
// when the caveat does not apply — no repo-own-devcontainer lane, or nothing
// named that would have been baked anyway. Used by resolveBuildView so the
// wizard's Build step can say so instead of leaving the gap silent.
func (s *Server) repoDevcontainerToolCaveat(ctx context.Context, ws types.Workspace) string {
	prof, ok := workspaceProfile(ws)
	if !ok || repoOwnDevcontainerURL(ws, prof) == "" {
		return ""
	}
	tools := workspacescan.AgentToolsForIntegrationTypes(s.namedIntegrationTypes(ctx, ws))
	if len(tools) == 0 {
		return ""
	}
	return "this repo carries its own devcontainer, built as-is — Wardyn does not add " +
		strings.Join(tools, ", ") + " to it"
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

// newStepRun mints the run identity and builds the run row every
// server-launched step/probe run (scan/verify/record/probe) shares: PENDING,
// claude-code, State/SPIFFEID/RunnerTarget set. set customizes what differs
// (the trusted linkage, Task specifics, AutoStopAfterSec) before the row is
// returned; callers take run.CreatedAt as the launch clock.
func (s *Server) newStepRun(ctx context.Context, runID uuid.UUID, actor, task string, cc types.ConfinementClass, set func(*types.AgentRun)) (types.AgentRun, string, error) {
	id, err := s.cfg.Identity.MintRunIdentity(ctx, runID, actor, actor, internalAudience)
	if err != nil {
		return types.AgentRun{}, "", fmt.Errorf("mint run identity: %w", err)
	}
	now := s.cfg.Now().UTC()
	run := types.AgentRun{
		ID: runID, CreatedAt: now, UpdatedAt: now, CreatedBy: actor,
		Agent: "claude-code", Task: task,
		ConfinementClass: cc, State: types.RunPending, SPIFFEID: id.SPIFFEID,
		RunnerTarget: s.cfg.RunnerTarget,
	}
	if set != nil {
		set(&run)
	}
	return run, id.Token, nil
}

// newWorkspaceStepRun is newStepRun linked to ws through WorkspaceID — the
// TRUSTED linkage each step's upload authorises on, never sandbox input.
func (s *Server) newWorkspaceStepRun(ctx context.Context, runID uuid.UUID, actor, task string, ws types.Workspace, cc types.ConfinementClass) (types.AgentRun, string, error) {
	wsID := ws.ID
	return s.newStepRun(ctx, runID, actor, task, cc, func(run *types.AgentRun) {
		run.WorkspaceID = &wsID
	})
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
	if built, ok := s.resolveWorkspaceImage(ctx, runID, ws, nil); ok {
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
func (s *Server) launchRecordRun(ctx context.Context, actor string, ws types.Workspace, sessionKey, sessionLabel string, confined bool) (types.AgentRun, bool, error) {
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
		RunID: runID, Label: sessionLabel, Mode: recordModeInteractive, Confined: confined, Status: recordStatusRecording, StartedAt: startedAt,
	}, ""); perr != nil {
		return types.AgentRun{}, false, release(fmt.Errorf("persist record state: %w", perr))
	}
	// A CreateGrant failure AFTER CreateRun (below) would otherwise orphan the
	// persisted RunPending run + leave its minted run token / eligible grants
	// un-revoked, so every failure path from here on returns through abort.
	abort := func(reason error) error {
		now := s.cfg.Now().UTC()
		_, _, _ = s.putRecordResult(ctx, ws.ID, sessionKey, RecordTaskResult{
			RunID: runID, Label: sessionLabel, Mode: recordModeInteractive, Confined: confined, Status: recordStatusFailed, StartedAt: now, FinishedAt: &now,
			FailureHint: "launch failed: " + reason.Error(),
		}, recordStatusRecording)
		return release(reason)
	}

	run, runToken, err := s.newWorkspaceStepRun(ctx, runID, actor, "workspace record", ws, cc)
	if err != nil {
		return types.AgentRun{}, false, abort(err)
	}
	now := run.CreatedAt
	run.Interactive = true
	// A confined verify HOLDS an off-policy host at the door (wait_for_review:
	// the connection parks while the approval surfaces in the verify panel's
	// live strip; approve releases it, deny/timeout fails it). The old
	// deny_with_review here leaned on a stale "unattended probe must fail
	// fast" rationale — every session is interactive now (the operator drives
	// the attach terminal), so the operator IS present to decide, and a held
	// request that gets approved both completes in-flight AND lands as an
	// egress: requirement row via the decide() hook. A learning session
	// (allow-all) makes this inert.
	verifyFirstUse := types.FirstUseAlwaysDeny
	if confined {
		verifyFirstUse = types.FirstUseWaitForReview
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
		// Generous but FINITE idle cap — an abandoned open-egress recording
		// self-terminates (and revokes) instead of living forever.
		AutoStopAfterSec: int(recordInteractiveIdleCap.Seconds()),
	}
	run.AutoStopAfterSec = policy.AutoStopAfterSec // reaper reads the run row
	cloneURLs, ephemeralDirs := wireWorkspaceSource(&run, &policy, ws)
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

	// Model access for the session comes from the WORKSPACE's OWN binding (SPINE-7)
	// — the same resolveRunIntegration precedence (explicit → workspace pin →
	// operator default) a real run of this workspace uses — not just the operator
	// ceiling's convention secret. A confined replay whose job is to PROVE least
	// privilege must authenticate on the SAME credential path a real run will, or
	// its capture (and the promotion candidates derived from it) reflect a different
	// transport. A synthetic claude-code request with this one workspace as wsRefs
	// drives the identical fold launch/preflight run; bedrockRef is threaded into
	// dispatch below so a bedrock-bound workspace records its OWN region, not the
	// global default.
	var injections []runner.InjectionGrant
	var integKind string
	var bedrockRef *types.WorkspaceBedrockRef
	// Only when the workspace carries its OWN binding: the record session honors the
	// workspace's pinned integration, but does NOT reach for the operator's
	// site-wide default here (that tier stays a real run's concern) — which also
	// keeps foldRunIntegration off defaultAgentRunsIntegration's site-config read on
	// the record path.
	if ws.LLMCred != nil && ws.LLMCred.IntegrationRef != "" {
		_, integKind, bedrockRef = s.foldRunIntegration(ctx, &policy, createRunRequest{Agent: "claude-code"}, []types.Workspace{ws})
	}
	subMounted := specHasMountTarget(&policy, claudeCredTarget)
	if integKind == "" && !subMounted {
		// No workspace/operator integration bound: fall back to the operator
		// ceiling's convention subscription mount, else a brokered api-key grant
		// (today's behavior for an unbound workspace).
		if m, _ := applyLLMCredMount(&policy, s.cfg.DefaultPolicy, "claude-code", true); m {
			subMounted = true
		} else {
			ensureLLMGrant(&policy, "claude-code", s.presentSecretNames(ctx), false)
		}
	}
	llmMode := "none"
	switch {
	case subMounted, integKind == "anthropic_subscription":
		llmMode = "subscription" // managed subscription is injected proxy-side by dispatch
	case integKind == "bedrock" || bedrockRef != nil:
		llmMode = "bedrock" // dispatch's resolveBedrockAuth wires it from bedrockRef below
	}
	if !subMounted {
		// Build the injection from whatever api_key grant the fold or the fallback
		// added — mirrors handleCreateRun's api_key branch (a subscription/bedrock
		// fold adds none: managed is injected proxy-side, Bedrock via resolveBedrockAuth).
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
		if len(injections) > 0 && llmMode == "none" {
			llmMode = "api-key"
		}
	}

	// Save the resolved auth mode + model onto the session entry so it's visible and a
	// later confined replay reflects the SAME provider the operator configured (not a
	// guess). Guarded on `recording`: a superseding re-record must not resurrect this
	// entry.
	_, _, _ = s.putRecordResult(ctx, ws.ID, sessionKey, RecordTaskResult{
		RunID: runID, Label: sessionLabel, Mode: recordModeInteractive, Confined: confined,
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
		// The workspace's own Bedrock region/model (SPINE-7) — nil for a non-bedrock
		// binding, so dispatch keeps the global config exactly as before.
		BedrockRef:  bedrockRef,
		Interactive: true,
		// A record/verify session runs ONE workspace — its scans decide the
		// toolchain env, same rule as an ordinary workspace run.
		Toolchains: runToolchainNeeds([]types.Workspace{ws}),
		// Any ephemeral source's scratch target — wireWorkspaceSource's doc
		// comment — surfaced the same way the ordinary create-run path does.
		EphemeralDirs: ephemeralDirs,
	}), weakCC, nil
}

// wireWorkspaceSource points run+policy at EVERY one of the workspace's
// sources: each repo source clones (added to policy.WorkspaceRepos; the FIRST
// also sets run.Repo, the run-row label); each local_dir source is
// bind-mounted at its own target (falling back to the composer workspace
// target when unset). An ephemeral source gets NO policy entry (a mkdir
// inside the sandbox, not a mount/clone) — its target is collected into
// ephemeralDirs instead, for the caller to surface as WARDYN_EPHEMERAL_DIRS at
// dispatch, mirroring seedRequestWorkspace's identical handling for the
// ordinary create-run path (runs_create.go).
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
func wireWorkspaceSource(run *types.AgentRun, policy *types.RunPolicySpec, ws types.Workspace) (cloneURLs, ephemeralDirs []string) {
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
			// No policy entry — it's a mkdir inside the sandbox, not a mount/clone —
			// which also means it never passes through ValidateMount at CreateSandbox
			// time the way a repo/local_dir target does. Re-validate it here against
			// the same deny-list runner.ValidateTarget applies at onboarding, mirroring
			// seedRequestWorkspace's identical guard (runs_create.go): a row written
			// before that check existed must not ride straight past the gate onto a
			// path outside allowedTargetPrefixes.
			if src.Target != "" && runner.ValidateTarget(src.Target) == nil {
				ephemeralDirs = append(ephemeralDirs, src.Target)
			}
		}
	}
	return cloneURLs, ephemeralDirs
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
// clone URL (nil for any other host) — extracted from the scan launch's
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
	if err != nil {
		return
	}
	// SOURCE scan runs settle on the source row: same self-heal one tier down,
	// fenced on the source's own active_run_id so a newer claim is untouched.
	if run.SourceID != nil {
		src, serr := s.cfg.Store.GetSource(ctx, *run.SourceID)
		if serr != nil || src.ActiveRunID == nil || *src.ActiveRunID != runID {
			return
		}
		if src.Status == types.WorkspaceScanning {
			_, _ = s.cfg.Store.SetSourceScanResult(ctx, src.ID, src.Profile, types.WorkspaceError, runID, nil)
			s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "source.scan",
				src.ID.String(), "failure", mustJSON(map[string]any{"reason": "no_facts_uploaded"})))
		}
		return
	}
	if run.WorkspaceID == nil {
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

// scanIdleCapSec bounds an idle scan run: short, since wardyn-scan clones + scans
// and uploads promptly. Written to both the run row (for the reaper) and the
// dispatch scanPolicy so the two never drift.
const scanIdleCapSec = 600

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
