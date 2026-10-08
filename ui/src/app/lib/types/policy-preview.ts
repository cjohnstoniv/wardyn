/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { AutonomyLevel } from "../api/governance";
import type { RunPolicySpec } from "./policy";
import type { SetupItem } from "./runs";

export type PolicyPreviewPending =
  | "task"
  | "model_provider_selection"
  | "credential_liveness"
  | "autonomy"
  | "tool_approvals"
  | "runner_confinement"
  | "drive_readiness"
  | "dispatch_egress";

export interface PolicyPreviewSource {
  kind: "default" | "profile" | "stored" | "inline";
  name?: string;
  policy_id?: string;
}

export interface PolicyPreviewRepository {
  kind: "github" | "azure_devops" | "other";
  repos: string[];
  org?: string;
  default_profile: string[];
  capability_ceiling: string[];
}

/** One secret of a component: how it reaches the run and whose it is. Never its name. */
export interface ComponentSecretFact {
  delivery: "header" | "env" | "file";
  shared: boolean;
}

/**
 * One row of `components` (internal/api's componentFact): something the run is
 * given access to, as the door decided it. A refused request has no facts.
 */
export interface ComponentFact {
  kind: "custom" | "git_provider";
  provider?: "github" | "azure_devops";
  // A stored component's id, "inline:<position>" or "git_provider:<provider>:<lane>[:<org>]".
  id: string;
  name?: string;
  version?: number;
  reason: "org" | "self" | "inline" | "workspace";
  status: "ready" | "needs_input" | "unavailable" | "unknown";
  requirements: SetupItem[];
  lane?: "app" | "pat" | "ssh" | "entra" | "direct" | "none";
  org?: string;
  repos?: string[];
  hosts?: string[];
  secrets?: ComponentSecretFact[];
  config_keys?: string[];
  self_defined?: boolean;
  autonomy_cap?: AutonomyLevel;
  vault_floor?: boolean;
  tls_intercept?: boolean;
  high_risk?: boolean;
}

/** Authorized, provisional read output. Never use it as the authored draft. */
export interface PolicyPreviewResult {
  spec: RunPolicySpec;
  source: PolicyPreviewSource;
  provisional: boolean;
  redacted: boolean;
  warnings: string[];
  pending: PolicyPreviewPending[];
  repository_access: PolicyPreviewRepository[];
  // Absent for a draft with no component and no GitHub or Azure DevOps repository.
  components?: ComponentFact[];
}
