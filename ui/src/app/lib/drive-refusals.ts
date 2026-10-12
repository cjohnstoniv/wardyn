/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The drives[] refusals New Run shows. The same bytes as the server's
// (internal/api/drive_refusals.go); both sides are pinned to
// drive-refusals.golden.json. Change a sentence here, there and in the table
// together.

export const DRIVE_REFUSAL = {
  SELECTION_CONFLICT: () => "Send drive or drives, not both.",
  INVALID: (i: string) => `drives[${i}].drive_id: give a drive id from your drive list, once.`,
  TOO_MANY: (max: string) => `drives: attach at most ${max} drives.`,
  NOT_FOUND: (id: string) => `No drive ${id} is available to you.`,
  NOT_IN_POOL: (name: string) => `${name}: not available in this pool's runners.`,
  TARGET_CONFLICT: (a: string, b: string, target: string) => `${a} and ${b} both mount at ${target}. Attach one of them.`,
  SHARE_BESIDE_MANAGED: (share: string, managed: string) =>
    `${share} is a shared drive and ${managed} a managed one; a Kubernetes run mounts one kind or the other. Attach one of them.`,
} as const;

/** The reason each sentence is sent with; the console matches on these, never on the text. */
export const DRIVE_REFUSAL_REASON = {
  SELECTION_CONFLICT: "drive_selection_conflict",
  INVALID: "drive_invalid",
  TOO_MANY: "drive_invalid",
  NOT_FOUND: "drive_not_found",
  NOT_IN_POOL: "drive_not_in_pool",
  TARGET_CONFLICT: "drive_target_conflict",
  SHARE_BESIDE_MANAGED: "drive_share_beside_managed",
} as const satisfies Record<keyof typeof DRIVE_REFUSAL, string>;
