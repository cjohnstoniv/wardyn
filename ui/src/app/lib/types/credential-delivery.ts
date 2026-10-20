/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The local_credential_delivery governance document (types only): per credential class, how an
// organisation-held credential may reach a run on a person's own runner. Every interface mirrors one
// Go struct and credential-delivery.wire.test.ts pins its json tags, the closed value sets and the
// bounds table below to internal/placement/delivery.go, which is where the server enforces them.

import type { RunnerTrustClass } from "./runners";

/** Go: placement.Mode. */
export type DeliveryMode = "own" | "via_org" | "runner_resident" | "refuse";

/** Go: placement.Class* — the closed set of design §7.2, in the order the editor lists them. */
export const DELIVERY_CLASSES = [
  "api_key",
  "bedrock_bearer",
  "aws_sso_bearer",
  "azure_foundry_token",
  "bedrock_role_credentials",
  "env_secret",
  "file_secret",
  "ssh_key",
  "git_pat_broker",
  "git_pat_helper",
  "github_token",
  "ado_minted_pat",
  "oauth_subscription",
  "cloud_sts",
] as const;
export type DeliveryClass = (typeof DELIVERY_CLASSES)[number];

/** Go: placement.classModes. The server's 400 stays the gate; this only tells the editor what to disable. */
export const CLASS_MODES: Record<DeliveryClass, readonly DeliveryMode[]> = {
  oauth_subscription: ["own"],
  api_key: ["via_org", "runner_resident", "refuse"],
  bedrock_bearer: ["via_org", "runner_resident", "refuse"],
  aws_sso_bearer: ["via_org", "runner_resident", "refuse"],
  azure_foundry_token: ["via_org", "runner_resident", "refuse"],
  bedrock_role_credentials: ["runner_resident", "refuse"],
  env_secret: ["runner_resident", "refuse"],
  file_secret: ["runner_resident", "refuse"],
  ssh_key: ["runner_resident", "refuse"],
  git_pat_broker: ["via_org", "runner_resident", "refuse"],
  git_pat_helper: ["runner_resident", "refuse"],
  github_token: ["via_org", "refuse"],
  ado_minted_pat: ["via_org", "runner_resident", "refuse"],
  cloud_sts: ["refuse"],
};

/** Go: placement.fixedClasses — a policy may not write these. */
export const FIXED_CLASSES: readonly DeliveryClass[] = ["oauth_subscription", "cloud_sts"];

/** The OS keys a runner reports (types.RunnerPosture.OS) that os_min is keyed by. */
export const POSTURE_OSES = ["darwin", "windows", "linux"] as const;
export type PostureOS = (typeof POSTURE_OSES)[number];

/** Go: placement.PostureRequirement. Only valid under `runner_resident`. */
export interface PostureRequirement {
  mdm_managed?: boolean;
  disk_encrypted?: boolean;
  // Keyed by the OS a runner reports; the Go side takes any key, so one the editor has no field for can be stored.
  os_min?: Record<string, string>;
}

/** Go: placement.ClassPolicy. */
export interface ClassPolicy {
  mode: DeliveryMode;
  require_posture?: PostureRequirement;
}

/** Go: placement.TrustPolicy — one runner trust class's overrides of the global classes. */
export interface TrustPolicy {
  classes: Partial<Record<DeliveryClass, ClassPolicy>>;
}

/**
 * Go: placement.DeliveryPolicy. A class absent from `classes` means `refuse`. `by_trust` overrides a class
 * on runners of one trust class; a class it does not name falls back to `classes`. An override never sets
 * or overrides `via_org`, and never carries `require_posture` under `org_cluster` (the server's 400).
 */
export interface DeliveryPolicy {
  classes: Partial<Record<DeliveryClass, ClassPolicy>>;
  by_trust?: Partial<Record<RunnerTrustClass, TrustPolicy>>;
}

/** Go: types.CredentialDeliveryPolicyDoc (GET /governance/credential-delivery). */
export interface CredentialDeliveryPolicyDoc {
  policy: DeliveryPolicy;
  updated_at: string;
  updated_by: string;
}
