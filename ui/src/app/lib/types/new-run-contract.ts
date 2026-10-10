/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The 0.9 New Run wire contract (types only): the request fields and dry-run
// facts the console's four panels (Run, Workspaces, Access, Policy) are built
// on. Every interface mirrors one Go struct of pkg/client/runs_new_run.go and
// wire-parity.test.ts pins its json tags to that struct's source.
//
// The UI word "Runs on" is the wire field `placement`; "Organisation runners"
// is `remote` and "My runner" is `local`.

import type { PlacementReason } from "../new-run-refusals";
import type { ToolRule } from "./policy";
import type { ComponentRef } from "./components";

export type PlacementValue = "remote" | "local";

/** How an organisation-held credential reaches a local run (OD-12). */
export type LocalDeliveryMode = "own" | "via_org" | "runner_resident" | "refuse";

/** The CPU and memory a request asks for (Go: client.RunResources). A zero or absent field asks for the placement's default. */
export interface RequestedResources {
  /** Milli-CPU: 2000 is two CPUs. The console works in tenths of a CPU. */
  cpu_millis?: number;
  /** Whole MiB. */
  memory_mib?: number;
}

/** A CPU and memory pair in a dry-run answer, where zero is a value. */
export interface ResourceAmounts {
  cpu_millis: number;
  memory_mib: number;
}

/** What one placement offers: `defaults` is what an untouched run receives there, `caps` the most a request may ask for. */
export interface PlacementResources {
  placement: PlacementValue;
  /** Set when `placement` is local. */
  runner_id?: string;
  defaults: ResourceAmounts;
  caps: ResourceAmounts;
}

export interface SecretOverride {
  secret_name: string;
  host: string;
  header?: string;
  format?: string;
}

/** The edits on the agent component. Hold and deny tool rules only; a rule is never removed. */
export interface AgentOverrides {
  add_hosts?: string[];
  remove_hosts?: string[];
  add_secrets?: SecretOverride[];
  remove_secrets?: string[];
  tool_rules?: ToolRule[];
}

/** The capability set the run keeps: a subset of what the source resolved. Never empty. */
export interface ADOOverrides {
  capabilities: string[];
}

/** Narrows one git_pat grant, found by its forge host. */
export interface GitPATOverride {
  host: string;
  repos?: string[];
  access?: "read" | "write";
  api?: boolean;
}

/** Adds push content rules for one provider and organisation. Monotone: never removes one. */
export interface PushRuleOverride {
  provider: "github" | "azure_devops";
  org: string;
  deny_paths?: string[];
  require_review_paths?: string[];
}

/**
 * The person's per-component edits (OD-1). Whether an edit may be asked for is
 * decided by override-narrowing.ts, never by a panel.
 */
export interface RunOverrides {
  agent?: AgentOverrides;
  azure_devops?: ADOOverrides;
  git_pat?: GitPATOverride[];
  push_rules?: PushRuleOverride[];
}

export type LocalPlacementKind = "host" | "source" | "component";

/** Whether one entry of the run can be placed on the person's runner. An entry with no row has not been evaluated. */
export interface LocalPlacementFact {
  kind: LocalPlacementKind;
  /** The host, the workspace source's id or the component's fact id. */
  key: string;
  local_placeable: boolean;
  reason?: PlacementReason;
}

export interface TokenScopeFact {
  scope: string;
  /** The ceiling capabilities the scope covers (#1880). */
  covers: string[];
}

export interface RepoAccessFact {
  repo: string;
  access: "read" | "write";
  /** Whether the person may raise a read repository to write. */
  can_write: boolean;
}

/** The model provider the agent calls through: name and kind, never its id. */
export interface AgentModelProviderFact {
  name: string;
  kind: string;
}

export interface AgentHostFact {
  host: string;
  /** `provider` for the model provider's own lane, `login` for a sign-in host. */
  role: "provider" | "login";
}

export interface AgentSecretFact {
  kind: "key" | "token" | "aws" | "subscription";
  owner: "own" | "shared";
  residency: "proxy" | "sandbox" | "resolved_at_launch";
  local_delivery?: LocalDeliveryMode;
}

/** The exact frozen managed-settings document Wardyn writes for the resolved autonomy level (OD-2). Read-only. */
export interface ManagedSettingsFact {
  path: string;
  /** The bytes agentpolicy.ForAgent returns. */
  document: string;
  locked?: boolean;
  locked_by?: "profile" | "organisation";
  profile?: string;
}

export interface TelemetryFact {
  off: boolean;
  env?: string[];
}

/** The agent component's fact (`kind: "agent"`). */
export interface AgentFact {
  agent: string;
  model_provider?: AgentModelProviderFact;
  hosts: AgentHostFact[];
  secrets: AgentSecretFact[];
  managed_settings?: ManagedSettingsFact;
  telemetry?: TelemetryFact;
}

/**
 * A registered runner as the Run panel's placement control reads it. PROVISIONAL:
 * GET /me/runners (H10) owns the real shape; New Run reads runners through this
 * one type so that lane replaces it in one place.
 */
export interface PlacementRunner {
  id: string;
  name: string;
  state: "unclaimed" | "online" | "offline";
  /** RFC 3339; set when `state` is offline. */
  last_seen?: string;
}

export type ImageSourceKind = "agent" | "workspace" | "build" | "org_allowed";

/** Where the resolved image comes from: the Runner tab's Image section says "from {source}". Name is empty for the agent. */
export interface ImageSource {
  kind: ImageSourceKind;
  name?: string;
}

/** The image the run would start from. Absent until the image lane resolves it. */
export interface ImageFact {
  ref: string;
  source: ImageSource;
}

/** One image the organisation lets a person choose for a run (`allowed_image` on the request). */
export interface AllowedImage {
  ref: string;
  name?: string;
}

// The run-mode contract (pkg/client/runs_mode.go): what a person is starting. A
// new client sends `experience` with these carriers in place of agent, task,
// task_mode, interactive and interactive_start; the server refuses a request that
// mixes the two (run_mode_conflict) and never infers an unset mode.

/** The canonical run mode (Go: types.RunExperience), stored on the run as `AgentRun.experience`. */
export type Experience = "background" | "interactive";

export type WorkloadKind = "agent_task" | "command";

/** What a Background run executes: an agent task (agent and task) or a command (command). */
export interface RunWorkload {
  kind: WorkloadKind;
  agent?: string;
  task?: string;
  command?: string;
}

export type IncludedToolKind = "harness" | "component";

/**
 * One included tool instance. Including a tool neither starts it nor grants it a
 * credential: each carries its own provider or configuration and its own tool
 * rules, so independent tools never share one selection. `id` is the key
 * `startup.tool`, the approval broker and the per-harness rules use.
 */
export interface IncludedTool {
  id: string;
  kind: IncludedToolKind;
  model_provider?: string;
  component?: ComponentRef;
  tool_rules?: ToolRule[];
  default_effect?: "allow" | "hold" | "deny";
}

export type StartupKind = "none" | "harness" | "command";

/** What an Interactive run starts on its own: one choice, never two competing launches. */
export interface RunStartup {
  kind: StartupKind;
  tool?: string;
  command?: string;
}

export type StartFolderKind = "image_default" | "attachment";

/** The run's exactly-one starting folder; `attachment` is a workspace id or `drive`. */
export interface StartFolder {
  kind: StartFolderKind;
  attachment?: string;
  subpath?: string;
}

/** The attachment name of the person's drive in `StartFolder.attachment`. */
export const START_FOLDER_DRIVE = "drive";
