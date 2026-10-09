/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

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
import type {
  AuditEvent,
  ConfinementClass,
  CreateRunInput,
  FirstUseMode,
  SetupModelProvider,
  WorkspaceSelection,
} from "../../../lib/types";
import type { ComponentRef } from "../../../lib/types/components";
import { templateProviders } from "../../wardyn/policy-template-providers";
// review U-01: the ONE place both clone doors (run header, Runs-list kebab)
// turn a run's audit trail into a prefill or a refusal — see cloneFromAudit
// below. lib/api must not import from components/ (audit.ts's own comment),
// so the dependency runs the allowed direction: this component-layer module
// imports the lib helper, not the reverse.
import { createRequestFromAudit } from "../../../lib/api/audit";

export type { WorkspaceSelection };

  import type { RunWorkspaceSelection, RunWorkspaceSelectionWire } from "./wizard-workspaces";
export {
  compositionSummary, hasSourceNotAdmitted, primaryWorkspaceId, resolvableSources,
  resolvedModelProviders, resolvedMountReadOnly, resolveWorkspace, resolveWorkspaceMounts,
  secretAutoGrants, toRunWorkspacesWire, UNUSABLE_PIN, workspaceModelProviderUnavailable,
  workspacePin, workspaceRequirements, workspaceUnavailableToCaller,
} from "./wizard-workspaces";
export type { RunWorkspaceSelection, RunWorkspaceSelectionWire } from "./wizard-workspaces";

// Only TWO agents are valid on the wire — fix the old claude_code/codex/cursor
// bug by constraining the picker to exactly these dotted ids.
// SEAM: there is no harness/tool-catalog endpoint exposed to the UI yet (no GET
// listing installable agent CLIs) — if one ships, read the picker's options from
// it instead of hand-adding a third literal here.
//
// A RUNTIME list with the type derived from it, not the other way round: the
// clone path (runPrefill) has to ask "can this build spell that agent?" at
// runtime, and a TS union cannot be asked. Widening the roster is then one
// edit here — a second literal list somewhere else is how a new agent ships
// pickable but unclonable.
// "none" (BYOA — the harness catalog's own id, harness.go) widened in here per
// the comment above: a third harness (Your own tools) is now offered by
// SetupStatus.harnesses (C-UI, W4), so it joins the roster this build can spell
// — the same list, not a second one.
export const WIZARD_AGENTS = ["claude-code", "codex-cli", "none"] as const;
export type WizardAgent = (typeof WIZARD_AGENTS)[number];

/** Whether a wire agent id is one this build can put in the picker. */
export function isWizardAgent(agent: string): agent is WizardAgent {
  return (WIZARD_AGENTS as readonly string[]).includes(agent);
}

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

