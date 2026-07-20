/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// ============================================================
// New Run permission wizard — typed state + spec composition.
//
// This is the single source of truth for the CANONICAL wire contract the
// wizard emits. buildSpec(state) returns the exact shapes the backend expects:
//   - run: CreateRunInput  (the POST /api/v1/runs scalar fields)
//   - inline_policy: RunPolicySpec  (sent inline; XOR with policy_id)
//
// Reuse existing nouns — do NOT invent shapes:
//   - mount entry is types.WorkspaceMount {source, target, read_only?}
//     (omitted read_only => read-only). Local-folder => target "/home/agent/work".
//   - github_token grant scope = {repos:[], permissions:{...}} where
//       read       => {contents:"read"}
//       read+write => {contents:"write","pull_requests":"write"}
//     (the broker clamps regardless).
//   - api_key grant scope = {host, header, secret_name, format}; format is the
//     "%s"-style wrapper "Bearer %s" (matches internal/egress /
//     injectionRuleFromScope, which defaults Format to "Bearer %s").
// ============================================================
import type {
  Agent,
  ComposeRunProposal,
  ConfinementClass,
  CreateRunInput,
  FirstUseMode,
  GrantSpec,
  RunPolicySpec,
  Workspace,
  WorkspaceMount,
  WorkspaceRepo,
  WorkspaceSelection,
} from "../../../lib/types";
import { asFirstUseMode, SUBSCRIPTION_OAUTH_SECRET } from "../../../lib/types";

export type { WorkspaceSelection };

export type WizardStepId =
  | "basics"
  | "access"
  | "egress"
  | "confinement"
  | "review";

export const WIZARD_STEPS: { id: WizardStepId; label: string }[] = [
  { id: "basics", label: "Basics" },
  { id: "access", label: "Access" },
  { id: "egress", label: "Egress" },
  { id: "confinement", label: "Confinement" },
  { id: "review", label: "Review" },
];

// Only TWO agents are valid on the wire — fix the old claude_code/codex/cursor
// bug by constraining the picker to exactly these dotted ids.
export type WizardAgent = "claude-code" | "codex-cli";

export type RunMode = "interactive" | "batch";
export type GitHubPermission = "read" | "read+write";
export type Lifecycle = "never" | "auto";

// What kind of run this is. "agent" runs a coding agent under Wardyn's harness
// (needs a model/harness — the Agent picker applies). "command" runs the task
// text as a plain shell command in the governed sandbox (task_mode=exec on the
// wire) — no agent, no model, the Agent picker is hidden/irrelevant.
export type RunType = "agent" | "command";

// How the run authenticates to Anthropic (claude-code only). API key is the
// zero-resident, secure default; subscription mounts the host OAuth creds into
// the sandbox (reduced isolation); Bedrock is a fast-follow (disabled for now).
export type AnthropicAuth = "subscription" | "apikey" | "bedrock";

// In subscription mode the wizard collects an ABSOLUTE host path to the resident
// ~/.claude directory (the wire needs an absolute path, NOT "~/.claude").
// Empty by default: the operator must type an ABSOLUTE host path. We can't
// pre-fill a valid default because "~/.claude" isn't expandable to a Docker
// mount source and the browser can't know the host's home dir. An empty field
// (with the "/home/you/.claude" placeholder + the absolute-path hint) reads as
// "needs input", instead of a pre-filled value that silently fails validation.
export const DEFAULT_CLAUDE_DIR = "";

// The curated preset egress domains the chips toggle. Custom domains are added
// separately and validated (exact host or "*.wildcard").
// Kept aligned with the scanner's marker-table registries
// (internal/workspacescan/markers.go) and the risk baseline
// (internal/composer/risk.go safeBaselineDomains) so the three lists agree.
export const PRESET_DOMAINS: string[] = [
  "github.com",
  "*.githubusercontent.com",
  "registry.npmjs.org",
  "registry.yarnpkg.com",
  "pypi.org",
  "files.pythonhosted.org",
  "proxy.golang.org",
  "repo.maven.apache.org",
  "services.gradle.org",
  "plugins.gradle.org",
  "crates.io",
  "rubygems.org",
  "api.anthropic.com",
  "api.openai.com",
];

