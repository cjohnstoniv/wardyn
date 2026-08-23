/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// New Run permission wizard — spec composition.
//
// Split out of wizard-types.ts (which stays the source of truth for
// WizardState + the wizard's own per-step validation): this module holds
// buildSpec, the CANONICAL state -> wire-contract composer described in
// wizard-types.ts's own header, plus impliedEgressHosts, the grant-implied
// egress-host list buildSpec and step-egress.tsx both read (D6/claim3) so the
// two can never drift. Pure extraction — re-exported from wizard-types.ts so
// every existing importer keeps working unchanged.
import type {
  Agent,
  GrantSpec,
  RunPolicySpec,
  Workspace,
  WorkspaceMount,
  WorkspaceRepo,
} from "../../../lib/types";
import {
  basename,
  dedupe,
  gitPatConfigured,
  parseRepoList,
  resolvableSources,
  resolveWorkspace,
  resolveWorkspaceMounts,
  toRunWorkspacesWire,
} from "./wizard-types";
import type {
  CreateRunInputWithComposition,
  GitHubPermission,
  WizardAgent,
  WizardState,
} from "./wizard-types";

// Why buildSpec unions a host into allowed_domains without the operator ever
// toggling it in the Network card (D6/claim3).
export type ImpliedEgressWhy = "GitHub access" | "model key" | "Git PAT" | "repo workspace";

export interface ImpliedEgressHost {
  host: string;
  why: ImpliedEgressWhy;
}

// The ONE list of grant-implied egress hosts — buildSpec unions these into
// allowed_domains (below) so a granted capability is never silently gated
// behind first-use approval; step-egress.tsx renders the SAME list as
// non-removable "Added by grants:" chips so the one screen that owns egress
// can't disagree with what actually ships. Extracted here so the two call
// sites share one predicate/host-list and can never drift (D6/claim3).
export function impliedEgressHosts(
  state: WizardState,
  workspaces: Workspace[] = [],
): ImpliedEgressHost[] {
  const out: ImpliedEgressHost[] = [];
  if (state.llmSecretName) {
    out.push({ host: llmHostForSecret(state.agent, state.llmSecretName), why: "model key" });
  }
  // Any repo-kind selection implies the GitHub clone hosts even with the
  // GitHub grant untouched — claim 3's sharpest sub-case. When the grant IS
  // on, name that as the reason instead; same two hosts either way.
  // resolvableSources (not the flattened w.kind) so a multi-source workspace
  // whose repo isn't sources[0] is still recognized (PARITY-2).
  const hasRepoSelection = state.workspaces.some((sel) => {
    const w = resolveWorkspace(sel, workspaces);
    return !!w && resolvableSources(w).some((s) => s.type === "repo");
  });
  if (state.githubEnabled) {
    out.push(
      { host: "github.com", why: "GitHub access" },
      { host: "*.githubusercontent.com", why: "GitHub access" },
    );
  } else if (hasRepoSelection) {
    out.push(
      { host: "github.com", why: "repo workspace" },
      { host: "*.githubusercontent.com", why: "repo workspace" },
    );
  }
  // Gated on the SAME predicate as the grant emission (D5/claim4): a host
  // typed with no secret selected must never claim to be "added by grants".
  if (gitPatConfigured(state)) {
    out.push({ host: state.gitPatHost.trim(), why: "Git PAT" });
  }
  return out;
}

