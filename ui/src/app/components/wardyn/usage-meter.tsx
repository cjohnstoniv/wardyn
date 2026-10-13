/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { cn } from "../ui/utils";

/**
 * A quantity against a limit. The value text is always shown, never colour alone. With no
 * `max` there is no limit to measure against, so it is text only (and no meter role).
 */
export function UsageMeter({
  label,
  used,
  max,
  valueText,
  tone = "neutral",
}: {
  label: string;
  used: number;
  max?: number;
  valueText: string;
  tone?: "neutral" | "warning";
}) {
  const atCap = max !== undefined && used >= max;
  const pct = max && max > 0 ? Math.min(100, Math.max(0, (used / max) * 100)) : 0;
  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-baseline justify-between gap-3 text-xs">
        <span className="text-foreground">{label}</span>
        <span className="text-muted-foreground">{valueText}</span>
      </div>
      {max !== undefined && (
        <div
          role="meter"
          aria-label={label}
          aria-valuemin={0}
          aria-valuemax={max}
          aria-valuenow={Math.min(Math.max(used, 0), max)}
          aria-valuetext={valueText}
          className="h-1.5 overflow-hidden rounded-full bg-muted"
        >
          <div
            className={cn("h-full rounded-full", atCap || tone === "warning" ? "bg-warning" : "bg-info")}
            style={{ width: `${pct}%` }}
          />
        </div>
      )}
    </div>
  );
}
