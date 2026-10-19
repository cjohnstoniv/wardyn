/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A profile's limits as chips, in GOVERNANCE's own labels — the Governance
// list's Limits cell and the User types editor's read-only "Ceiling and run
// limits" section render the same words for the same field.
import type { GovernanceLimits } from "../../../lib/api/governance";
import { foldAutonomyRubric, GOVERNANCE as GOV, LIMITS_CHIP, MEMBER_DRIVE_DOOR, runLimitsChip, setsRunLimits } from "../../../lib/governance-copy";
import { AUTONOMY_META } from "../../wardyn/autonomy-meta";
import { Chip } from "../../wardyn/primitives";

// Every limit a profile can carry: the doors, the run quota, the strictest
// autonomy cap, the run-limits summary (RL-14, #579) and the numeric caps. An
// empty array is the profile bounding none of them — the caller's "None",
// unless setsRunLimits says a run-limit field the summary does not name (a
// default end, say) still binds; limitsBoundNothing is that whole question.
export function limitChips(limits: GovernanceLimits) {
  const autonomyLowest = foldAutonomyRubric(limits.autonomy_rubric).lowest;
  const chips: string[] = [];
  if (limits.deny_task_mode_exec) chips.push(GOV.LIMIT_EXEC_LABEL);
  if (limits.deny_interactive) chips.push(GOV.LIMIT_INTERACTIVE_LABEL);
  if (limits.deny_user_drive) chips.push(GOV.LIMIT_DRIVE_LABEL);
  if (limits.deny_member_drives) chips.push(MEMBER_DRIVE_DOOR.LABEL);
  if (limits.deny_ui_apps) chips.push(LIMITS_CHIP.UI_APPS);
  for (const placement of limits.deny_placements ?? []) chips.push(LIMITS_CHIP.PLACEMENT(placement));
  if ((limits.max_concurrent_runs ?? 0) > 0) chips.push(GOV.LIMIT_QUOTA_LABEL(limits.max_concurrent_runs!));
  // Ruling 2 (#96 review): names the STRICTEST cap, not merely that a rubric
  // exists — on the chip face, not behind a tooltip.
  if (autonomyLowest) chips.push(LIMITS_CHIP.AUTONOMY(AUTONOMY_META[autonomyLowest].label));
  const runLimits = runLimitsChip(limits);
  if (runLimits) chips.push(runLimits);
  // 0/absent is no limit for each of these.
  const caps: [string, number | undefined][] = [
    [LIMITS_CHIP.CPU, limits.max_cpu_millis],
    [LIMITS_CHIP.MEMORY, limits.max_memory_mib],
    [GOV.LIMIT_EPHEMERAL_LABEL, limits.max_ephemeral_disk_mib],
    [GOV.LIMIT_DRIVE_SIZE_LABEL, limits.max_drive_size_mib],
  ];
  for (const [label, n] of caps) if ((n ?? 0) > 0) chips.push(`${label}: ${n}`);
  return chips.map((label) => (
    <Chip key={label} tone="neutral">
      {label}
    </Chip>
  ));
}

// "None" is a claim that the profile bounds nothing, so it is read from the
// chips and the run-limit fields together, never from one list alone.
export const limitsBoundNothing = (limits: GovernanceLimits) => limitChips(limits).length === 0 && !setsRunLimits(limits);
