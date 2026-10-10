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

/** Who is responsible for a provenance row; an id or name only where the kind has one. */
export interface ProvenanceSource {
  // An unknown kind renders no chip, never a guess.
  kind: "policy" | "ceiling" | "workspace" | "component" | "model_provider" | "person" | (string & {});
  id?: string;
  name?: string;
}

/**
 * Why one entry of the resolved spec is there (internal/api's provenanceRow).
 * field is a RunPolicySpec json name; value a redaction-safe entry key: a host,
 * a grant as "kind:host", a mount's target, a repository as "repo@ref", a capability.
 */
export interface ProvenanceEntry {
  field: string;
  value: string;
  source: ProvenanceSource;
  // clamped: the source asked and the ceiling removed it. narrowed: it stands, reduced, or a
  // capability was taken away. removed: the person's override dropped it.
  effect: "added" | "clamped" | "narrowed" | "removed" | (string & {});
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
  // Why each entry is in `spec`, from the same fold as the launch. The server always
  // sends the array; optional here so an older server's body still types.
  provenance?: ProvenanceEntry[];
}