// RETIRED: PRESET_DOMAINS, the curated egress list the Network card's
// "Registries" preset and the Edit-hosts dialog's chip groups both spelled.
// Both surfaces are gone — /runs/new authors egress as spec JSON through the
// shared Policy panel, whose "Package registries" template carries the same set
// (examples/policies/default.json, the source the risk baseline
// internal/composer/risk.go safeBaselineDomains is itself aligned with).

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
  // The agent's PROMPT: a batch run's Task, or an agent-started interactive
  // run's OPTIONAL boot seed (the Initial prompt), fired once at sandbox boot in
  // the persistent session the human's attach later joins. Empty stays today's
  // pure-idle behavior.
  task: string;
  // A "command" run's shell command, and a terminal-started interactive run's
  // optional Startup command. Each field holds its OWN value (#1922): a prompt
  // typed as a Task can never be sent as a command line. runPromptText picks
  // the one this run shape sends.
  command: string;
  startupCommand: string;
  // What an INTERACTIVE run's attach shell opens with: the image's agent CLI in
  // the prepared workspace, or a bare terminal there. Ignored for every other
  // run mode. Defaults to "agent": you picked "Agent task" and named an agent.
  interactiveStart: "shell" | "agent";
  // Opt-in for an agent-started boot seed (interactiveStart==="agent" with a
  // non-empty task): lets the seed use tools before a human attaches, instead
  // of parking at its first tool-approval prompt until someone joins. Default
  // false = supervised. Meaningless (and never emitted) without a seed.
  seedAutoTools: boolean;
  // Tool-approval posture for an AUTONOMOUS (batch) agent run: "auto" (default)
  // is today's behavior — the sandbox and egress policy are the only boundary;
  // "hold" routes every tool call through a Wardyn approval instead. Only
  // meaningful for runType==="agent" && mode==="batch" && agent==="claude-code"
  // (codex-cli has no external tool-approval contract) — buildSpec never emits
  // it otherwise.
  toolApprovals: "auto" | "hold";
  // Set ONLY when the operator picked a SAVED POLICY: the run then launches by
  // REFERENCE (policy_id) instead of an inline_policy, so the server enforces
  // the stored spec verbatim. Editing the panel's spec text detaches it (see
  // new-run-screen.tsx) — the authored document is not a round-trip of a stored
  // one.
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
  // #542 — this run's own model-provider pick (CreateRunRequest.ModelProvider,
  // #526): the id new-run-screen.tsx's provider picker resolved for the
  // current agent (model-provider-lane.ts's resolveProviderSelection), or
  // undefined with no provider block, no provider serving this agent, or no
  // choice made yet. Sent whenever set — see buildSpec — so the launched run
  // always chooses exactly the provider the rail showed, never whatever the
  // server's own default resolution would have picked instead.
  modelProviderId?: string;
  // The components this run carries (CreateRunRequest.Components, #1914): a
  // stored one by id or a run-only inline definition. The Access rows read what
  // the server makes of them; empty sends nothing, byte for byte as before.
  components: ComponentRef[];

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

  // --- The member's own user drive (0.7) ---
  // Mount the caller's allocated drive at /home/agent/drive for this run. OFF
  // by default and never a path: the request carries a flag, the server
  // resolves which drive belongs to the caller (internal/api/user_drives_run.go).
  // Orthogonal to `workspaces` above — a drive is not a workspace.
  driveEnabled: boolean;
  // NARROW this run's mount to read-only even though the allocation is
  // writable. Off by default (Q5). Meaningless without driveEnabled, and
  // `false` is never emitted: read_only:false cannot widen an allocation, and
  // the server refuses it as an attempt to.
  driveReadOnly: boolean;

  // --- Step 1: basics — Bring Your Own Image ---
  // A user-supplied base image ref. When set, the backend wraps it with the
  // runner tools before use (see CreateRunInput.image). "" = the convention image.
  image: string;
  // RETIRED with the wizard's hydrator: both
  // `devcontainerRepo` and `opaqueGrants` existed ONLY to carry a hydrated
  // proposal/policy through a round trip, and the hydrator that wrote them
  // (wizardStateFromProposal / applyProfileSpecToState) has no callers left.
  // Their buildSpec branches could never fire, so the "carried verbatim,
  // never silently dropped" promise was unreachable code claiming a guarantee.
  // Hand-authored grant kinds this screen has no control for now ride the
  // Policy panel's spec JSON, where mergeRunSelections preserves them by
  // construction — the JSON IS the document.
}

// B4b — what a CLONE of an existing run carries into a fresh wizard, and (the
// half that earns the type) what it knowingly could not.
//
// The prefill travels as react-router location state, so every field here must
// be structured-cloneable: plain objects, no class instances, no functions.
export type RunPrefill = {
  /** The overlay initialWizardState applies over its own defaults. */
  state: Partial<WizardState>;
  /** The source run's policy was written INLINE, and an inline policy is never
   *  persisted (internal/api/inline_policy.go attaches it with a nil id;
   *  runs.go records only `inline_policy: true`). The clone therefore cannot
   *  carry the one thing that governed the run it copies — a named ceiling the
   *  wizard states, never a silent fall back to the default policy. */
  inlinePolicy: boolean;
  /** Review F4 (#1197 L3): the Runs landing page's composer rides this SAME
   *  channel (task + an optional workspace, no policy/state overlay beyond
   *  them) rather than a second one, but it is not a clone — there is no
   *  source run, so the clone banner's two sentences (RUN.CLONE_NOTE,
   *  RUN.CLONE_CEILING_NOTE: "prefilled from THIS RUN…", "…this run had
   *  above it…") would both be false copy. Absent (the default) means a
   *  clone, unchanged for every existing caller; "composer" suppresses just
   *  that banner. */
  source?: "composer";
};

