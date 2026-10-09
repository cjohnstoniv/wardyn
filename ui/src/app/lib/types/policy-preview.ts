/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import type { ComponentFact } from "./components";
import type { RunPolicySpec } from "./policy";

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
