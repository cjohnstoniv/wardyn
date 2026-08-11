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
import type {
  WorkspaceRequirementsMap,
  WorkspaceSourceInput,
} from "../../../lib/types";
import { effectiveWorkspaceRequirements } from "../../../lib/types";

export type { WorkspaceSelection };

// A run-creation-time selection, widened with the per-run requirements-contract
// options CreateRunRequest.Workspaces carries (pkg/client/client.go's
// WorkspaceSelection.EnabledOptional/ReadOnly) — lib/types/workspaces.ts's
// WorkspaceSelection doesn't carry enabledOptional yet, so extend locally like
// the import above rather than widening the shared type mid-flight.
export interface RunWorkspaceSelection extends WorkspaceSelection {
  // Optional-requirement KEYS ("secret:NAME" | "egress:host" | "write:/path")
  // this run opts into for this workspace. A Required entry never needs to be
  // listed — it applies automatically. Wire: WorkspaceSelection.enabled_optional.
  enabledOptional?: string[];
}

// What a run against `ws` is actually held to: the server's FOLD of attached
// sources' contracts under the workspace's own overlay. One shared reader so
// every New Run surface (picker chips, preflight, review) reads the same
// contract the create-run gate enforces.
export function workspaceRequirements(ws: Workspace): WorkspaceRequirementsMap {
  return effectiveWorkspaceRequirements(ws);
}

// The derived sources view, in attachment order (sources[0] is primary).
function workspaceSources(ws: Workspace): WorkspaceSourceInput[] {
  return ws.sources ?? [];
}

// splitRequirementKey mirrors internal/api/workspaces.go's splitRequirementKey:
// split on the FIRST colon only — a write:/host/path value may itself legally
// contain colons, so the type prefix must never be recovered from the last one.
function splitRequirementKey(key: string): { type: string; name: string } | null {
  const i = key.indexOf(":");
  if (i < 0) return null;
  return { type: key.slice(0, i), name: key.slice(i + 1) };
}

// One requirements-contract entry as the picker needs it: the bare name/host/path
// for display, plus the full "<type>:<key>" wire key for enabledOptional.
export interface RequirementEntry {
  key: string;
  name: string;
}

// A workspace's requirements contract, grouped for display — what
// workspace-picker.tsx's "Comes with:" summary + optional-toggle checkboxes and
// step-review.tsx's Review grid both render from, so the two surfaces can't
// disagree about what a workspace carries. Required entries ride along with
// every run automatically (never a checkbox); Optional entries are the per-run
// opt-ins that become buildSpec's enabled_optional.
export interface RequirementsSummary {
  requiredSecrets: string[];
  optionalSecrets: RequirementEntry[];
  requiredHosts: string[];
  optionalHosts: RequirementEntry[];
  requiredWrite: boolean;
  // Usually 0 or 1 (one write:<path> per local_dir source), but kept as a list
  // since a multi-source workspace can in principle declare more than one.
  optionalWriteKeys: string[];
}

export function summarizeWorkspaceRequirements(ws: Workspace): RequirementsSummary {
  const out: RequirementsSummary = {
    requiredSecrets: [],
    optionalSecrets: [],
    requiredHosts: [],
    optionalHosts: [],
    requiredWrite: false,
    optionalWriteKeys: [],
  };
  const reqs = workspaceRequirements(ws);
  for (const key of Object.keys(reqs).sort()) {
    const split = splitRequirementKey(key);
    if (!split) continue; // defense only — the write endpoint already rejects a bad key
    const { type, name } = split;
    const required = reqs[key].level === "required";
    if (type === "secret") {
      if (required) out.requiredSecrets.push(name);
      else out.optionalSecrets.push({ key, name });
    } else if (type === "egress") {
      if (required) out.requiredHosts.push(name);
      else out.optionalHosts.push({ key, name });
    } else if (type === "write") {
      if (required) out.requiredWrite = true;
      else out.optionalWriteKeys.push(key);
    }
  }
  return out;
}

