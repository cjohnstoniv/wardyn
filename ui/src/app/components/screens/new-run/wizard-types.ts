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
  // The run's NAME. Required by the New Run screen (the server is deliberately
  // tolerant — see CreateRunRequest.Title): runs sharing a title are grouped on
  // the Runs board, so this is the one field that makes a run findable later.
  title: string;
  // Optional free-text note: why this run exists. Shown on run detail.
  description: string;
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
  // The agent's PROMPT for a batch run, or the shell command for a "command"
  // run. An INTERACTIVE run has no task at all — the server ignores it for one,
  // so the screen hides the field and buildSpec sends "" (see interactiveStart,
  // which is what an interactive run configures instead).
  task: string;
  // What an INTERACTIVE run's attach shell opens with: the image's agent CLI in
  // the prepared workspace, or a bare terminal there. Ignored for every other
  // run mode. Defaults to "agent": you picked "Agent task" and named an agent.
  interactiveStart: "shell" | "agent";
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
  // A composed proposal's devcontainer build (composer.RunInput.
  // DevcontainerRepo — mutually exclusive with `image`, operator-only, same as
  // it). Wizard-editable, never — it is CARRIED from "Edit in wizard" so the
  // wizard's own Launch builds the SAME sandbox "Approve & launch" would have
  // (see wizardStateFromProposal / buildSpec); "" = no devcontainer build.
  devcontainerRepo: string;

  // W15-W15e-wizard-roundtrip-3: grant kinds this wizard has no editable UI
  // for at all — ssh_key (a resident private key for git's SSH transport) and
  // cloud_sts — carried verbatim from a hydrated spec's eligible_grants so
  // buildSpec can re-emit them unchanged instead of silently dropping them
  // (the git_pat kind above got its own editable Access fields for the same
  // reason). Never wizard-editable; the wizard just passes these through.
  opaqueGrants: GrantSpec[];
}

