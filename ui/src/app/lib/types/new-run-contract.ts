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
import type { LimitSource } from "./runner-pools";

export type PlacementValue = "remote" | "local";

/** How an organisation-held credential reaches a local run (OD-12). */
export type LocalDeliveryMode = "own" | "via_org" | "runner_resident" | "refuse";

/** The CPU, memory and main-drive size a request asks for (Go: client.RunResources). A zero or absent field asks for the placement's default. */
export interface RequestedResources {
  /** Milli-CPU: 2000 is two CPUs. The console works in tenths of a CPU. */
  cpu_millis?: number;
  /** Whole MiB. */
  memory_mib?: number;
  /** The main drive in MiB, 0 to 16 TiB. A soft request: over the cap it is reduced and said so; a runner that cannot cap disk runs it uncapped. */
  disk_mib?: number;
}

/** Where a main-drive size came from (Go: client.MainDriveBasis). */
export type MainDriveBasis = "request" | "policy" | "default" | "none";

/** What cut a main-drive size (Go: client.MainDriveClampSource). */
export type MainDriveClampSource = "pool" | "governance" | "deployment" | "policy";

/** The main-drive size a run asks for and gets, the number dispatch creates the sandbox with (Go: client.MainDriveApplied). */
export interface MainDriveApplied {
  requested_mib?: number;
  /** 0 is no size: unbounded scratch. */
  applied_mib: number;
  basis: MainDriveBasis;
  /** With basis "default": the limit whose default sized the run. */
  default_source?: LimitSource;
  clamped_by?: MainDriveClampSource;
  /** True when a runner that cannot cap disk starts the run uncapped. A size the run's policy writes is never soft. */
  soft: boolean;
}

/** The CPU and memory a run asked for and gets (Go: client.AppliedResources). */
export interface AppliedResources {
  cpu_millis: AppliedAmount;
  memory_mib: AppliedAmount;
}

/** One field of AppliedResources (Go: client.AppliedAmount). `source` names the limit that cut an over-ask when `capped`, else the one whose default an ask of 0 got; `source_name` is the pool's or the profile's name. */
export interface AppliedAmount {
  /** The request's value, else its source policy's; 0 asks for the default. */
  requested: number;
  /** What the sandbox gets. */
  applied: number;
  capped?: boolean;
  source?: LimitSource;
  source_name?: string;
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

/** Adds push content rules for one provider host and organisation. Monotone: never removes one. */
export interface PushRuleOverride {
  provider: "github" | "azure_devops";
  /** A GitHub Enterprise Server or Azure DevOps Server host; absent is the provider's public host. */
  host?: string;
  org: string;
  deny_paths?: string[];
  require_review_paths?: string[];
}

/**
 * Hosts the person adds to the run as a whole, on top of the chosen base policy.
 * The base's own hosts are read-only: the remove lists exist so the server can
 * refuse them, and a panel never offers them.
 */
export interface RunHostOverrides {
  add_allowed?: string[];
  add_denied?: string[];
  remove_allowed?: string[];
  remove_denied?: string[];
}

/**
 * Run-wide egress choices on top of the chosen base policy: how an unlisted
 * destination is handled, and "request all other egress". Both widen and are
 * clamped by the ceiling. `allow_all_egress` is only ever true; leave it out
 * rather than send false.
 */
export interface RunEgressOverrides {
  first_use_approval?: "always_deny" | "deny_with_review" | "wait_for_review";
  allow_all_egress?: true;
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
  hosts?: RunHostOverrides;
  egress?: RunEgressOverrides;
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
  /** Whose document this is: the started agent's, else the first harness of the set that brings one. */
  harness?: string;
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

export type ImageSourceKind = "base" | "agent" | "harness_set" | "workspace" | "build" | "org_allowed";

/** Where the resolved image comes from: the Runner tab's Image section says "from {source}". Name is the harness for agent and the set key for harness_set; empty for base. */
export interface ImageSource {
  kind: ImageSourceKind;
  name?: string;
}

/** The image the run would start from. Absent until the image lane resolves it. */
export interface ImageFact {
  ref: string;
  source: ImageSource;
}

/** One included harness's model credential: provider, kind, and where it lands. */
export interface HarnessModelCredential {
  provider: string;
  kind: string;
  residency: string;
}

/** One included harness: whether it starts with the run, and its model credential (absent when it takes none or the choice is open). */
export interface HarnessFact {
  id: string;
  starts: boolean;
  model_credential?: HarnessModelCredential;
}

/** The run's included harnesses as the door resolved them. `shared_model_access`: two or more carry a model credential, which any process in the sandbox can use. */
export interface HarnessSetFact {
  harnesses: HarnessFact[];
  shared_model_access: boolean;
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
  /** The started harness's first message; a harness startup only. */
  prompt?: string;
}

export type StartFolderKind = "image_default" | "attachment";

/**
 * The run's exactly-one starting folder; `attachment` is a workspace id or `drive`.
 * `repo` names one repository the run clones from that workspace; `drive_id` names
 * one entry of the request's `drives`, exclusive with `attachment`.
 */
export interface StartFolder {
  kind: StartFolderKind;
  attachment?: string;
  repo?: string;
  drive_id?: string;
  subpath?: string;
}

/** One drive the run attaches from GET /me/drives (pkg/client.DriveAttachment); `read_only` narrows only. */
export interface DriveAttachment {
  drive_id: string;
  read_only?: boolean;
}

/** One repository a workspace selection clones (pkg/client.RepoSelection); "" ref = the source's own. */
export interface RepoSelection {
  repo: string;
  ref?: string;
}

/** One entry of the run's mount map on the preflight and the policy preview (pkg/client.RunAttachment). */
export interface RunAttachment {
  kind: "repository" | "folder" | "drive";
  workspace_id?: string;
  repo?: string;
  ref?: string;
  drive_id?: string;
  /** The workspace or drive name, never a host path. */
  name: string;
  target: string;
  read_only: boolean;
  starts_here: boolean;
}

/** The attachment name of the person's drive in `StartFolder.attachment`. */
export const START_FOLDER_DRIVE = "drive";