// The picker/detail "Comes with: …" line (mirrors the approved mock's
// comesWith()): a short "N secrets · N hosts · write access" summary, or the
// honest fallback when the contract adds nothing beyond the auto-allowed set.
// `sel` is OPTIONAL and additive: pass this run's selection (workspace-picker.tsx,
// step-review.tsx) to fold in what the operator opted into for THIS run; omit it
// (workspace-detail.tsx, which has no per-run selection) for the contract-only
// summary — behavior is byte-identical to before when omitted.
export function comesWithLine(ws: Workspace, sel?: RunWorkspaceSelection): string {
  const s = summarizeWorkspaceRequirements(ws);
  const enabled = new Set(sel?.enabledOptional ?? []);
  const bits: string[] = [];
  if (s.requiredSecrets.length) {
    bits.push(`${s.requiredSecrets.length} secret${s.requiredSecrets.length > 1 ? "s" : ""}`);
  }
  if (s.requiredHosts.length) {
    bits.push(`${s.requiredHosts.length} host${s.requiredHosts.length > 1 ? "s" : ""}`);
  }
  // Write access reflects what THIS run actually resolves to — a Required
  // write, or an Optional one this selection enabled — the SAME reader
  // buildSpec's mount uses (resolvedMountReadOnly), not just the static
  // Required flag, so toggling the write switch moves this line too.
  if (ws.kind === "local_dir" && !resolvedMountReadOnly(ws, sel ?? { workspaceId: ws.id })) {
    bits.push("write access");
  }
  // Per-run opt-ins into the Optional secret/host set (workspace-picker.tsx's
  // checkboxes) — "start from the workspace and edit from there" means an edit
  // must show up here, not just on the picker card that made it. Honesty
  // constraint: an opted-in SECRET whose provenance is scan_seeded is never
  // actually granted (secretAutoGrants' trust boundary — runs_create.go's
  // applyWorkspaceRequirements skips anything but operator_set), so it only
  // counts here when it will really take effect; an opted-in HOST has no such
  // gate (the "egress" case there is provenance-blind) and always counts.
  let optedIn = 0;
  for (const e of s.optionalSecrets) {
    if (enabled.has(e.key) && secretAutoGrants(ws, e.name)) optedIn++;
  }
  for (const e of s.optionalHosts) {
    if (enabled.has(e.key)) optedIn++;
  }
  if (optedIn > 0) bits.push(`${optedIn} opted in`);
  return bits.length ? bits.join(" · ") : "nothing beyond the auto-allowed set";
}

// Required secrets this workspace declares that aren't in the stored-secret
// list — the picker's "⚠ N secrets it requires aren't stored" attention line.
export function unstoredRequiredSecrets(ws: Workspace, storedSecrets: string[]): string[] {
  return summarizeWorkspaceRequirements(ws).requiredSecrets.filter((n) => !storedSecrets.includes(n));
}

// Whether a workspace's `secret:<name>` requirement will actually be
// auto-granted at run-create — the TRUST BOUNDARY in
// internal/api/runs_create.go's applyWorkspaceRequirements: only an
// operator_set row ever auto-mints a grant; a scan_seeded row NEVER does,
// required or optional, stored or not (untrusted repo content must never
// route the operator's own stored secrets into a run just by naming them).
// A Required-and-not-auto-granting row must never be presented as "you get
// this automatically" — see the plan's honesty constraints.
export function secretAutoGrants(ws: Workspace, name: string): boolean {
  return workspaceRequirements(ws)[`secret:${name}`]?.provenance === "operator_set";
}

// A multi-source workspace's composition ("2 dirs · 1 repo") instead of a
// single kind label. Returns null for a single-source (or sources-less, i.e.
// pre-composition) workspace, so the caller falls back to its ordinary
// single-kind rendering — only a genuinely multi-source workspace needs this.
export function compositionSummary(ws: Workspace): string | null {
  const sources = workspaceSources(ws);
  if (sources.length <= 1) return null;
  const counts = { local_dir: 0, repo: 0, ephemeral: 0 };
  for (const s of sources) if (s.type in counts) counts[s.type]++;
  const bits: string[] = [];
  if (counts.local_dir) bits.push(`${counts.local_dir} dir${counts.local_dir > 1 ? "s" : ""}`);
  if (counts.repo) bits.push(`${counts.repo} repo${counts.repo > 1 ? "s" : ""}`);
  if (counts.ephemeral) bits.push(`${counts.ephemeral} scratch dir${counts.ephemeral > 1 ? "s" : ""}`);
  return bits.join(" · ");
}

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
// SEAM: there is no harness/tool-catalog endpoint exposed to the UI yet (no GET
// listing installable agent CLIs) — if one ships, read the picker's options from
// it instead of hand-adding a third literal here.
export type WizardAgent = "claude-code" | "codex-cli";

