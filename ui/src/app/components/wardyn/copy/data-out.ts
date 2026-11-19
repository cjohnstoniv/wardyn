/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The data_out rule in the editors: the policy section, a profile's Ceiling row and the Wardyn boundary's card.
// One rule for content leaving a run, set as allowed, review or blocked and met as the stricter.
import type { RunPolicySpec } from "../../../lib/types/policy";

export type DataOutLevel = NonNullable<RunPolicySpec["data_out"]>;

const LEVELS: readonly { value: DataOutLevel; label: string }[] = [
  { value: "allowed", label: "Allowed" },
  { value: "review", label: "Review" },
  { value: "blocked", label: "Blocked" },
];

export const DATA_OUT = {
  LABEL: "Data out",
  LEVELS,
  // A value's label; a rule that is not set reads as allowed.
  label: (v: string | undefined): string => LEVELS.find((o) => o.value === v)?.label ?? LEVELS[0].label,
  ALLOWED: "Content can leave the run by every path its other rules allow.",
  REVIEW:
    "Files leave one at a time through the Files panel, each with a person's yes. SSH file transfer, SSH commands, port forwards, run output, recording replay, web apps and computer folders are off.",
  BLOCKED_RUN: "Nothing leaves by those paths, the Files panel included, and git push is off.",
  BLOCKED_GOVERNED: "Nothing leaves by those paths, the Files panel included; git push, drives and keeping the machine are off.",
  UNSET: "Not set: allowed, unless a layer above holds content in.",
  NOT_COVERED: "Reading the terminal or the screen is not covered: turn off interactive access for that.",
  SET_ABOVE: "Set above",
  FLOOR: (unit: string, value: string, looser: readonly string[]): string =>
    `${unit} holds this at ${value} or stricter, so ${looser.join(" and ")} is not available.`,
  CEILING_HINT: "Content leaving runs under this profile. A profile can only hold in more than its base.",
  BOUNDARY_COUNT: "1 setting",
  BOUNDARY_APPLIES: "Applies to every run, break-glass included.",
  BOUNDARY_ROW: "Content leaving a run",
  BOUNDARY_BELOW: "Profiles below may only hold in more.",
  BOUNDARY_ENFORCED:
    "Enforced by Wardyn at each door: the Files panel, SSH, run output, recording replay, web apps and computer folders. Reading the terminal or the screen is not covered: turn off interactive access for that.",
} as const;
