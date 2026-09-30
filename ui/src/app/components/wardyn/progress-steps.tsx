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
export type StepRow = { key: string; label: string; mark: StepMark };

function StepMarkIcon({ mark }: { mark: StepMark }) {
  if (mark === "done") return <Check className="size-3.5 shrink-0 text-success" aria-hidden />;
  if (mark === "active") return <Loader2 className="size-3.5 shrink-0 animate-spin text-info" aria-hidden />;
  if (mark === "failed") return <TriangleAlert className="size-3.5 shrink-0 text-danger" aria-hidden />;
  return <span className="size-3.5 shrink-0 rounded-full border border-border" aria-hidden />;
}

export function ProgressSteps({ rows, testId }: { rows: StepRow[]; testId: string }) {
  return (
    <ol className="space-y-1.5" data-testid={testId}>
      {rows.map(({ key, label, mark }) => (
        <li
          key={key}
          data-state={mark}
          aria-current={mark === "active" ? "step" : undefined}
          className={
            "flex items-center gap-2 text-xs " + (mark === "pending" ? "text-muted-foreground" : "text-foreground")
          }
        >
          <StepMarkIcon mark={mark} />
          {label}
        </li>
      ))}
    </ol>
  );
}