export type RunMode = "interactive" | "batch";
export type GitHubPermission = "read" | "read+write";
export type Lifecycle = "never" | "auto";

// What kind of run this is. "agent" runs a coding agent under Wardyn's harness
// (needs a model/harness — the Agent picker applies). "command" runs the task
// text as a plain shell command in the governed sandbox (task_mode=exec on the
// wire) — no agent, no model, the Agent picker is hidden/irrelevant.
export type RunType = "agent" | "command";

// RETIRED: AnthropicAuth ("subscription"|"apikey"|"bedrock") + DEFAULT_CLAUDE_DIR
// used to let a run pick its own Anthropic auth mode and mount an ad-hoc host
// ~/.claude path per launch. Model access no longer comes from per-run auth
// cards — it RESOLVES from integrations (run override -> workspace pin ->
// server default -> honest none; see step-access.tsx). A subscription is now an
// ai_provider integration configured once (Integrations), not typed in per run.

// The curated preset egress domains the chips toggle. Custom domains are added
// separately (see isValidDomain below for what the client does and doesn't check).
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
  // workspace_mounts[] (local_dir) or workspace_repos[] (repo) entry, PLUS a
  // run.workspaces[] entry carrying its enabledOptional/readOnly options.
  workspaces: RunWorkspaceSelection[];
  mode: RunMode;
  task: string;
  // The Basics "start from" picker's current value — either a recorded profile's
  // key or a saved policy's id. When set, that source has populated steps 2-4 and
  // the wizard offers "Review Now" to fast-track straight to Review. Cleared when
  // the workspace selection changes.
  selectedProfile?: string;
  // Set ONLY when the picked source was a SAVED POLICY: the run then launches by
  // REFERENCE (policy_id) instead of an inline_policy, so the server enforces the
  // stored spec verbatim. Any hand edit detaches it (see the wizard's patch
  // funnel) — buildSpec's composed spec is not a round-trip of a stored one.
  selectedPolicyId?: string;

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
  // Run override for model/harness access: pins this run to a SPECIFIC
  // ai_provider integration (Integration.ID), overriding the workspace pin and
  // server default — see step-access.tsx's "Override for this run…" peek and
  // CreateRunRequest.IntegrationID. Unset => resolves normally.
  integrationId?: string;

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
    // Deliberately the OPPOSITE of the AI Composer's default (autonomous) —
    // not a mismatch to "fix" into uniformity. Describe always seeds a
    // described TASK, so there is by construction something to run
    // unattended; this envelope-first path has no task yet, so interactive is
    // what keeps a fresh wizard reachable at zero required keystrokes (an
    // ephemeral run needs neither workspace nor task). Forcing Task required
    // here to match the composer would trade that shortcut for cosmetic
    // parity. See pass3-ux-proposal.md §4.
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