export interface WizardState {
  // --- Step 1: basics ---
  // "agent" (default) vs "command" (task_mode=exec, no agent/model involved).
  runType: RunType;
  agent: WizardAgent;
  // Onboarded workspaces attached to this run — ONLY sources onboarded via
  // /workspaces may be selected (the picker fetches listWorkspaces() and offers
  // nothing else). The FIRST entry is the primary: its kind/source drives the
  // run's `repo` label and (for a repo) the image resolution; buildSpec resolves
  // each selection's workspaceId against the fetched Workspace list into a
  // workspace_mounts[] (local_dir) or workspace_repos[] (repo) entry.
  workspaces: WorkspaceSelection[];
  mode: RunMode;
  task: string;
  // A saved profile (policy id) picked on the Basics step for the primary workspace.
  // When set, the profile has populated steps 2-4 and the wizard offers "Review Now"
  // to fast-track straight to Review. Cleared when the workspace selection changes.
  selectedProfile?: string;

  // --- Step 2: access ---
  githubEnabled: boolean;
  githubRepos: string; // comma/space separated "org/repo" list
  githubPermission: GitHubPermission;
  githubRequiresApproval: boolean;
  githubTtlMinutes: number;
  llmSecretName: string; // selected secret name for the LLM api_key grant ("" = none)
  // git_pat grant: broker a STORED Personal Access Token to git for a non-GitHub
  // host (Azure DevOps / GitLab). Unlike the LLM api_key (proxy-injected, value
  // never returned), the PAT value reaches git via the credential helper as the
  // password. Host is reached over plain CONNECT egress (like github), so
  // buildSpec unions gitPatHost into allowed_domains.
  gitPatEnabled: boolean;
  gitPatHost: string; // e.g. dev.azure.com or gitlab.com
  gitPatSecretName: string; // stored secret name holding the PAT ("" = none)
  gitPatUsername: string; // optional git username override (ADO=pat, GitLab=oauth2 by default)
  gitPatRequiresApproval: boolean;
  // Anthropic auth mode (claude-code only). codex-cli always uses the OpenAI
  // api_key path regardless of this field.
  anthropicAuth: AnthropicAuth;
  // Absolute host path to the resident ~/.claude dir, used only in subscription
  // mode. Mounted at /home/agent/.claude; the sibling .json is derived/mounted too.
  subscriptionClaudeDir: string;

  // --- Step 3: egress ---
  allowedDomains: string[]; // selected preset + custom domains
  deniedDomains: string[];
  firstUseApproval: FirstUseMode;
  // When ON the proxy runs deny-list only: any non-denied public host is allowed,
  // allowed_domains may be empty, and first-use approval is inert.
  allowAllEgress: boolean;

  // --- Step 4: confinement + lifecycle ---
  confinementClass: ConfinementClass;
  lifecycle: Lifecycle;
  autoStopMinutes: number;

  // --- Step 5: review ---
  saveAsProfile: boolean;
  profileName: string;

  // --- Step 1: basics — Bring Your Own Image ---
  // A user-supplied base image ref. When set, the backend wraps it with the
  // runner tools before use (see CreateRunInput.image). "" = the convention image.
  image: string;
}

export function initialWizardState(defaultCc: ConfinementClass = "CC1"): WizardState {
  return {
    runType: "agent",
    agent: "claude-code",
    workspaces: [],
    mode: "interactive",
    task: "",

    githubEnabled: false,
    githubRepos: "",
    githubPermission: "read",
    githubRequiresApproval: true,
    githubTtlMinutes: 60,
    llmSecretName: "",
    gitPatEnabled: false,
    gitPatHost: "",
    gitPatSecretName: "",
    gitPatUsername: "",
    // Default to approval-gated: a PAT is a long-lived, non-expirable secret, so
    // its first use should route through a human approval by default.
    gitPatRequiresApproval: true,
    // API key is the secure, zero-resident default.
    anthropicAuth: "apikey",
    subscriptionClaudeDir: DEFAULT_CLAUDE_DIR,

    allowedDomains: ["api.anthropic.com"],
    deniedDomains: [],
    firstUseApproval: "deny_with_review",
    allowAllEgress: false,

    confinementClass: defaultCc,
    lifecycle: "never",
    autoStopMinutes: 60,

    saveAsProfile: false,
    profileName: "",

    image: "",
  };
}

