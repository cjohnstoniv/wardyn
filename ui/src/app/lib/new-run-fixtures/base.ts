/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Fixtures for the 0.9 New Run packet's routes (docs: lanes D-UI-a, M-NR-1/2/3):
// the dry-run answers and form state each frame is drawn from, so the UI lanes
// (A-L6…A-L14) test against the same data. Synthetic names only. A fixture is
// data, never a rendering: it holds the wire the server answers with and the
// draft the form holds, not what a panel shows.
import type { ActiveSections, RunContractDraft } from "../run-contract-draft";
import type { ComponentFact } from "../types/components";
import type {
  AgentFact,
  AllowedImage,
  PlacementResources,
  PlacementRunner,
} from "../types/new-run-contract";
import type { PolicyPreviewResult, ProvenanceEntry } from "../types/policy-preview";
import type { RunPolicySpec } from "../types/policy";
import type { PreflightResult } from "../types/runs";

/** The same provenance rows decoded from the policy-preview and preflight wire. */
export type FixtureProvenance = ProvenanceEntry;

/** One workspace as a Workspaces-panel frame holds it. */
export interface FixtureWorkspace {
  id: string;
  name: string;
  /** The target path field's value; absent keeps the workspace's own. */
  target?: string;
  /** Other sources of the workspace, each at a fixed path of its own. */
  otherSourceTargets?: string[];
  readOnly?: boolean;
  /** The model provider the workspace pins. */
  pin?: string;
  baseImage?: string;
  /** The Azure DevOps organisation its repositories are in. */
  adoOrg?: string;
  /** Optional requirement keys it declares, and the ones switched on. */
  optional?: string[];
  enabledOptional?: string[];
  /** A workspace that grants no writes (read-only is a sentence, not a checkbox). */
  noWrites?: boolean;
  scratch?: boolean;
  /** A local_dir source registered for another runner. */
  localDirForRunner?: string;
}

/** A server refusal a frame shows, by reason and the sentence the server sent. */
export interface FixtureRefusal {
  status: number;
  reason: string;
  text: string;
}

/** The tabs, in order (DECISIONS "New Run tabs restructured"). */
export type FixtureTab = "info" | "workspaces" | "runner" | "tools_image" | "repositories_drives" | "access" | "policy";

export interface NewRunFixture {
  /** The tab the frame belongs to. The packet's `run/…` routes are the Runner tab (Info is Title and Description only). */
  tab: FixtureTab;
  /** The packet's route, relative to /m-nr/, e.g. "run/p1-no-runner". */
  route: string;
  note: string;
  /** False when the deployment has runners turned off. Default true. */
  runnersEnabled?: boolean;
  runners?: PlacementRunner[];
  /** A ceiling term that denies placing runs on a runner (P3); the profile names it. */
  ceilingDeniesLocal?: { profile?: string };
  preview: PolicyPreviewResult;
  preflight?: PreflightResult;
  provenance?: FixtureProvenance[];
  /** The form's draft of the contract fields, and which sections are on the page. */
  contract?: RunContractDraft;
  active?: ActiveSections;
  workspaces?: FixtureWorkspace[];
  drive?: boolean;
  refusal?: FixtureRefusal;
}

/** A fixture as its file writes it: the tab is the file's. */
export type FixtureBody = Omit<NewRunFixture, "tab">;

export const inTab = (tab: FixtureTab, list: readonly FixtureBody[]): NewRunFixture[] => list.map((f) => ({ ...f, tab }));

export const emptySpec = (over: Partial<RunPolicySpec> = {}): RunPolicySpec => ({
  allowed_domains: ["api.anthropic.com"],
  first_use_approval: "deny_with_review",
  min_confinement_class: "CC2",
  ...over,
});

export function preview(over: Partial<PolicyPreviewResult> = {}): PolicyPreviewResult {
  return {
    spec: emptySpec(),
    source: { kind: "default" },
    provisional: true,
    redacted: false,
    warnings: [],
    pending: ["credential_liveness", "autonomy", "tool_approvals", "runner_confinement", "dispatch_egress"],
    repository_access: [],
    resources: [],
    local_placement: [],
    image: { ref: "ghcr.io/wardyn/agent-claude-code:0.9", source: { kind: "agent" } },
    allowed_images: [],
    ...over,
  };
}

/** The images the organisation allows a person to choose. */
export const ALLOWED_IMAGES: AllowedImage[] = [
  { ref: "ghcr.io/acme/dev:1", name: "Acme dev image" },
  { ref: "ghcr.io/acme/dev-gpu:1", name: "Acme GPU image" },
];

