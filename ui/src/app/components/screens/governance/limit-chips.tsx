/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A profile's limits as chips, in GOVERNANCE's own labels — the Governance
// list's Limits cell and the User types editor's read-only "Ceiling and run
// limits" section render the same words for the same field.
import type { GovernanceLimits } from "../../../lib/api/governance";
import { foldAutonomyRubric, GOVERNANCE as GOV, LIMITS_CHIP } from "../../../lib/governance-copy";
import { AUTONOMY_META } from "../../wardyn/autonomy-meta";
import { Chip } from "../../wardyn/primitives";

// The doors, the run quota and the strictest autonomy cap. An empty array is
// the profile bounding none of them — the caller's "None".
export function limitChips(limits: GovernanceLimits) {
  const autonomyLowest = foldAutonomyRubric(limits.autonomy_rubric).lowest;
  const chips: string[] = [];
  if (limits.deny_task_mode_exec) chips.push(GOV.LIMIT_EXEC_LABEL);
  if (limits.deny_interactive) chips.push(GOV.LIMIT_INTERACTIVE_LABEL);
  if (limits.deny_user_drive) chips.push(GOV.LIMIT_DRIVE_LABEL);
  if ((limits.max_concurrent_runs ?? 0) > 0) chips.push(GOV.LIMIT_QUOTA_LABEL(limits.max_concurrent_runs!));
  // Ruling 2 (#96 review): names the STRICTEST cap, not merely that a rubric
  // exists — on the chip face, not behind a tooltip.
  if (autonomyLowest) chips.push(LIMITS_CHIP.AUTONOMY(AUTONOMY_META[autonomyLowest].label));
  return chips.map((label) => (
    <Chip key={label} tone="neutral">
      {label}
    </Chip>
  ));
}

// The two disk caps (0/absent is no limit), labelled as the profile editor
// labels their fields. The Governance list leaves them out; the type editor
// shows them because it is the one place a type's limits are read in full.
export function sizeLimitChips(limits: GovernanceLimits) {
  const sizes: [string, number | undefined][] = [
    [GOV.LIMIT_EPHEMERAL_LABEL, limits.max_ephemeral_disk_mib],
    [GOV.LIMIT_DRIVE_SIZE_LABEL, limits.max_drive_size_mib],
  ];
  return sizes
    .filter(([, n]) => (n ?? 0) > 0)
    .map(([label, n]) => (
      <Chip key={label} tone="neutral">
        {`${label}: ${n}`}
      </Chip>
    ));
}
