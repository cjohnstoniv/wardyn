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
// egress-host list buildSpec and mergeRunSelections both read (D6/claim3) so
// the two can never drift. Re-exported from wizard-types.ts so every existing
// importer keeps working unchanged.
//
// Since the Policy panel took over /runs/new, buildSpec's own envelope
// (allowed/denied domains, first_use_approval, the barrier floor, the
// lifecycle) is no longer what the run screen ships — the operator's spec JSON
// is. buildSpec still composes the `run` scalars, the Workspace card's
// mounts/repos and the grant lanes, and mergeRunSelections unions exactly those
// back into the authored document.
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

// The ONE list of grant-implied egress hosts — unioned into allowed_domains so
// a granted capability is never silently gated behind first-use approval. Both
// consumers read THIS list: buildSpec (below) and mergeRunSelections, whose
// "Added for this run's selections" line names each host on screen, so what the
// operator is shown and what actually ships cannot drift (D6/claim3).
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
  // The member's USER DRIVE, requested as a bare FLAG — nothing here names a
  // drive, a path or a directory (CreateRunRequest.Drive): the server resolves
  // what is allocated to the authenticated caller, so this can only ever ask
  // for storage that caller was already granted.
  //
  // Emitted ONLY when the checkbox is on. An absent `drive` is "mount
  // nothing", byte for byte the body every run sent before the field existed,
  // so a run that never touched the control is unchanged on the wire. And
  // read_only rides along only when this run NARROWS a writable allocation:
  // read_only:false cannot widen one — the server refuses it as an attempt to
  // — so `false` is never sent.
  if (state.driveEnabled) {
    run.drive = state.driveReadOnly ? { enabled: true, read_only: true } : { enabled: true };
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

/* ---------- the post-parse merge (new-run's Policy panel) ---------- */

// What the run's own SELECTIONS added on top of the spec the operator wrote.
// Rendered verbatim next to the panel ("Added for this run's selections: …"):
// a union the operator cannot see is a policy they did not author.
export interface SpecAdditions {
  /** allowed_domains entries the JSON did not already carry. */
  hosts: string[];
  /** eligible_grants the JSON did not already carry. */
  grants: GrantSpec[];
  mounts: WorkspaceMount[];
  repos: WorkspaceRepo[];
}

// Deep equality over the FULL canonicalized GrantSpec — kind + scope +
// ttl_seconds + requires_approval (types.go's GrantSpec). Exact duplicates
// ONLY: any PARTIAL key silently drops a real grant (github_token has no
// secret name at all; one secret_name can serve two hosts; two grants equal on
// scope can still differ on requires_approval/TTL). Key-sorted so JSON authored
// by hand matches a composed grant that spells the same scope in another order;
// array order INSIDE a scope is left as-is — a kept near-duplicate is harmless,
// a dropped grant is not. Absent ttl/scope/requires_approval normalize to the
// values the Go decoder would default them to, so "omitted" and "written out"
// are one grant, not two.
function grantKey(g: GrantSpec): string {
  const sortKeys = (v: unknown): unknown =>
    v && typeof v === "object" && !Array.isArray(v)
      ? Object.fromEntries(
          Object.keys(v as object)
            .sort()
            .map((k) => [k, sortKeys((v as Record<string, unknown>)[k])]),
        )
      : v;
  return JSON.stringify({
    kind: g.kind,
    scope: sortKeys(g.scope ?? {}),
    ttl_seconds: g.ttl_seconds ?? 0,
    requires_approval: g.requires_approval === true,
  });
}

// mergeRunSelections is the run screen's post-parse union, and the ONE place it
// happens: the panel's JSON is the authored policy, and this adds back only
// what the JSON cannot know — the Workspace card's mounts/repos, the grant
// lanes' own grants, and the egress hosts those grants require.
//
// The JSON OWNS everything else (the egress keys, first_use_approval,
// min_confinement_class, auto_stop_after_sec, llm_inspection, resources): a
// merge that overwrote them would silently un-author the document on screen.
export function mergeRunSelections(
  authored: RunPolicySpec,
  state: WizardState,
  workspaces: Workspace[] = [],
): { spec: RunPolicySpec; added: SpecAdditions } {
  const { inline_policy: composed } = buildSpec(state, workspaces);

  // --- eligible_grants: authored ∪ composed, deep-equal dedupe ---
  const grants = [...(authored.eligible_grants ?? [])];
  const seen = new Set(grants.map(grantKey));
  const addedGrants: GrantSpec[] = [];
  for (const g of composed.eligible_grants ?? []) {
    const k = grantKey(g);
    if (seen.has(k)) continue;
    seen.add(k);
    grants.push(g);
    addedGrants.push(g);
  }

  // --- allowed_domains: grant-implied hosts + the credential-injection pin ---
  // The pin is derived from the UNION's api_key grants, NOT from wizard state:
  // proxy credential injection only rewrites requests whose host is already on
  // allowed_domains, and that holds under allow_all_egress too
  // (types/policy.go's allow_all_egress note). A hand-written api_key grant
  // whose host nobody pinned authenticates nothing.
  const pins = grants
    .filter((g) => g.kind === "api_key")
    .map((g) => (typeof g.scope?.host === "string" ? g.scope.host.trim() : ""))
    .filter(Boolean);
  const have = new Set(authored.allowed_domains ?? []);
  const hosts = dedupe([
    ...impliedEgressHosts(state, workspaces).map((h) => h.host),
    ...pins,
  ]).filter((h) => !have.has(h));

  const added: SpecAdditions = {
    hosts,
    grants: addedGrants,
    mounts: composed.workspace_mounts ?? [],
    repos: composed.workspace_repos ?? [],
  };

  const spec: RunPolicySpec = { ...authored };
  if (hosts.length) spec.allowed_domains = [...(authored.allowed_domains ?? []), ...hosts];
  if (addedGrants.length) spec.eligible_grants = grants;
  if (added.mounts.length) {
    spec.workspace_mounts = [...(authored.workspace_mounts ?? []), ...added.mounts];
  }
  if (added.repos.length) {
    spec.workspace_repos = [...(authored.workspace_repos ?? []), ...added.repos];
  }
  return { spec, added };
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