// basename("/a/b/c") => "c"; tolerant of trailing slashes and bare paths.
export function basename(path: string): string {
  const cleaned = path.replace(/\/+$/, "");
  const parts = cleaned.split("/").filter(Boolean);
  return parts.length ? parts[parts.length - 1] : cleaned || "workspace";
}

// A domain entry is valid if it's an exact host (a.b.c) or a single-label
// wildcard prefix ("*.b.c"). No scheme, no path, no port.
export function isValidDomain(d: string): boolean {
  const s = d.trim();
  if (!s) return false;
  if (/[\s/:]/.test(s)) return false;
  const host = s.startsWith("*.") ? s.slice(2) : s;
  if (!host) return false;
  // each label: alphanumerics + hyphen, not leading/trailing hyphen; >=2 labels.
  return /^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$/i.test(host);
}

// Split a free-text repo list ("org/a, org/b") into trimmed non-empty entries.
function parseRepoList(raw: string): string[] {
  return raw
    .split(/[\s,]+/)
    .map((r) => r.trim())
    .filter(Boolean);
}

// Compose the github_token grant scope. read => contents:read; read+write =>
// contents:write + pull_requests:write. The broker clamps to its ceiling.
function githubPermissionsMap(perm: GitHubPermission): Record<string, string> {
  return perm === "read+write"
    ? { contents: "write", pull_requests: "write" }
    : { contents: "read" };
}