export function preflight(over: Partial<PreflightResult> = {}): PreflightResult {
  return { setup_items: [], enforced_confinement_class: "CC2", resources: [], local_placement: [], allowed_images: [], ...over };
}

export const RUNNER_ONLINE: PlacementRunner = { id: "11111111-1111-4111-8111-111111111111", name: "ada-laptop", state: "online" };
export const RUNNER_SECOND: PlacementRunner = { id: "22222222-2222-4222-8222-222222222222", name: "ada-desktop", state: "online" };
export const RUNNER_OFFLINE: PlacementRunner = { id: "33333333-3333-4333-8333-333333333333", name: "ada-laptop", state: "offline", last_seen: "2026-10-09T08:12:00Z" };
export const RUNNER_UNCLAIMED: PlacementRunner = { id: "44444444-4444-4444-8444-444444444444", name: "ada-laptop", state: "unclaimed" };

export const RESOURCES_ORG: PlacementResources = {
  placement: "remote",
  defaults: { cpu_millis: 2000, memory_mib: 4096 },
  caps: { cpu_millis: 8000, memory_mib: 16384 },
};
export const resourcesFor = (runner: PlacementRunner, caps = { cpu_millis: 4000, memory_mib: 8192 }): PlacementResources => ({
  placement: "local",
  runner_id: runner.id,
  defaults: { cpu_millis: 2000, memory_mib: 4096 },
  caps,
});

export const agentFact = (over: Partial<AgentFact> = {}): AgentFact => ({
  agent: "claude-code",
  model_provider: { name: "Anthropic API", kind: "anthropic_api_key" },
  hosts: [
    { host: "api.anthropic.com", role: "provider" },
    { host: "console.anthropic.com", role: "login" },
  ],
  secrets: [{ kind: "key", owner: "own", residency: "proxy" }],
  managed_settings: { path: "/etc/claude-code/managed-settings.json", document: '{\n  "permissions": {}\n}\n' },
  telemetry: { off: true, env: ["DISABLE_TELEMETRY", "DISABLE_ERROR_REPORTING"] },
  ...over,
});

export const agentComponent = (over: Partial<ComponentFact> = {}, agent: Partial<AgentFact> = {}): ComponentFact => ({
  kind: "agent",
  id: "agent:claude-code",
  reason: "workspace",
  status: "ready",
  requirements: [],
  agent: agentFact(agent),
  ...over,
});

export const githubComponent = (over: Partial<ComponentFact> = {}): ComponentFact => ({
  kind: "git_provider",
  provider: "github",
  id: "git_provider:github:app",
  reason: "workspace",
  status: "unknown",
  requirements: [],
  lane: "app",
  org: "acme",
  repos: ["https://github.com/acme/payments"],
  repo_access: [{ repo: "acme/payments", access: "write", can_write: true }],
  ...over,
});

export const adoComponent = (over: Partial<ComponentFact> = {}): ComponentFact => ({
  kind: "git_provider",
  provider: "azure_devops",
  id: "git_provider:azure_devops:entra:globex",
  reason: "workspace",
  status: "ready",
  requirements: [],
  lane: "entra",
  org: "globex",
  repos: ["https://dev.azure.com/globex/core/_git/ledger"],
  capabilities: ["code_read", "code_write"],
  capability_ceiling: ["code_read", "code_write", "pr_write"],
  token_mode: "minted_pat",
  ...over,
});

export const customComponent = (name: string, over: Partial<ComponentFact> = {}): ComponentFact => ({
  kind: "custom",
  id: `inline:0`,
  name,
  version: 1,
  reason: "inline",
  status: "ready",
  requirements: [],
  hosts: ["api.example.com"],
  secrets: [{ delivery: "header", shared: false }],
  ...over,
});

export const sources = (...pairs: [string, FixtureProvenance["source"]["kind"], string?][]): FixtureProvenance[] =>
  pairs.map(([value, kind, name]) => ({ field: "allowed_domains", value, source: { kind, ...(name ? { name } : {}) }, effect: "added" as const }));

/** The form's contract draft with nothing chosen. */
export const noContract = (): RunContractDraft => ({ runner: {}, access: { overrides: { gitPAT: [], pushRules: [] } }, mode: { tools: [] } });

/** A component the person defined (a run on a runner needs the ceiling term that allows it). */
export const selfDefined = (name: string, over: Partial<ComponentFact> = {}): ComponentFact =>
  customComponent(name, { reason: "self", self_defined: true, ...over });
