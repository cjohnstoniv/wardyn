/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The New Run refusals the server and the console say in the same bytes: the
// wire reasons, and the sentences of the workspace refusals. The console prints
// the server's sentence on a refusal and uses these for its own pre-check, so the
// two never disagree. Both sides are pinned to workspace-refusals.golden.json
// (internal/api/run_contract_test.go, new-run-refusals.test.ts); change a
// sentence in workspace_refusals.go, here and in that table together.
//
// Strings for the New Run panels live in copy/new-run-flow.ts; its
// NEW_RUN_FLOW.WORKSPACES keys re-export these.

/** The wire reasons the New Run request contract answers with (internal/api/reasons.go). */
export const NEW_RUN_REASON = {
  PLACEMENT_INVALID: "placement_invalid",
  WORKSPACE_TARGET_INVALID: "workspace_target_invalid",
  WORKSPACE_TARGET_OVERLAP: "workspace_target_overlap",
  WORKSPACE_PIN_CONFLICT: "workspace_pin_conflict",
  WORKSPACE_IMAGE_CONFLICT: "workspace_image_conflict",
  WORKSPACE_ADO_ORG_CONFLICT: "workspace_ado_org_conflict",
  RESOURCES_INVALID: "resources_invalid",
  OVERRIDE_INVALID: "override_invalid",
  OVERRIDE_REFUSED: "override_refused",
  /** A field that changes the run's posture, which the server cannot honour yet: refused, never dropped. */
  REQUEST_FIELD_UNAVAILABLE: "request_field_unavailable",
} as const;

export type NewRunReason = (typeof NEW_RUN_REASON)[keyof typeof NEW_RUN_REASON];

/** The closed set of placement and runner-link refusal reasons (internal/placement/reasons.go). */
export const PLACEMENT_REASONS = [
  "placement_required",
  "placement_unavailable",
  "placement_denied",
  "placement_credential",
  "placement_trusted_output",
  "placement_component_self_defined",
  "placement_capability",
  "placement_local_path",
  "runner_offline",
  "runner_ambiguous",
  "runner_not_found",
  "runner_unclaimed",
  "runner_revoked",
  "runner_token_invalid",
  "runner_claim_mismatch",
  "runner_posture_unmet",
  "runner_keystore_unavailable",
  "runner_action_pending",
  "relay_runner_mismatch",
  "relay_required",
  "relay_route_refused",
  "delivery_via_org",
  "delivery_runner_resident",
  "delivery_not_via_org",
  "via_org_destination_refused",
  "via_org_cap_exceeded",
] as const;

export type PlacementReason = (typeof PLACEMENT_REASONS)[number];

/** The workspace refusals' sentences, byte for byte what internal/api/workspace_refusals.go says. */
export const WORKSPACE_REFUSAL = {
  TARGET_SHAPE: (path: string) => `${path} must be an absolute path under /home/agent, such as /home/agent/work.`,
  OVERLAP_EQUAL: (path: string, other: string) => `${path} is also where ${other} mounts. Give one of them another path.`,
  OVERLAP_NESTED: (path: string, other: string, otherPath: string) =>
    `${path} is inside ${otherPath}, where ${other} mounts. Two mounts can't nest.`,
  PIN_CONFLICT: (a: string, pinA: string, b: string, pinB: string) =>
    `${a} uses ${pinA} and ${b} uses ${pinB}. A run uses one model provider, so remove one of them.`,
  IMAGE_CONFLICT: (a: string, b: string) => `${a} and ${b} use different base images. A run uses one image, so remove one of them.`,
  ADO_ORG_CONFLICT: (a: string, orgA: string, b: string, orgB: string) =>
    `${a} uses Azure DevOps organisation ${orgA} and ${b} uses ${orgB}. A run signs in to one Azure DevOps organisation, so remove one of them.`,
} as const;

/** The reason a refusal belongs to, for the sentences above; the console matches on these, never on the text. */
export const WORKSPACE_REFUSAL_REASON = {
  TARGET_SHAPE: NEW_RUN_REASON.WORKSPACE_TARGET_INVALID,
  OVERLAP_EQUAL: NEW_RUN_REASON.WORKSPACE_TARGET_OVERLAP,
  OVERLAP_NESTED: NEW_RUN_REASON.WORKSPACE_TARGET_OVERLAP,
  PIN_CONFLICT: NEW_RUN_REASON.WORKSPACE_PIN_CONFLICT,
  IMAGE_CONFLICT: NEW_RUN_REASON.WORKSPACE_IMAGE_CONFLICT,
  ADO_ORG_CONFLICT: NEW_RUN_REASON.WORKSPACE_ADO_ORG_CONFLICT,
} as const;

/** Every sentence function by key, for the parity test and for callers that pick one by reason. */
export const WORKSPACE_REFUSAL_BY_KEY: Record<keyof typeof WORKSPACE_REFUSAL, (...args: string[]) => string> = {
  TARGET_SHAPE: (...a) => WORKSPACE_REFUSAL.TARGET_SHAPE(a[0]),
  OVERLAP_EQUAL: (...a) => WORKSPACE_REFUSAL.OVERLAP_EQUAL(a[0], a[1]),
  OVERLAP_NESTED: (...a) => WORKSPACE_REFUSAL.OVERLAP_NESTED(a[0], a[1], a[2]),
  PIN_CONFLICT: (...a) => WORKSPACE_REFUSAL.PIN_CONFLICT(a[0], a[1], a[2], a[3]),
  IMAGE_CONFLICT: (...a) => WORKSPACE_REFUSAL.IMAGE_CONFLICT(a[0], a[1]),
  ADO_ORG_CONFLICT: (...a) => WORKSPACE_REFUSAL.ADO_ORG_CONFLICT(a[0], a[1], a[2], a[3]),
};

/**
 * The server's target rule, for the console's own pre-check: absolute, already
 * cleaned, no ".." element, strictly under /home/agent. The server adds the
 * authored-target deny-list; its answer is authoritative.
 */
export function workspaceTargetShapeOk(target: string): boolean {
  if (!target.startsWith("/home/agent/")) return false;
  const parts = target.split("/");
  if (parts.includes("..") || parts.includes(".")) return false;
  // Cleaned: no empty element (a "//" or a trailing "/").
  return !parts.slice(1).includes("");
}

const within = (inner: string, outer: string) => inner === outer || inner.startsWith(`${outer}/`);

/** One mount the overlap check compares: a workspace's id and its target. */
export interface PlacedTarget {
  workspace: string;
  target: string;
}

/**
 * The first overlap between `entry` and the targets already placed for other
 * workspaces (OD-3), as the sentence the server would answer. A workspace may
 * nest inside its own sources, so entries of one workspace never overlap.
 */
export function workspaceTargetOverlap(entry: PlacedTarget, placed: readonly PlacedTarget[]): string | null {
  for (const other of placed) {
    if (other.workspace === entry.workspace) continue;
    if (entry.target === other.target) return WORKSPACE_REFUSAL.OVERLAP_EQUAL(entry.target, other.workspace);
    if (within(entry.target, other.target)) return WORKSPACE_REFUSAL.OVERLAP_NESTED(entry.target, other.workspace, other.target);
    if (within(other.target, entry.target)) return WORKSPACE_REFUSAL.OVERLAP_NESTED(other.target, entry.workspace, entry.target);
  }
  return null;
}