/** The request-scoped half of a run, as read back off its `run.create` audit
 *  row (lib/api/audit.ts's createRequestFromAudit). Structurally typed so this
 *  module does not depend on the api layer. */
export type RunCreateRequestFacts = {
  task_mode?: string;
  interactive_start?: string;
  seed_auto_tools?: boolean;
  tool_approvals?: string;
  inline_policy?: boolean;
};

// A minimal AgentRun view — everything the run ROW durably holds that a clone
// can use. Narrow on purpose: it names, in one place, the exact set of fields
// the row is good for, and the remainder below says what it is not.
type ClonableRun = {
  agent: string;
  task: string;
  title?: string;
  description?: string;
  policy_id?: string;
  confinement_class: ConfinementClass;
  interactive?: boolean;
  workspace_ids?: string[];
};

/**
 * Build the wizard overlay for "start a run like this one" from the TWO durable
 * sources a finished run leaves behind: the run row, and its `run.create` audit
 * event.
 *
 * Why two: the row carries no task_mode, no interactive_start, no
 * seed_auto_tools and no tool_approvals — createRunAuditData
 * (internal/api/runs.go) stamps those on the event precisely because none of
 * them is stored. A clone off the row alone would quietly downgrade a run that
 * had been launched with `tool_approvals: hold` into an unsupervised one.
 *
 * The DOCUMENTED REMAINDER — what no clone carries, and why, so the list is a
 * decision and not an omission:
 *   - the inline policy body (never persisted; see RunPrefill.inlinePolicy),
 *   - credential grants: github_token / git_pat / the LLM api_key secret. They
 *     are minted per run and per approval; re-attaching them from a record
 *     would be minting a credential nobody asked for on this launch.
 *   - approvals: every one is a fresh question about a fresh run.
 *   - egress allow/deny lists and first-use posture: they live in the policy,
 *     which a saved policy_id carries whole and an inline one cannot carry
 *     at all — re-deriving them from the run's egress DECISIONS would author a
 *     policy the operator never wrote.
 *   - the user drive, per-workspace read_only / enabled_optional overrides, the
 *     run override integration, lifecycle/auto-stop: request-scoped and NOT on
 *     the audit row either, so there is nothing durable to read.
 *   - the IMAGE. AgentRun.image is the RESOLVED sandbox image — the convention
 *     image, a devcontainer build, a workspace-built one, or a BYOI wrap — not
 *     the caller's request. WizardState.image is the opposite: a BYOI BASE, and
 *     buildSpec sends anything in it as one. Carrying it across would turn
 *     every clone into a BYOI request: refused outright where no image builder
 *     is wired (validateImageBuildRequest, runs_create_validate.go), re-wrapped
 *     where one is, and for a real BYOI run it would ask to wrap that run's own
 *     wrapper (wardyn-byoi/<old-run-id>:latest). There is no UI field showing
 *     it either, so the operator could not see what they were about to send.
 * Create re-clamps everything (resolveRunPolicy), so a member cloning an
 * admin's run is narrowed honestly at launch rather than flattered here.
 */
export function runPrefill(run: ClonableRun, created: RunCreateRequestFacts = {}): RunPrefill {
  return {
    inlinePolicy: created.inline_policy === true,
    state: {
      title: run.title ?? "",
      description: run.description ?? "",
      // task_mode is the audit row's, and it is the ONLY record that this run
      // was a plain shell command rather than an agent task.
      runType: created.task_mode === "exec" ? "command" : "agent",
      // An agent this build cannot spell (an older run, a retired id) leaves
      // the picker on its default rather than writing an unlaunchable value.
      // Asked of the roster, never of a second literal pair — WIZARD_AGENTS is
      // what widens when a third harness ships.
      ...(isWizardAgent(run.agent) ? { agent: run.agent } : {}),
      // The row holds ONE text; the audit row says which field it was typed in.
      ...(created.task_mode === "exec"
        ? { command: run.task }
        : run.interactive && created.interactive_start === "shell"
          ? { startupCommand: run.task }
          : { task: run.task }),
      mode: run.interactive ? "interactive" : "batch",
      interactiveStart: created.interactive_start === "shell" ? "shell" : "agent",
      seedAutoTools: created.seed_auto_tools === true,
      toolApprovals: created.tool_approvals === "hold" ? "hold" : "auto",
      confinementClass: run.confinement_class,
      // undefined for an inline-policy run — which is what makes the ceiling
      // sentence necessary rather than decorative.
      selectedPolicyId: run.policy_id,
      // workspace_ids is the create-time denormalization of what the run
      // resolved to. The per-selection OPTIONS are not on it (see the
      // remainder above), so each comes back as a plain attachment.
      workspaces: (run.workspace_ids ?? []).map((id) => ({ workspaceId: id })),
    },
  };
}

