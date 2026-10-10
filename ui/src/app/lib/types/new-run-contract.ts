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