// Resolve one WorkspaceSelection against the fetched onboarded-workspace list
// into its onboarded kind/source/name. Returns undefined for a stale selection
// (the workspace was deleted after it was picked) — buildSpec defensively skips
// those rather than emitting a dangling reference.
function resolveWorkspace(sel: WorkspaceSelection, workspaces: Workspace[]): Workspace | undefined {
  return workspaces.find((w) => w.id === sel.workspaceId);
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
  run: CreateRunInput;
  inline_policy: RunPolicySpec;
} {
  const interactive = state.mode === "interactive";

  // --- run scalars ---
  const run: CreateRunInput = {
    agent: state.agent as Agent,
    repo: "",
    // An interactive run comes up idle (the backend ignores task for it), but
    // sending the trimmed task is harmless and preserves it for display.
    task: state.task.trim(),
    confinement_class: state.confinementClass,
    interactive,
  };
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

  // --- onboarded workspace selections -> workspace_mounts[] / workspace_repos[]
  // ---
  // The FIRST selection is the PRIMARY: its kind/source drives the run's `repo`
  // label (and, per the synthesis doc, the sandbox's base image) — additional
  // selections are just attached alongside it.
  const workspaceMounts: WorkspaceMount[] = [];
  const workspaceRepos: WorkspaceRepo[] = [];
  let hasRepoSelection = false;
  state.workspaces.forEach((sel, i) => {
    const w = resolveWorkspace(sel, workspaces);
    if (!w) return; // stale selection — defensively skip rather than dangle
    const target = sel.target?.trim() || w.default_target?.trim() || undefined;
    if (w.kind === "container") {
      // A container workspace is the run's base IMAGE, not a mount. The backend
      // resolves it back to this onboarded workspace by image ref
      // (GetWorkspaceBySource) to inherit its bound model/harness creds —
      // emitting it as a workspace_mount would fail the onboarded-mount gate (an
      // image ref is not an onboarded local_dir source). An explicit BYO image
      // (the Sandbox image field) still wins if the operator set one.
      if (!run.image) run.image = w.source;
    } else if (w.kind === "repo") {
      hasRepoSelection = true;
      const entry: WorkspaceRepo = { repo: w.source };
      if (target) entry.target = target;
      workspaceRepos.push(entry);
    } else {
      workspaceMounts.push({
        source: w.source,
        // Mount at the agent's working dir (~/work = /home/agent/work) by
        // convention — that's where `claude` and the `wardyn attach` shell
        // start. A workspace's own default_target, or a per-run override,
        // takes precedence.
        target: target || "/home/agent/work",
        read_only: !!sel.readOnly,
      });
    }
    if (i === 0) {
      // Synthetic repo label so the run row reads meaningfully. A container has
      // no repo/mount source — it rides run.image — so it carries no label.
      run.repo =
        w.kind === "repo" ? w.source : w.kind === "container" ? "" : `local:${basename(w.source)}`;
    }
  });

  // Claude-code subscription mode: mount the resident OAuth creds instead of
  // granting an api_key. The presence of these mounts is how agent-run DETECTS
  // subscription (it then unsets ANTHROPIC_BASE_URL so claude uses the resident
  // OAuth creds via the HTTPS_PROXY CONNECT tunnel).
  const isSubscription =
    state.agent === "claude-code" && state.anthropicAuth === "subscription";

  if (isSubscription) {
    const dir = state.subscriptionClaudeDir.trim().replace(/\/+$/, "");
    if (dir) {
      workspaceMounts.push({
        source: dir,
        target: "/home/agent/.claude",
        read_only: true,
      });
      // Mount the sibling ~/.claude.json (the host stores it next to the dir).
      const jsonPath = claudeJsonSibling(dir);
      if (jsonPath) {
        workspaceMounts.push({
          source: jsonPath,
          target: "/home/agent/.claude.json",
          read_only: true,
        });
      }
    }
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
  if (state.gitPatEnabled && state.gitPatHost.trim() && state.gitPatSecretName.trim()) {
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

  // The LLM api_key grant. Subscription mode uses resident OAuth creds instead,
  // so it never adds an api_key grant.
  if (state.llmSecretName && !isSubscription) {
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
  // approval. This only unions in hosts the operator already opted into via the
  // Access step — it never broadens beyond the run's own granted capabilities.
  const requiredHosts: string[] = [];
  if (state.llmSecretName && !isSubscription) {
    requiredHosts.push(llmHostForSecret(state.agent, state.llmSecretName));
  }
  // Subscription claude needs api.anthropic.com reachable for the OAuth tunnel.
  if (isSubscription) requiredHosts.push("api.anthropic.com");
  // any repo-kind selection unions the GitHub clone hosts — the
  // overwhelmingly common case. A repo onboarded from a non-GitHub host still
  // works (its clone egress comes from the workspace's own scanned profile
  // server-side, or from the operator's custom allowed_domains / git_pat host
  // below); this just covers the default GitHub case without parsing the URL.
  if (state.githubEnabled || hasRepoSelection) {
    requiredHosts.push("github.com", "*.githubusercontent.com");
  }
  // git_pat is reached over plain CONNECT egress (like github), NOT a proxy
  // injection rule — so union its host into allowed_domains here. Forgetting
  // this gates the clone behind first-use approval.
  if (state.gitPatEnabled && state.gitPatHost.trim()) {
    requiredHosts.push(state.gitPatHost.trim());
  }

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
  if (state.llmSecretName && !isSubscription) {
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

// Derive the resident ~/.claude.json path from the ~/.claude dir by stripping a
// trailing slash and appending ".json" (".../.claude" => ".../.claude.json").
function claudeJsonSibling(dir: string): string {
  const cleaned = dir.replace(/\/+$/, "");
  if (!cleaned) return "";
  return `${cleaned}.json`;
}

function dedupe(xs: string[]): string[] {
  return Array.from(new Set(xs.map((x) => x.trim()).filter(Boolean)));
}

// Map a composer PROPOSAL (run scalars + clamped inline_policy) back into wizard
// state so "Edit in wizard" lands the operator in the existing 5-step flow with
// the proposal prefilled. This is a best-effort INVERSE of buildSpec — it can't
// always perfectly round-trip (e.g. it can't recover which preset domains were
// toggled vs typed), but it reproduces a launch-equivalent state.
//
// `workspaces` is the onboarded-workspace list (the caller fetches it via
// listWorkspaces()) used to re-resolve the proposal's raw mount source / repo
// string back into a WorkspaceSelection. The composer's workspace is still a
// single operator-chosen source (not onboarding-aware) but, per the run-create
// mount-restriction gate, it must already reference an onboarded source to have
// been proposable at all — so matching by source is a reliable inverse.
// an omitted/empty `workspaces` (a call site that hasn't loaded the
// list yet) degrades to no workspace prefilled — a known, documented gap, not a
// crash — the operator just re-picks it in the Basics step.
export function wizardStateFromProposal(
  run: ComposeRunProposal,
  spec: RunPolicySpec,
  workspaces: Workspace[] = [],
): WizardState {
  const cc = (run.confinement_class ?? spec.min_confinement_class ?? "CC1") as ConfinementClass;
  const base = initialWizardState(cc);

  // The agent is constrained to the two dotted wire ids; tolerate either form.
  const agent: WizardAgent =
    String(run.agent).replace(/_/g, "-").startsWith("codex") ? "codex-cli" : "claude-code";

  // Workspace: a "local:<name>" repo label + a host mount at ~/work => a local
  // folder; otherwise an org/repo github clone. Resolve it against the
  // onboarded list by source so it becomes a real WorkspaceSelection.
  const workMount = (spec.workspace_mounts ?? []).find(
    (m) => m.target === "/home/agent/work",
  );
  const matched = workMount
    ? workspaces.find((w) => w.kind === "local_dir" && w.source === workMount.source)
    : workspaces.find((w) => w.kind === "repo" && w.source === run.repo);
  const workspaceSelections: WorkspaceSelection[] = matched
    ? [{ workspaceId: matched.id, readOnly: workMount ? !!workMount.read_only : undefined }]
    : [];

  const githubGrant = (spec.eligible_grants ?? []).find((g) => g.kind === "github_token");
  const apiKeyGrant = (spec.eligible_grants ?? []).find((g) => g.kind === "api_key");
  const apiKeySecret = (apiKeyGrant?.scope?.secret_name as string) ?? "";

  // Subscription claude is signalled EITHER by the resident ~/.claude mount OR by
  // an api_key grant naming the subscription OAuth sentinel — the latter is how a
  // RECORDED profile carries subscription auth (recordings never synthesize the
  // mount). Recognizing the sentinel here is what stops fromSpec from carrying it
  // into llmSecretName and re-emitting a broken x-api-key grant to a secret that
  // doesn't exist (the "references unknown secret" launch failure).
  const claudeMount = (spec.workspace_mounts ?? []).find(
    (m) => m.target === "/home/agent/.claude",
  );
  const recordedSubscription = apiKeySecret === SUBSCRIPTION_OAUTH_SECRET;
  const isSubscription = agent === "claude-code" && (!!claudeMount || recordedSubscription);
  const ghScope = (githubGrant?.scope ?? {}) as {
    repos?: unknown;
    permissions?: Record<string, unknown>;
  };
  const ghPerm: GitHubPermission =
    ghScope.permissions && ghScope.permissions.contents === "write" ? "read+write" : "read";

  return {
    ...base,
    agent,
    workspaces: workspaceSelections,
    mode: run.interactive ? "interactive" : "batch",
    task: run.task ?? "",

    githubEnabled: !!githubGrant,
    githubRepos: Array.isArray(ghScope.repos) ? (ghScope.repos as string[]).join(", ") : "",
    githubPermission: ghPerm,
    githubRequiresApproval: githubGrant?.requires_approval ?? base.githubRequiresApproval,
    githubTtlMinutes: githubGrant?.ttl_seconds
      ? Math.max(1, Math.round(githubGrant.ttl_seconds / 60))
      : base.githubTtlMinutes,
    // The api_key grant references a stored secret by name; carry it forward so
    // the wizard re-emits the same grant — EXCEPT the subscription sentinel, which
    // is not a real stored secret (it means "subscription auth", handled above).
    llmSecretName: recordedSubscription ? "" : apiKeySecret,
    anthropicAuth: isSubscription ? "subscription" : "apikey",
    subscriptionClaudeDir: claudeMount?.source ?? base.subscriptionClaudeDir,

    allowedDomains: spec.allowed_domains?.length ? dedupe(spec.allowed_domains) : base.allowedDomains,
    deniedDomains: dedupe(spec.denied_domains ?? []),
    firstUseApproval: asFirstUseMode(spec.first_use_approval),
    allowAllEgress: spec.allow_all_egress === true,

    confinementClass: cc,
    lifecycle: spec.auto_stop_after_sec === -1 ? "never" : "auto",
    autoStopMinutes:
      spec.auto_stop_after_sec != null && spec.auto_stop_after_sec > 0
        ? Math.max(1, Math.round(spec.auto_stop_after_sec / 60))
        : base.autoStopMinutes,
  };
}

// Per-step validation. Returns null when the step is valid, else an error string
// the wizard renders inline and uses to gate Next/Launch.
// A workspace's recorded PROFILES: its settled OPEN recordings. Each is tied to the
// workspace by construction (it lives in the workspace's record_results) — no naming
// heuristic or policy↔workspace FK needed — and synthesizes a full least-privilege
// policy on demand (api.profileRun). Confined verify replays + failed captures are
// excluded (they aren't the canonical learned profile).
export type WorkspaceProfileOption = { key: string; label: string; runId: string };
export function workspaceProfileOptions(ws: Workspace | undefined): WorkspaceProfileOption[] {
  if (!ws) return [];
  return Object.entries(ws.record_results ?? {})
    .filter(([, v]) => v.status === "recorded" && !v.confined && !!v.run_id)
    .map(([key, v]) => ({ key, label: v.label || key, runId: v.run_id }));
}

// applyProfileSpecToState loads a recorded profile's synthesized spec into the wizard's
// steps 2-4 (access, egress, confinement) while KEEPING the operator's Basics choices
// (agent, mode, task, workspace). Sets selectedProfile so the footer can fast-track.
export function applyProfileSpecToState(
  state: WizardState,
  spec: RunPolicySpec,
  workspaces: Workspace[],
  profileKey: string,
): WizardState {
  const primary = state.workspaces[0]
    ? workspaces.find((w) => w.id === state.workspaces[0].workspaceId)
    : undefined;
  const run: ComposeRunProposal = {
    agent: state.agent as Agent,
    repo: primary ? (primary.kind === "repo" ? primary.source : `local:${basename(primary.source)}`) : "",
    task: state.task,
    interactive: state.mode === "interactive",
    confinement_class: spec.min_confinement_class,
  };
  const applied = wizardStateFromProposal(run, spec, workspaces);
  return {
    ...applied,
    agent: state.agent,
    mode: state.mode,
    task: state.task,
    workspaces: state.workspaces,
    selectedProfile: profileKey,
    // Already based on a saved profile — don't also offer to re-save it as a policy.
    saveAsProfile: false,
  };
}

export function validateStep(id: WizardStepId, state: WizardState): string | null {
  switch (id) {
    case "basics": {
      // A workspace is OPTIONAL: zero mounts => an ephemeral scratch run (buildSpec
      // leaves repo "" and emits no workspace_mounts/repos). Only a batch run needs
      // a task; an interactive run comes up idle for the operator to drive.
      if (state.mode === "batch" && !state.task.trim())
        return "A batch run needs a task to perform.";
      return null;
    }
    case "access": {
      if (state.githubEnabled && !parseRepoList(state.githubRepos).length)
        return "Add at least one repo for the GitHub token, or disable it.";
      if (state.githubEnabled && state.githubTtlMinutes <= 0)
        return "GitHub token TTL must be a positive number of minutes.";
      if (state.agent === "claude-code" && state.anthropicAuth === "subscription") {
        const dir = state.subscriptionClaudeDir.trim();
        if (!dir) return "Enter the host path to your ~/.claude directory.";
        if (!dir.startsWith("/"))
          return "The ~/.claude path must be absolute (e.g. /home/you/.claude).";
      }
      return null;
    }
    case "egress": {
      // Allow-all egress is deny-list only — no allowed domain is required.
      if (state.allowAllEgress) return null;
      if (!dedupe(state.allowedDomains).length)
        return "Allow at least one egress domain.";
      return null;
    }
    case "confinement": {
      if (state.lifecycle === "auto" && state.autoStopMinutes <= 0)
        return "Auto-stop window must be a positive number of minutes.";
      return null;
    }
    case "review": {
      if (state.saveAsProfile && !state.profileName.trim())
        return "Name the profile, or turn off save-as-profile.";
      return null;
    }
    default:
      return null;
  }
}