// Review U-01: moved here from runs/run-card.tsx
// so both clone doors (the run header's onClone, run-detail.tsx; the
// Runs-list kebab, run-card.tsx) show ONE string, not two that could drift.
export const CLONE_UNREADABLE =
  "This run's launch settings couldn't be read — its clone would start from defaults, so it was not opened.";

// U2-08 (blind round 2, lens-U2): the OTHER clone
// refusal, the one where the audit read itself failed rather than came back
// empty. It lived inline in runs/run-card.tsx, which is both the one new
// user-facing literal outside a constants block in this delta and the one
// clone-door string the two doors did not share. Beside CLONE_UNREADABLE for
// the same reason CLONE_UNREADABLE is here.
export const CLONE_LOAD_FAILED = "Could not load this run's details";

/**
 * The ONE door both clone affordances open through. `null` when the run's
 * own run.create audit row is missing — an older run, a pruned trail, or a
 * non-owner's empty 200 (auditScope writes `[]` rather than an error) — so
 * NEITHER door can silently launch a clone with wizard DEFAULTS standing in
 * for task_mode/interactive_start/seed_auto_tools/tool_approvals. The caller
 * toasts CLONE_UNREADABLE and does not navigate.
 */
export function cloneFromAudit(run: ClonableRun, events: AuditEvent[]): RunPrefill | null {
  if (!events.some((e) => e.action === "run.create")) return null;
  return runPrefill(run, createRequestFromAudit(events));
}

// The New Run title's default, while the operator hasn't typed one of their
// own (#1197 L2, design.md §3.4): the task's own first line, cut at a WORD
// BOUNDARY within 80 characters — never mid-word, and never past a line break
// the task itself chose. The server never required a title (runs_create_
// validate.go's own doc comment); this is the console's default, not a
// second validation rule.
const MAX_PREFILLED_TITLE_LEN = 80;

/** The one text this run shape sends as `task`: the Command for a shell run, the
 *  Startup command for a terminal-started interactive run, the Task/Initial
 *  prompt otherwise. Launch, the gates and the rail all read it here. */
export function runPromptText(s: Pick<WizardState, "runType" | "mode" | "interactiveStart" | "task" | "command" | "startupCommand">): string {
  if (s.runType !== "agent") return s.command;
  return s.mode === "interactive" && s.interactiveStart === "shell" ? s.startupCommand : s.task;
}

export function titleFromTask(task: string): string {
  const firstLine = task.split("\n", 1)[0];
  // F4 (#1197 L2 review): the task field tolerates tab/CR (the multiline
  // exemption, runFieldCharsAllowed's `multiline` arg) but a title does not
  // (it is validated non-multiline both here and server-side), so an
  // interior tab or other control character surviving into the prefill would
  // get the operator refused on Launch for a title they never typed. Collapse
  // every run of control characters and whitespace into one space BEFORE the
  // length/word-boundary cut, so the prefilled title always passes the same
  // check a hand-typed one does.
  const collapsed = firstLine.replace(/[\p{Cc}\s]+/gu, " ").trim();
  if (collapsed.length <= MAX_PREFILLED_TITLE_LEN) return collapsed;
  const cut = collapsed.slice(0, MAX_PREFILLED_TITLE_LEN);
  const wordBoundary = cut.lastIndexOf(" ");
  return (wordBoundary > 0 ? cut.slice(0, wordBoundary) : cut).trim();
}

/**
 * A fresh wizard, optionally overlaid with a prefill (B4b's clone). The overlay
 * is applied WHOLE over the defaults rather than merged field-by-field: every
 * key it carries is one runPrefill above decided to carry, and a partial merge
 * would be a second, disagreeing place where that decision is made.
 */
