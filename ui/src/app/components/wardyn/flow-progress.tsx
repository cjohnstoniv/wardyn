/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Check } from "lucide-react";
import { cn } from "../ui/utils";

export type FlowStep = { id: string; label: string; state: "done" | "current" | "upcoming" };

/**
 * Where you are in a short flow. Not interactive. Labels stay in the accessibility tree
 * below the sm breakpoint (visually hidden) so a screen reader never hears just "1 2 3 4".
 */
export function FlowProgress({ steps, label }: { steps: FlowStep[]; label: string }) {
  return (
    <nav aria-label={label}>
      <ol className="flex items-center gap-2">
        {steps.map((s, i) => (
          <li
            key={s.id}
            aria-current={s.state === "current" ? "step" : undefined}
            className="flex items-center gap-2"
          >
            <span
              aria-hidden="true"
              className={cn(
                "flex size-6 items-center justify-center rounded-full border text-xs font-medium",
                s.state === "current" && "border-primary bg-primary/10 text-foreground",
                s.state === "done" && "border-border-strong bg-muted text-foreground",
                s.state === "upcoming" && "border-border text-muted-foreground",
              )}
            >
              {s.state === "done" ? <Check className="size-3.5" /> : i + 1}
            </span>
            <span
              className={cn(
                "sr-only sm:not-sr-only text-sm",
                s.state === "current" ? "font-medium text-foreground" : "text-muted-foreground",
              )}
            >
              {s.label}
            </span>
            {i < steps.length - 1 && <span aria-hidden="true" className="hidden h-px w-6 bg-border sm:block" />}
          </li>
        ))}
      </ol>
    </nav>
  );
}
