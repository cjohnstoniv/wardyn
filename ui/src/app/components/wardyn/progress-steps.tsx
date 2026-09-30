/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// One step list for every surface that narrates a sandbox coming up: the
// sign-in door and the run page's startup view. Presentational only; the
// caller decides each row's mark. The DOM (`data-state`, `aria-current`) is
// what the door's and the run page's specs assert on.
import { Check, Loader2, TriangleAlert } from "lucide-react";

export type StepMark = "done" | "active" | "pending" | "failed";
// `stale`: a pending row that WAS the active one and is no longer claimed to be
// in progress (the run page's overdue state); only the "run" variant reads it.
export type StepRow = { key: string; label: string; mark: StepMark; stale?: boolean };

function StepMarkIcon({ mark }: { mark: StepMark }) {
  if (mark === "done") return <Check className="size-3.5 shrink-0 text-success" aria-hidden />;
  if (mark === "active") return <Loader2 className="size-3.5 shrink-0 animate-spin text-info" aria-hidden />;
  if (mark === "failed") return <TriangleAlert className="size-3.5 shrink-0 text-danger" aria-hidden />;
  return <span className="size-3.5 shrink-0 rounded-full border border-border" aria-hidden />;
}

// Label colour. "door" is the sign-in door's, unchanged. "run" follows the
// approved run-startup mock: the active row is medium weight, a stale row keeps
// the normal text colour, and a failed row is red.
function labelClass(mark: StepMark, stale: boolean | undefined, variant: "door" | "run"): string {
  if (variant === "door") return mark === "pending" ? "text-muted-foreground" : "text-foreground";
  if (mark === "failed") return "font-medium text-danger";
  if (mark === "active") return "font-medium text-foreground";
  return mark === "pending" && !stale ? "text-muted-foreground" : "text-foreground";
}

export function ProgressSteps({
  rows,
  testId,
  variant = "door",
}: {
  rows: StepRow[];
  testId: string;
  variant?: "door" | "run";
}) {
  return (
    <ol className="space-y-1.5" data-testid={testId}>
      {rows.map(({ key, label, mark, stale }) => (
        <li
          key={key}
          data-state={mark}
          aria-current={mark === "active" ? "step" : undefined}
          className={"flex items-center gap-2 text-xs " + labelClass(mark, stale, variant)}
        >
          <StepMarkIcon mark={mark} />
          {label}
        </li>
      ))}
    </ol>
  );
}