export function initialWizardState(
  defaultCc: ConfinementClass = "CC1",
  overlay?: Partial<WizardState>,
  providers?: readonly SetupModelProvider[],
): WizardState {
  return { ...freshWizardState(defaultCc, providers), ...(overlay ?? {}) };
}

// The Network card's seed: the configured model providers' hosts, else the
// no-provider default. Same derivation as the policy templates.
export function seedAllowedDomains(providers?: readonly SetupModelProvider[]): string[] {
  return [...templateProviders(providers).hosts];
}

function freshWizardState(defaultCc: ConfinementClass, providers?: readonly SetupModelProvider[]): WizardState {
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
    command: "",
    startupCommand: "",
    // You chose "Agent task" and named an agent; attaching should hand you that
    // agent, not a prompt you then have to type its name at. Opt out for a
    // plain terminal in the same prepared workspace.
    interactiveStart: "agent",
    seedAutoTools: false,
    toolApprovals: "auto",

    githubEnabled: false,
    githubRepos: "",
    githubPermission: "read",
    githubRequiresApproval: true,
    githubTtlMinutes: 60,
    llmSecretName: "",
    components: [],
    gitPatEnabled: false,
    gitPatHost: "",
    gitPatSecretName: "",
    gitPatUsername: "",
    // Approval-gated sounds safer, but the broker's mint is single-use per
    // grant (internal/broker/broker.go's minted_jti guard) regardless of
    // RequiresApproval — an approval-gated git_pat authenticates exactly ONE
    // git operation, then every later one in the SAME run (a second push, a
    // submodule fetch, …) 409s "mint returned without approval_id" with no
    // way to re-approve mid-run. The cached github_token lane doesn't hit
    // this (its brokered mint is refreshed per use, not a raw single-use
    // secret grant), so default off matches its effective behavior; the
    // operator can still opt back in.
    gitPatRequiresApproval: false,

    allowedDomains: seedAllowedDomains(providers),
    deniedDomains: [],
    firstUseApproval: "deny_with_review",
    allowAllEgress: false,

    confinementClass: defaultCc,
    lifecycle: "never",
    autoStopMinutes: 60,

    saveAsProfile: false,
    profileName: "",

    // A run mounts nothing unless the member ticks the box — the same default
    // every run has had, and the only safe one: an unasked-for mount is
    // storage nobody chose to expose to an agent.
    driveEnabled: false,
    driveReadOnly: false,

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
  // The member's user-drive request — pkg/client.DriveSelection 1:1. Absent
  // (the overwhelmingly common case) mounts nothing.
  drive?: { enabled: boolean; read_only?: boolean };
  // #542/#526 — this run's chosen model provider (pkg/client.CreateRunRequest.
  // ModelProvider 1:1). Absent under no provider block, same wire default as
  // every install before providers existed.
  model_provider?: string;
  // The components this run carries — pkg/client.CreateRunRequest.Components 1:1.
  components?: ComponentRef[];
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
  if (agent === "codex-cli") return "Codex CLI";
  // "none" (BYOA) — the harness catalog's own display name (harness.go), so a
  // custom-image run's rail/startup prose never calls it "Claude Code".
  if (agent === "none") return "Your own tools";
  return "Claude Code";
}

// EXPORTED: wizard-spec.ts's buildSpec shares this ONE dedupe.
export function dedupe(xs: string[]): string[] {
  return Array.from(new Set(xs.map((x) => x.trim()).filter(Boolean)));
}

// Per-step validation. Returns null when the step is valid, else an error string
// the wizard renders inline and uses to gate Next/Launch.
// RETIRED: validateStep(WizardStepId, WizardState) + WIZARD_STEPS/WizardStepId
// were the five-step wizard's per-step gate. That wizard was replaced by the
// single-page new-run-screen.tsx, which never called them — so they had no
// non-test caller at all, and they had started to describe a form that no
// longer exists (no title, and a task field on interactive runs). Two answers
// to "is this run valid?", one of them wrong and unreachable, is how the wrong
// one gets wired up later. The live answer is new-run-screen.tsx's `problem`
// memo; the git_pat rule that lived here is enforced where it matters, by
// gitPatConfigured() inside buildSpec (wizard-spec.ts).