export function initialWizardState(defaultCc: ConfinementClass = "CC1"): WizardState {
  return {
    title: "",
    description: "",
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
    // You chose "Agent task" and named an agent; attaching should hand you that
    // agent, not a prompt you then have to type its name at. Opt out for a
    // plain terminal in the same prepared workspace.
    interactiveStart: "agent",

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
    // W12-W12-B-4: approval-gated by default used to sound safer, but the
    // broker's mint is single-use per grant (internal/broker/broker.go's
    // minted_jti guard) regardless of RequiresApproval — an approval-gated
    // git_pat authenticates exactly ONE git operation, then every later one
    // in the SAME run (a second push, a submodule fetch, …) 409s "mint
    // returned without approval_id" with no way to re-approve mid-run. The
    // cached github_token lane doesn't hit this (its brokered mint is
    // refreshed per use, not a raw single-use secret grant), so default off
    // to match its effective behavior; the operator can still opt back in.
    gitPatRequiresApproval: false,

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
    devcontainerRepo: "",
    opaqueGrants: [],
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
// EXPORTED: shared with wizard-spec.ts's buildSpec (github_token grant scope)
// and buildSpec's grant emission.
export function parseRepoList(raw: string): string[] {
  return raw
    .split(/[\s,]+/)
    .map((r) => r.trim())
    .filter(Boolean);
}

// The ONE predicate for "is there a real git_pat grant" — shared by buildSpec's
// grant emission and its requiredHosts union, so a
// half-configured PAT (host with no secret, or vice versa) can never widen
// egress for a grant that was never minted (D5/claim4). EXPORTED: buildSpec
// and impliedEgressHosts live in wizard-spec.ts now.
export function gitPatConfigured(state: WizardState): boolean {
  return state.gitPatEnabled && !!state.gitPatHost.trim() && !!state.gitPatSecretName.trim();
}

// Resolve one WorkspaceSelection against the fetched onboarded-workspace list
// into its onboarded kind/source/name. Returns undefined for a stale selection
// (the workspace was deleted after it was picked) — buildSpec defensively skips
// those rather than emitting a dangling reference. EXPORTED: buildSpec and
// impliedEgressHosts (wizard-spec.ts) resolve selections the same way
// primaryWorkspaceId below does.
export function resolveWorkspace(sel: WorkspaceSelection, workspaces: Workspace[]): Workspace | undefined {
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
  // A source the operator explicitly ticked "Allow writes to this directory"
  // on grants write the same way a required write: row does. Without this the
  // checkbox in AddWorkspaceDialog is a NO-OP for anything launched from the
  // UI: it stores sources[].writable=true, nothing ever creates a write: row,
  // so every mount resolved here came out read-only and an agent's edits could
  // not reach the host. internal/api/workspace_run.go already does exactly
  // this (`ro := !src.Writable`) — this is the client mirror catching up, and
  // it widens nothing that a human did not tick.
  const src = resolvableSources(ws).find((s) => (s.path ?? s.source) === path);
  const grantedDefault =
    req?.level === "required" ||
    (sel.enabledOptional ?? []).includes(`write:${path}`) ||
    src?.writable === true;
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
  // A composed proposal's devcontainer build (pkg/client.CreateRunRequest.
  // DevcontainerRepo — mutually exclusive with `image`). Carried from
  // WizardState.devcontainerRepo so "Launch" from the wizard builds the same
  // sandbox "Approve & launch" would have for the same proposal.
  devcontainer_repo?: string;
  // The PRIMARY workspace's id, sent ONLY when the selection resolves to no
  // mount/repo (a pure-ephemeral / migrated-0029 container workspace). Such a
  // workspace has no source the server's referencedWorkspaces can match, so its
  // base_image would be silently dropped; workspace_id routes it through
  // seedRequestWorkspace, which seeds the scratch target and its base_image.
  // Safe from double-mounting precisely because there is no mount/repo to
  // duplicate — never set when buildSpec already emitted workspace_mounts/repos.
  workspace_id?: string;
};

// buildSpec (the state -> canonical wire-contract composer) and
// impliedEgressHosts (the grant-implied-egress-host list it shares with
// step-egress.tsx, D6/claim3) moved to ./wizard-spec.ts — re-exported here so
// every existing importer (wizard.tsx, step-review.tsx, the test suite, …)
// keeps working unchanged.
export { buildSpec, impliedEgressHosts } from "./wizard-spec";
export type { ImpliedEgressHost, ImpliedEgressWhy } from "./wizard-spec";

// The agent's human display label — shared by every surface that names it in
// prose (RD.NONE_LINE, step-access.tsx's OverridePeek) so "Codex CLI" can
// never come out as the hardcoded "Claude Code" default.
export function agentLabel(agent: WizardAgent): string {
  return agent === "codex-cli" ? "Codex CLI" : "Claude Code";
}

// EXPORTED: wizard-spec.ts's buildSpec and wizardStateFromProposal share this
// ONE dedupe.
export function dedupe(xs: string[]): string[] {
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
  // W15-W15e-wizard-roundtrip-3: this used to hydrate ONLY github_token/
  // api_key — a recorded/composed spec's git_pat grant silently vanished on
  // "Edit in wizard" / fast-track while spec.allowed_domains (below) still
  // carried its host into allowedDomains, so the destination stayed allowed
  // with no credential left to authenticate to it. git_pat has a full,
  // editable home in WizardState (Access's Git-PAT card), so hydrate it.
  const gitPatGrant = (spec.eligible_grants ?? []).find((g) => g.kind === "git_pat");
  const gitPatScope = (gitPatGrant?.scope ?? {}) as { host?: string; secret_name?: string; username?: string };
  // W15-W15e-wizard-roundtrip-3 (part 2, per the finding's own callout):
  // ssh_key/cloud_sts grants have no editable home in this wizard at all (no
  // fields anywhere represent them) — they used to silently drop here, same
  // as any workspace_mounts entry this best-effort inverse doesn't recognize
  // (see the recordedSubscription comment below), while spec.allowed_domains
  // still carried their host, the identical shape of bug git_pat had. Keep
  // every OTHER grant kind verbatim in a pass-through bucket buildSpec
  // re-emits unchanged (WizardState.opaqueGrants) instead of refusing the
  // fast-track — this wizard cannot EDIT these, but re-emitting the grant the
  // recording/proposal already had is not an edit.
  const KNOWN_GRANT_KINDS = new Set(["github_token", "api_key", "git_pat"]);
  const opaqueGrants = (spec.eligible_grants ?? []).filter((g) => !KNOWN_GRANT_KINDS.has(g.kind));

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
  // W15-W15e-wizard-roundtrip-4: this wizard's github_token permission is a
  // two-state read/read+write toggle (githubPermissionsMap re-emits
  // "read+write" as BOTH contents:write AND pull_requests:write together).
  // Collapsing to "read+write" off contents:write ALONE — the prior check —
  // WIDENS an asymmetric source scope (contents:write with no
  // pull_requests:write, e.g. a recording that never opened a PR) into one
  // that re-emits pull_requests:write it never had, contradicting Record
  // Mode's "reuse can only ever subset" claim. Require BOTH, matching
  // exactly what "read+write" re-emits; an asymmetric scope this two-state
  // toggle can't represent falls back to "read" (narrower, never wider).
  const ghPerm: GitHubPermission =
    ghScope.permissions?.contents === "write" && ghScope.permissions?.pull_requests === "write"
      ? "read+write"
      : "read";

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

    gitPatEnabled: !!gitPatGrant,
    gitPatHost: gitPatScope.host ?? "",
    gitPatSecretName: gitPatScope.secret_name ?? "",
    gitPatUsername: gitPatScope.username ?? "",
    gitPatRequiresApproval: gitPatGrant?.requires_approval ?? base.gitPatRequiresApproval,
    // The api_key grant references a stored secret by name; carry it forward so
    // the wizard re-emits the same grant — EXCEPT the subscription sentinel, which
    // is not a real stored secret (it means "subscription auth", handled above).
    llmSecretName: recordedSubscription ? "" : apiKeySecret,

    // W15-W15e-wizard-roundtrip-7: allowed_domains is a REQUIRED field
    // (RunPolicySpec.allowed_domains string[]) — `[]` is always a real,
    // meaningful value, not "unset". Record Mode's own synthesized profile
    // (internal/recordmode/recordmode.go's Synthesize) writes exactly `[]`
    // — or, for a genuinely deny-all outcome ("no allowed egress observed"),
    // a NIL slice (`var allowed []string`, never appended to) — for that
    // deny-all case. types.go's AllowedDomains has no `omitempty`, so a nil
    // slice still serializes to wire `null`, not `[]` (Go's ordinary
    // encoding/json behavior); TS's `string[]` type says "always an array"
    // but does not guard the runtime JSON boundary. dedupe() itself would
    // THROW (Array.prototype.map on null) on that null, crashing the
    // recorded-profile fast-track (applyProfileSpecToState skips straight to
    // Review, so the operator never sees a screen to work around it) for
    // exactly the deny-all case this file most wants to get right.
    // compose-quick-review.tsx:114's `p.allowed_domains ?? []` already
    // guards the identical field for the same reason. `?? []` treats that
    // wire null the same as an explicit empty array — both mean deny-all.
    allowedDomains: dedupe(spec.allowed_domains ?? []),
    deniedDomains: dedupe(spec.denied_domains ?? []),
    firstUseApproval: asFirstUseMode(spec.first_use_approval),
    allowAllEgress: spec.allow_all_egress === true,

    confinementClass: cc,
    // auto_stop_after_sec is `int json:"...,omitempty"` server-side (internal/
    // types/types.go) — absent and an explicit 0 are indistinguishable on the
    // wire, and 0 is never a meaningful idle-timeout choice (the UI's own
    // min=1 already disallows it) — unlike allowed_domains above, there is no
    // real "unset vs deliberately empty" distinction to lose here, so
    // defaulting to this wizard's normal fresh-entry default (base, same as
    // initialWizardState) when absent is not a misrepresentation.
    lifecycle: spec.auto_stop_after_sec === -1 ? "never" : "auto",
    autoStopMinutes:
      spec.auto_stop_after_sec != null && spec.auto_stop_after_sec > 0
        ? Math.max(1, Math.round(spec.auto_stop_after_sec / 60))
        : base.autoStopMinutes,

    // W15-W15e-wizard-roundtrip-6: without this, "Edit in wizard" silently
    // dropped a composed devcontainer_repo — the wizard's own Launch then
    // built the plain convention image, a DIFFERENT sandbox than "Approve &
    // launch" (which sends result.proposed.run — devcontainer_repo intact —
    // unchanged) would have built for the identical proposal.
    devcontainerRepo: run.devcontainer_repo ?? "",
    opaqueGrants,
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

// RETIRED: validateStep(WizardStepId, WizardState) + WIZARD_STEPS/WizardStepId
// were the five-step wizard's per-step gate. That wizard was replaced by the
// single-page new-run-screen.tsx, which never called them — so they had no
// non-test caller at all, and they had started to describe a form that no
// longer exists (no title, and a task field on interactive runs). Two answers
// to "is this run valid?", one of them wrong and unreachable, is how the wrong
// one gets wired up later. The live answer is new-run-screen.tsx's `problem`
// memo; the git_pat rule that lived here is enforced where it matters, by
// gitPatConfigured() inside buildSpec (wizard-spec.ts).