// buildSpec is the contract chokepoint: state in, canonical wire shapes out.
// `workspaces` is the onboarded-workspace list each selection resolves against
// (the wizard fetches it via listWorkspaces(); tests that don't touch
// state.workspaces can omit it). Resolution happens here rather than being
// embedded in WizardState so a selection is just "which workspace id, plus this
// run's target/read-only override" — never a stale copy of the workspace record.
export function buildSpec(
  state: WizardState,
  workspaces: Workspace[] = [],
): {
  run: CreateRunInputWithComposition;
  inline_policy: RunPolicySpec;
} {
  // A SHELL COMMAND is unattended by definition, so it forces batch here rather
  // than trusting state.mode. This is not cosmetic: task_mode=exec (below) is a
  // BATCH-only concept — no agent, no model, no attach — so a stale
  // state.mode==="interactive" surviving a runType toggle would launch the
  // command into an ordinary interactive shell session instead of the governed
  // exec lane the operator actually picked. Deriving it at the composer means
  // no stale state.mode can resurrect that.
  const interactive = state.runType === "agent" && state.mode === "interactive";

  // --- run scalars ---
  const run: CreateRunInputWithComposition = {
    agent: state.agent as Agent,
    repo: "",
    // An interactive run's task IS its optional boot seed now (Part A1):
    // interpreted per interactive_start below (an initial prompt for "agent", a
    // startup command for "shell") and fired once, at sandbox boot, in the same
    // persistent session the human's attach later joins. Empty is today's
    // pure-idle behavior, unchanged — so a mode toggle that leaves stray batch
    // text behind still needs the operator to have typed it INTO an interactive
    // seed field to ship it, never a leftover from before the toggle.
    task: state.task.trim(),
    confinement_class: state.confinementClass,
    interactive,
  };
  // The run's name (its grouping key on the board) and optional note.
  if (state.title.trim()) run.title = state.title.trim();
  if (state.description.trim()) run.description = state.description.trim();
  // Interactive only, and only the non-default: "shell" IS the long-standing
  // behavior, so it never needs to go on the wire.
  if (interactive && state.interactiveStart === "agent") run.interactive_start = "agent";
  // The boot-seed opt-in: only meaningful (and only ever true) for an
  // agent-started seed that actually has text — an empty seed has nothing to
  // auto-approve tool use FOR, and a shell startup command has no tool-approval
  // prompt to begin with. Default false (supervised) is the wire default too,
  // so there is nothing to omit-vs-send here; only `true` ever goes on the wire.
  if (interactive && state.interactiveStart === "agent" && state.task.trim() && state.seedAutoTools) {
    run.seed_auto_tools = true;
  }
  // Tool-approval posture: AUTONOMOUS claude-code only (codex-cli has no
  // external tool-approval contract — the form disables the choice, but
  // buildSpec re-asserts it structurally rather than trusting the UI alone).
  // "auto" is the wire default, so it's never sent — only the non-default
  // "hold" goes on the wire, same shape as interactive_start above.
  if (!interactive && state.runType === "agent" && state.agent === "claude-code" && state.toolApprovals === "hold") {
    run.tool_approvals = "hold";
  }
  // BYOI: a user-supplied base image the backend wraps with the runner tools.
  if (state.image.trim()) {
    run.image = state.image.trim();
  } else if (state.devcontainerRepo.trim()) {
    // W15-W15e-wizard-roundtrip-6: a composed devcontainer build — mutually
    // exclusive with `image` (the `else` above), same as the server enforces.
    run.devcontainer_repo = state.devcontainerRepo.trim();
  }
  // Governed command: task_mode=exec runs `task` as a plain shell command, no
  // agent/model involved. Omitted for "agent" so the wire default ("harness")
  // stays backward-compatible.
  if (state.runType === "command") {
    run.task_mode = "exec";
  }
  // Run override: pins model/harness access to one specific integration,
  // overriding the workspace pin and server default (see step-access.tsx).
  if (state.integrationId) {
    run.integration_id = state.integrationId;
  }

  // --- onboarded workspace selections -> workspace_mounts[] / workspace_repos[]
  // ---
  // The FIRST selection is the PRIMARY: its kind/source drives the run's `repo`
  // label (and, per the synthesis doc, the sandbox's base image) — additional
  // selections are just attached alongside it.
  const workspaceMounts: WorkspaceMount[] = [];
  const workspaceRepos: WorkspaceRepo[] = [];
  state.workspaces.forEach((sel, i) => {
    const w = resolveWorkspace(sel, workspaces);
    if (!w) return; // stale selection — defensively skip rather than dangle
    // One entry per SOURCE this workspace carries (PARITY-2) — a multi-source
    // or migrated-ephemeral workspace has no single mount/repo to flatten to;
    // the old w.kind/w.source read was EMPTY for exactly those cases, so it
    // silently attached nothing at all.
    const { mounts, repos } = resolveWorkspaceMounts(w, sel);
    workspaceMounts.push(...mounts);
    workspaceRepos.push(...repos);
    if (i === 0) {
      // Synthetic repo label so the run row reads meaningfully — the first
      // resolved repo/mount from the PRIMARY selection, never a bare "" for a
      // workspace w.source can't represent on its own.
      if (repos[0]) run.repo = repos[0].repo;
      else if (mounts[0]) run.repo = `local:${basename(mounts[0].source)}`;
    }
  });

  // --- per-workspace requirements-contract options (CreateRunRequest.Workspaces)
  // --- additive to the mounts/repos above: a selection here does nothing unless
  // its workspace is already attached via one of those (see WorkspaceSelection's
  // doc comment on the Go side).
  const runWorkspaces = toRunWorkspacesWire(state.workspaces);
  if (runWorkspaces.length) run.workspaces = runWorkspaces;

  // Residual PARITY-2: a pure-ephemeral (migrated-0029 container) primary
  // contributes no mount/repo, so the per-source resolution above emits nothing
  // the server can match a workspace to — referencedWorkspaces reads only the
  // spec's mounts/repos, so wsRefs is empty and the workspace's custom
  // base_image (and scratch target) is silently dropped, launching on the
  // DEFAULT image. Convey the workspace's IDENTITY via workspace_id so the
  // server's seedRequestWorkspace resolves its base_image. Gated on "buildSpec
  // produced no mount/repo" so it can never double-mount a local_dir/repo the
  // resolution already emitted; the first resolvable selection is the primary.
  if (!workspaceMounts.length && !workspaceRepos.length) {
    const primary = state.workspaces.find((sel) => resolveWorkspace(sel, workspaces));
    if (primary) run.workspace_id = primary.workspaceId;
  }

  // --- eligible grants ---
  const grants: GrantSpec[] = [];

  if (state.githubEnabled) {
    grants.push({
      kind: "github_token",
      scope: {
        repos: parseRepoList(state.githubRepos),
        permissions: githubPermissionsMap(state.githubPermission),
      },
      ttl_seconds: Math.max(0, Math.round(state.githubTtlMinutes * 60)),
      requires_approval: state.githubRequiresApproval,
    });
  }

  // git_pat grant: broker a stored PAT to git for a non-GitHub host. The PAT
  // VALUE reaches git via the credential helper (opposite of api_key). Emit only
  // when enabled with both a host and a secret selected.
  if (gitPatConfigured(state)) {
    const scope: Record<string, string> = {
      host: state.gitPatHost.trim(),
      secret_name: state.gitPatSecretName.trim(),
    };
    const user = state.gitPatUsername.trim();
    if (user) scope.username = user;
    grants.push({
      kind: "git_pat",
      scope,
      requires_approval: state.gitPatRequiresApproval,
    });
  }

  // The LLM api_key grant — no manual picker in step-access.tsx (model access
  // resolves from integrations instead).
  if (state.llmSecretName) {
    const host = llmHostForSecret(state.agent, state.llmSecretName);
    const { header, format } = apiKeyInjectionFor(host);
    grants.push({
      kind: "api_key",
      scope: {
        host,
        header,
        secret_name: state.llmSecretName,
        format,
      },
      requires_approval: false,
    });
  }

  // W15-W15e-wizard-roundtrip-3: re-emit any grant kind this wizard has no
  // editable UI for (ssh_key, cloud_sts) verbatim, unchanged, rather than
  // silently dropping it — see WizardState.opaqueGrants.
  grants.push(...state.opaqueGrants);

  // --- lifecycle: an interactive run comes up idle, so never-reap (-1) unless
  // the operator explicitly chose an auto-stop window. ---
  let autoStopAfterSec: number | undefined;
  if (state.lifecycle === "never") {
    autoStopAfterSec = -1;
  } else {
    autoStopAfterSec = Math.max(1, Math.round(state.autoStopMinutes * 60));
  }

  // Ensure the egress allowlist covers the hosts the run's OWN grants need, so a
  // selected LLM key or a GitHub clone isn't silently gated behind a first-use
  // approval. impliedEgressHosts is the ONE list (D6/claim3) — step-egress.tsx
  // renders the SAME hosts as "Added by grants:" chips, so the two can never
  // disagree about what buildSpec actually unions in here.
  const requiredHosts = impliedEgressHosts(state, workspaces).map((h) => h.host);

  // Allow-all egress: deny-list only. allowed_domains may be empty and first-use
  // approval is inert, so we drop the run's own required hosts (everything
  // non-denied is already reachable) and force first_use_approval off.
  const allowAll = state.allowAllEgress;

  // HIGH fix (wizard contract): proxy credential injection fails CLOSED unless
  // the api_key grant's EXACT injection host is present in allowed_domains — the
  // injector only rewrites requests whose host is on the allowlist. Under
  // allow-all the rest of the allowlist is correctly dropped, but the api_key
  // host MUST always be pinned, or a selected LLM key never gets injected and
  // the agent can't authenticate at startup. (github_token clones don't need a
  // pinned host under allow-all — they're reached via plain egress, not a proxy
  // injection rule — so we only force the api_key host through here.)
  const grantInjectionHosts: string[] = [];
  if (state.llmSecretName) {
    grantInjectionHosts.push(llmHostForSecret(state.agent, state.llmSecretName));
  }

  const allowedDomains = allowAll
    ? dedupe(grantInjectionHosts) // deny-list only, but keep api_key injection host(s)
    : dedupe([...state.allowedDomains, ...requiredHosts]);

  const inline_policy: RunPolicySpec = {
    allowed_domains: allowedDomains,
    first_use_approval: allowAll ? "always_deny" : state.firstUseApproval,
    min_confinement_class: state.confinementClass,
  };
  if (allowAll) inline_policy.allow_all_egress = true;
  const denied = dedupe(state.deniedDomains);
  if (denied.length) inline_policy.denied_domains = denied;
  if (grants.length) inline_policy.eligible_grants = grants;
  if (workspaceMounts.length) inline_policy.workspace_mounts = workspaceMounts;
  if (workspaceRepos.length) inline_policy.workspace_repos = workspaceRepos;
  if (autoStopAfterSec !== undefined) inline_policy.auto_stop_after_sec = autoStopAfterSec;

  return { run, inline_policy };
}

// Compose the github_token grant scope. read => contents:read; read+write =>
// contents:write + pull_requests:write. The broker clamps to its ceiling.
function githubPermissionsMap(perm: GitHubPermission): Record<string, string> {
  return perm === "read+write"
    ? { contents: "write", pull_requests: "write" }
    : { contents: "read" };
}

// The LLM key target host. Anthropic for Claude Code, OpenAI for Codex.
function llmHostForSecret(agent: WizardAgent, _secret: string): string {
  return agent === "codex-cli" ? "api.openai.com" : "api.anthropic.com";
}

// The injection header + format are per-host: Anthropic wants the RAW key in
// x-api-key (the prior always-"Authorization: Bearer" was the bug); OpenAI wants
// "Authorization: Bearer <key>".
function apiKeyInjectionFor(host: string): { header: string; format: string } {
  if (host === "api.anthropic.com") return { header: "x-api-key", format: "%s" };
  return { header: "Authorization", format: "Bearer %s" };
}
