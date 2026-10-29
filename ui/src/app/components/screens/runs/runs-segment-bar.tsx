/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// R17-N3 — All · Agent tasks · Environments · Apps & scripts · Services, each with its count. Buttons
// with aria-pressed, as Segmented is, because each option carries a count beside its label.
import { cn } from "../../ui/utils";
import { RUNS_SEGMENTS } from "../../wardyn/copy/runs-landing";
import type { RunsSegmentFilter } from "./runs-segment";

const OPTIONS: { value: RunsSegmentFilter; label: string }[] = [
  { value: "all", label: RUNS_SEGMENTS.ALL },
  { value: "agent_tasks", label: RUNS_SEGMENTS.AGENT_TASKS },
  { value: "environments", label: RUNS_SEGMENTS.ENVIRONMENTS },
  { value: "apps", label: RUNS_SEGMENTS.APPS },
  { value: "services", label: RUNS_SEGMENTS.SERVICES },
];

export function RunsSegmentBar({
  value,
  counts,
  onChange,
}: {
  value: RunsSegmentFilter;
  counts: Record<RunsSegmentFilter, number>;
  onChange: (next: RunsSegmentFilter) => void;
}) {
  return (
    <div
      role="group"
      aria-label={RUNS_SEGMENTS.LABEL}
      className="mt-3.5 inline-flex w-fit max-w-full overflow-x-auto rounded-lg border border-border-strong"
    >
      {OPTIONS.map((o) => (
        <button
          key={o.value}
          type="button"
          aria-pressed={value === o.value}
          onClick={() => onChange(o.value)}
          className={cn(
            "flex items-center gap-1.5 whitespace-nowrap border-l border-border px-3 py-1.5 text-sm transition-colors first:border-l-0",
            value === o.value ? "bg-primary font-medium text-primary-foreground" : "text-muted-foreground hover:text-foreground",
          )}
        >
          {o.label}{" "}
          <span className="text-meta tabular-nums">{counts[o.value]}</span>
        </button>
      ))}
    </div>
  );
}
