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
  // A person's own runner (M10): a drive with no second place, and one bound to another runner. The quoted
  // name is Go's %q.
  ORG_RUNNERS_ONLY: (name: string) =>
    `drive: ${JSON.stringify(name)} mounts only from the organisation's runners — choose an organisation pool, or launch without drive`,
  BOUND_ELSEWHERE: (name: string) =>
    `drive: ${JSON.stringify(name)} is bound to a different runner — choose an organisation pool, or launch without drive`,
  TARGET_CONFLICT: (a: string, b: string, target: string) => `${a} and ${b} both mount at ${target}. Attach one of them.`,
  SHARE_BESIDE_MANAGED: (share: string, managed: string) =>
    `${share} is a shared drive and ${managed} a managed one; a Kubernetes run mounts one kind or the other. Attach one of them.`,
  // A macOS VM can't open a Docker volume or a cluster volume.
  MACOS_DOCKER_VOLUME: (name: string) => `${name} is a Docker volume, which a macOS VM can't open.`,
  MACOS_CLUSTER_VOLUME: (name: string) => `${name} is a cluster volume, which a macOS VM can't open.`,
  // A person's disk drive is kept on one Mac, and only a macOS VM there attaches it.
  MAC_DISK_ELSEWHERE: (name: string, mac: string) => `${name} is a disk on ${mac}, so only macOS VM runs on ${mac} can use it.`,
  // Member drives: POST /me/drives and DELETE /me/drives/{id}. A quoted name is
  // Go's %q, which JSON.stringify matches for the names a drive can carry.
  MEMBER_REQUIRES_PERSON: () => "Sign in under your own account to create or delete a drive.",
  MEMBER_DISABLED: () => "Your organisation doesn't let people create drives. Ask an admin.",
  MEMBER_DENIED_BY_UNIT: (profile: string) =>
    `Your governance profile ${JSON.stringify(profile)} doesn't let you create drives.`,
  MEMBER_LIMIT_REACHED: (n: string, limit: string) => `You have ${n} of ${limit} drives. Delete one to create another.`,
  MEMBER_UNSUPPORTED: () => "This server can't create drives.",
  MEMBER_UNSIZED: () => "This server can't create drives until an admin sets their size.",
  MEMBER_DELETE_UNAVAILABLE_CREATE: () => "This server can't delete drives, so it can't create them. Ask your admin.",
  MEMBER_DELETE_UNAVAILABLE: () => "This server can't delete drives right now. Ask your admin.",
  MEMBER_NAME_INVALID: () => "Give the drive a name with at least one letter or digit, up to 128 characters.",
  MEMBER_NAME_TAKEN: (name: string) => `${JSON.stringify(name)} is already the name of a drive you can use. Pick another name.`,
  MEMBER_IN_USE: () =>
    "A run is still using this drive. End that run, or delete a run kept after its end, then try again.",
  MEMBER_BUSY: () => "This drive is already being deleted.",
  MEMBER_DELETE_FAILED: () => "The drive's files couldn't be deleted. Nothing was removed; try again.",
  MEMBER_RATE_LIMITED: () => "Too many drive changes; wait a minute and try again.",
  // A disk drive kept on a Mac: made on one of the person's own Macs, and deleted there.
  MEMBER_MAC_UNAVAILABLE: () => "runner_id: choose one of your own Macs, online, whose runner keeps disk drives.",
  MEMBER_DELETE_DEFERRED: (mac: string) => `Deleting. ${mac} is offline, so the disk is removed when it reconnects.`,
  MEMBER_IN_RUN_ON_MAC: (name: string, mac: string) => `${name} is in a run on ${mac}. Delete it after that run ends.`,
  MEMBER_RECORD_ONLY: (mac: string) =>
    `${mac} is no longer registered, so this drive's disk stays on that Mac. Deleting removes only the record.`,
} as const;

/** The reason each sentence is sent with; the console matches on these, never on the text. */
export const DRIVE_REFUSAL_REASON = {
  SELECTION_CONFLICT: "drive_selection_conflict",
  INVALID: "drive_invalid",
  TOO_MANY: "drive_invalid",
  NOT_FOUND: "drive_not_found",
  NOT_IN_POOL: "drive_not_in_pool",
  ORG_RUNNERS_ONLY: "drive_not_in_pool",
  BOUND_ELSEWHERE: "drive_not_in_pool",
  TARGET_CONFLICT: "drive_target_conflict",
  SHARE_BESIDE_MANAGED: "drive_share_beside_managed",
  MACOS_DOCKER_VOLUME: "placement_capability",
  MACOS_CLUSTER_VOLUME: "placement_capability",
  MAC_DISK_ELSEWHERE: "drive_not_in_pool",
  MEMBER_REQUIRES_PERSON: "member_drive_requires_person",
  MEMBER_DISABLED: "member_drives_disabled",
  MEMBER_DENIED_BY_UNIT: "member_drives_denied_by_unit",
  MEMBER_LIMIT_REACHED: "member_drive_limit_reached",
  MEMBER_UNSUPPORTED: "member_drive_unsupported",
  MEMBER_UNSIZED: "member_drive_unsupported",
  MEMBER_DELETE_UNAVAILABLE_CREATE: "member_drive_delete_unavailable",
  MEMBER_DELETE_UNAVAILABLE: "member_drive_delete_unavailable",
  MEMBER_NAME_INVALID: "member_drive_name_invalid",
  MEMBER_NAME_TAKEN: "member_drive_name_taken",
  MEMBER_IN_USE: "member_drive_in_use",
  MEMBER_BUSY: "member_drive_busy",
  MEMBER_DELETE_FAILED: "member_drive_delete_failed",
  MEMBER_RATE_LIMITED: "member_drive_rate_limited",
  MEMBER_MAC_UNAVAILABLE: "member_drive_unsupported",
  MEMBER_DELETE_DEFERRED: "member_drive_delete_deferred",
  MEMBER_IN_RUN_ON_MAC: "member_drive_in_use",
  MEMBER_RECORD_ONLY: "member_drive_record_only",
} as const satisfies Record<keyof typeof DRIVE_REFUSAL, string>;
