/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The limits row for the scheduled-runs door. The profile editor renders it with its other doors; it sits in its
// own file so the editor, near its size cap, takes a one-line mount. The matching rubric row (self_defined_component)
// is a row of ProfileRubric, in its own group.
import type { Dispatch, SetStateAction } from "react";
import type { GovernanceLimits } from "../../../lib/api/governance";
import { SCHEDULE_DOOR } from "../../wardyn/copy/schedule-door";
import { Switch } from "../../wardyn/form-primitives";

export function ScheduledRunsDoorRow({
  limits,
  disabled,
  onChange,
}: {
  limits: GovernanceLimits;
  disabled: boolean;
  onChange: Dispatch<SetStateAction<GovernanceLimits>>;
}) {
  return (
    <div className="mt-3 flex items-start gap-3 border-t border-border pt-3 first-of-type:border-t-0">
      <Switch
        checked={!!limits.deny_scheduled_runs}
        onChange={(v) => onChange((l) => ({ ...l, deny_scheduled_runs: v }))}
        disabled={disabled}
        label={SCHEDULE_DOOR.LABEL}
        className="mt-0.5"
      />
      <div className="min-w-0">
        <p className="text-body font-medium text-foreground">{SCHEDULE_DOOR.LABEL}</p>
        <p className="mt-0.5 max-w-[62ch] text-xs text-muted-foreground">{SCHEDULE_DOOR.HINT}</p>
      </div>
    </div>
  );
}