// A deliberately LOOSE paste-guard — NOT a second copy of the shape rule. The
// one canonical check is ValidDomainEntry (internal/egress/proxy/policy.go),
// which every policy ingest point runs and which accepts a bare host, a
// leading-"*." wildcard, either with a ":port" qualifier, and IP literals; its
// 422 quotes the offending entry and lists the supported forms, and the wizard
// already routes the whole spec through it at preflight. So this only has to
// catch the obvious slip (empty, or a pasted URL). It must never be STRICTER
// than the server: the old TLD regex + ":" rejection here is exactly what made
// "artifactory.corp:8443", "registry:5000" and IP literals — all valid stored
// policy — untypeable in the console.
export function isValidDomain(d: string): boolean {
  const s = d.trim();
  if (!s) return false;
  return !/[\s/]/.test(s);
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

// The ONE predicate for "is there a real git_pat grant" — shared by buildSpec's
// grant emission, its requiredHosts union, and validateStep's error, so a
// half-configured PAT (host with no secret, or vice versa) can never widen
// egress for a grant that was never minted (D5/claim4).
function gitPatConfigured(state: WizardState): boolean {
  return state.gitPatEnabled && !!state.gitPatHost.trim() && !!state.gitPatSecretName.trim();
}

// Resolve one WorkspaceSelection against the fetched onboarded-workspace list
// into its onboarded kind/source/name. Returns undefined for a stale selection
// (the workspace was deleted after it was picked) — buildSpec defensively skips
// those rather than emitting a dangling reference.
function resolveWorkspace(sel: WorkspaceSelection, workspaces: Workspace[]): Workspace | undefined {
  return workspaces.find((w) => w.id === sel.workspaceId);
}

// Mirrors internal/api/runs_create.go's applyWriteNarrowing CLIENT-SIDE, so the
// mount buildSpec composes is ALREADY the true resolved value — Review's "exact
// policy" JSON must show what will actually be enforced, not a placeholder the
// server silently rewrites later. Required (or an enabled Optional) grants
// write by default; sel.readOnly may only narrow that to read-only, never widen
// a not-granted default to writable (matches the Go doc comment exactly).
// EXPORTED so every caller that resolves a local_dir mount's write access
// shares this ONE reader instead of re-deriving it — lib/api/compose.ts's
// resolveComposeWorkspace uses it for the AI Run Composer's `workspaces[]`
// resolution, the same way buildSpec below uses it for the manual wizard's
// workspace_mounts[]. Two derivations that can disagree is exactly the defect
// class this function exists to close.
//
// `path` is the SOURCE's own locator — the write:<locator> requirement key is
// scoped to ONE source (source_scan.go:134's seed["write:"+locator]), never
// the whole workspace. Defaults to ws.source (the single-source mirror) so
// every existing 2-arg call site keeps its old whole-workspace behavior
// unchanged; a multi-source caller (resolveWorkspaceMounts below,
// resolveComposeWorkspace) passes each source's own path explicitly — a
// multi-source workspace can carry several write:<path> rows with DIFFERENT
// levels, which a single aggregated "does ANY of them grant write" flag would
// blur across sources (PARITY-2).
export function resolvedMountReadOnly(
  ws: Workspace,
  sel: RunWorkspaceSelection,
  path: string = ws.source,
): boolean {
  const req = workspaceRequirements(ws)[`write:${path}`];
  const grantedDefault = req?.level === "required" || (sel.enabledOptional ?? []).includes(`write:${path}`);
  if (!grantedDefault) return true;
  return sel.readOnly === true;
}

// The workspace's REAL source list a mount/repo resolution should iterate —
// ws.sources[] when the record carries it (every server-fetched Workspace
// does — PARITY-2), else a SYNTHETIC single-entry list built from the legacy
// kind/source mirror, so a hand-built fixture (or a stale cached record) that
// never populated .sources resolves EXACTLY as the old single-mirror code
// did. Exported so lib/api/compose.ts's resolveComposeWorkspace shares the
// SAME fallback resolveWorkspaceMounts uses below — two derivations of "what
// are this workspace's sources" that could disagree is the exact defect class
// this function exists to close.
export function resolvableSources(ws: Workspace): WorkspaceSourceInput[] {
  if (ws.sources?.length) return ws.sources;
  if (ws.kind === "local_dir") {
    return [{ type: "local_dir", path: ws.source, target: ws.default_target }];
  }
  if (ws.kind === "repo") {
    return [{ type: "repo", source: ws.source, target: ws.default_target }];
  }
  // "ephemeral", or an unrecognized/empty kind (a genuinely multi-source
  // record whose sources[] wasn't fetched) — no host path to mount, no repo
  // to clone; callers correctly emit nothing for it rather than a garbage
  // empty-string mount source (the exact PARITY-2 bug).
  return [{ type: "ephemeral", target: ws.default_target }];
}

// Resolve ONE workspace selection into its workspace_mounts[]/workspace_repos[]
// entries — one WorkspaceMount per local_dir source, one WorkspaceRepo per repo
// source, nothing for ephemeral (PARITY-2: the old code flattened to the
// workspace's single-mirror kind/source, which is EMPTY for a multi-source or
// migrated-ephemeral workspace, so it silently mounted nothing at all).
// Exported so StepReview's own preview (if it ever needs one) and buildSpec
// below share the identical per-source resolution.
export function resolveWorkspaceMounts(
  w: Workspace,
  sel: RunWorkspaceSelection,
): { mounts: WorkspaceMount[]; repos: WorkspaceRepo[] } {
  const mounts: WorkspaceMount[] = [];
  const repos: WorkspaceRepo[] = [];
  resolvableSources(w).forEach((src, i) => {
    // The picker's single "Target override" field (workspace-picker.tsx) only
    // ever meant one mount point per workspace — apply it to the FIRST source
    // only; every other source keeps its own onboarded target.
    const override = i === 0 ? sel.target?.trim() : undefined;
    if (src.type === "repo" && src.source) {
      const target = override || src.target?.trim();
      const entry: WorkspaceRepo = { repo: src.source };
      if (target) entry.target = target;
      repos.push(entry);
    } else if (src.type === "local_dir" && src.path) {
      mounts.push({
        source: src.path,
        // Mount at the agent's working dir (~/work = /home/agent/work) by
        // convention — that's where `claude` and the `wardyn attach` shell
        // start. A source's own target, or a per-run override, takes
        // precedence.
        target: override || src.target?.trim() || "/home/agent/work",
        // The workspace's requirements contract decides write access now (a
        // Required write, or an enabled Optional one) — not a bare per-run
        // flag. See resolvedMountReadOnly.
        read_only: resolvedMountReadOnly(w, sel, src.path),
      });
    }
    // ephemeral: no host path to mount and no repo to clone — the server
    // mkdirs the scratch target itself; nothing for the policy to carry.
  });
  return { mounts, repos };
}

// primaryWorkspaceId mirrors the server's own primary pick (referencedWorkspaces,
// internal/api/workspace_run.go): it walks the RESOLVED spec's workspace_mounts
// in FULL before workspace_repos, so the primary is whichever SELECTED
// workspace contributes the FIRST local_dir mount — never simply
// selections[0] (PARITY-3). A workspace's OWN composition decides eligibility
// (resolvableSources, so a multi-source workspace with a local_dir source
// anywhere in it still counts — PARITY-2), never the flattened single-mirror
// kind. Used everywhere a "primary workspace" drives a decision (the
// model-access binding, Review's summary) so the console can't name a
// different workspace's credential than the run actually inherits.
export function primaryWorkspaceId(
  selections: RunWorkspaceSelection[],
  workspaces: Workspace[],
): string | undefined {
  const resolved = selections
    .map((sel) => ({ id: sel.workspaceId, w: resolveWorkspace(sel, workspaces) }))
    .filter((x): x is { id: string; w: Workspace } => !!x.w);
  const local = resolved.find(({ w }) => resolvableSources(w).some((s) => s.type === "local_dir"));
  if (local) return local.id;
  const repo = resolved.find(({ w }) => resolvableSources(w).some((s) => s.type === "repo"));
  return repo?.id;
}

// One entry of CreateRunRequest.Workspaces (pkg/client/client.go's
// WorkspaceSelection) — snake_case wire shape, distinct from this module's own
// camelCase RunWorkspaceSelection (the wizard's per-attachment STATE).
export interface RunWorkspaceSelectionWire {
  workspace_id: string;
  enabled_optional?: string[];
  read_only?: boolean;
}

// toRunWorkspacesWire converts per-workspace selections to the wire shape
// above, emitting an entry ONLY for a selection that opts into something —
// an all-defaults selection is a no-op the server doesn't need to see (a
// Required entry never needs one; it applies automatically). Shared by
// buildSpec (the manual wizard) and the AI Run Composer's approveLaunch
// (new-run-dialog.tsx) so the two paths can't drift on what counts as
// "non-default".
export function toRunWorkspacesWire(selections: RunWorkspaceSelection[]): RunWorkspaceSelectionWire[] {
  const out: RunWorkspaceSelectionWire[] = [];
  for (const sel of selections) {
    if (!sel.enabledOptional?.length && sel.readOnly === undefined) continue;
    const entry: RunWorkspaceSelectionWire = { workspace_id: sel.workspaceId };
    if (sel.enabledOptional?.length) entry.enabled_optional = sel.enabledOptional;
    if (sel.readOnly !== undefined) entry.read_only = sel.readOnly;
    out.push(entry);
  }
  return out;
}

// CreateRunInput (lib/types/runs.ts) doesn't carry `workspaces`/`integration_id`
// yet — same stopgap as the WorkspaceRequirementsMap import above: extend
// locally rather than widen the shared type out from under whoever else is
// mid-edit on it. Mirrors pkg/client/client.go's CreateRunRequest.Workspaces /
// IntegrationID 1:1.
export type CreateRunInputWithComposition = CreateRunInput & {
  workspaces?: RunWorkspaceSelectionWire[];
  integration_id?: string;
  // The PRIMARY workspace's id, sent ONLY when the selection resolves to no
  // mount/repo (a pure-ephemeral / migrated-0029 container workspace). Such a
  // workspace has no source the server's referencedWorkspaces can match, so its
  // base_image would be silently dropped; workspace_id routes it through
  // seedRequestWorkspace, which seeds the scratch target and its base_image.
  // Safe from double-mounting precisely because there is no mount/repo to
  // duplicate — never set when buildSpec already emitted workspace_mounts/repos.
  workspace_id?: string;
};

// Why buildSpec unions a host into allowed_domains without the operator ever
// toggling it on the Egress step (D6/claim3).
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
  const interactive = state.mode === "interactive";

  // --- run scalars ---
  const run: CreateRunInputWithComposition = {
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

  // The LLM api_key grant — carried forward for a hydrated recording/policy
  // that already names one (see wizardStateFromProposal); there is no longer a
  // manual picker for it in step-access.tsx (model access resolves from
  // integrations instead).
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

// The agent's human display label — shared by every surface that names it in
// prose (RD.NONE_LINE, step-access.tsx's OverridePeek) so "Codex CLI" can
// never come out as the hardcoded "Claude Code" default.
export function agentLabel(agent: WizardAgent): string {
  return agent === "codex-cli" ? "Codex CLI" : "Claude Code";
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
//
// `echoedSelections` is the compose proposal's OWN workspace_selections echo
// (ComposeResponse.proposed.workspace_selections — the wire shape
// RunWorkspaceSelectionWire) carrying whatever enabled_optional/read_only the
// operator picked on the AI path's WorkspacePicker. Without it, "Edit in
// wizard" silently dropped every Optional opt-in the operator just made — the
// matched selection only ever carried the inferred workMount.read_only, never
// enabledOptional at all. Absent/empty (an older server, or no match) degrades
// to that same inferred-only behavior, never a crash.
export function wizardStateFromProposal(
  run: ComposeRunProposal,
  spec: RunPolicySpec,
  workspaces: Workspace[] = [],
  echoedSelections: RunWorkspaceSelectionWire[] = [],
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
  // The echoed entry for the matched workspace, if the proposal named one —
  // its enabled_optional/read_only win over the workMount-inferred read-only
  // (the echo IS what produced that mount in the first place; workMount stays
  // the fallback for an older server that predates the echo).
  const echoed = matched ? echoedSelections.find((s) => s.workspace_id === matched.id) : undefined;
  const workspaceSelections: RunWorkspaceSelection[] = matched
    ? [
        {
          workspaceId: matched.id,
          readOnly: echoed?.read_only ?? (workMount ? !!workMount.read_only : undefined),
          enabledOptional: echoed?.enabled_optional,
        },
      ]
    : [];

  const githubGrant = (spec.eligible_grants ?? []).find((g) => g.kind === "github_token");
  const apiKeyGrant = (spec.eligible_grants ?? []).find((g) => g.kind === "api_key");
  const apiKeySecret = (apiKeyGrant?.scope?.secret_name as string) ?? "";

  // A recorded profile's api_key grant can name the subscription OAuth sentinel
  // instead of a real stored secret (recordings never synthesize a resident
  // mount for it) — recognizing it here is what stops it from being carried
  // into llmSecretName and re-emitted as a broken x-api-key grant to a secret
  // that doesn't exist (the "references unknown secret" launch failure). The
  // resident ~/.claude PATH itself is no longer reconstructed (RETIRED —
  // model access resolves from integrations, not a per-run subscription dir);
  // a stored/recorded spec's OWN mount there is otherwise carried as an
  // ordinary, unrecognized workspace_mounts entry — dropped on hydration, same
  // as any other mount this best-effort inverse doesn't specifically recognize.
  const recordedSubscription = apiKeySecret === SUBSCRIPTION_OAUTH_SECRET;
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
// (runType, agent, mode, task, workspace, image). Sets selectedProfile so the footer
// can fast-track.
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
    // UI-RUN-3: runType and image are Basics choices this function's own
    // contract promises to keep — dropping them (they were missing here)
    // silently converted a governed command into an agent run and discarded
    // a BYOI image the instant a saved policy/recorded profile was picked.
    runType: state.runType,
    agent: state.agent,
    mode: state.mode,
    task: state.task,
    workspaces: state.workspaces,
    image: state.image,
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
        return "An autonomous run needs a task to perform.";
      return null;
    }
    case "access": {
      if (state.githubEnabled && !parseRepoList(state.githubRepos).length)
        return "Add at least one repo for the GitHub token, or disable it.";
      if (state.githubEnabled && state.githubTtlMinutes <= 0)
        return "GitHub token TTL must be a positive number of minutes.";
      if (state.gitPatEnabled && !gitPatConfigured(state))
        return "Git PAT needs both a host and a stored secret.";
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
