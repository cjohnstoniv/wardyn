/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { Check } from "lucide-react";
import { cn } from "../ui/utils";

export type FlowStep = { id: string; label: string; state: "done" | "current" | "upcoming" };

/**
 * Where you are in a short flow, as Make draws it: "✓ Shape —— 2 Run —— 3 …" in small type, a green
 * check on finished steps, a number on the rest, hairline connectors between. Not interactive. Labels
 * stay in the accessibility tree below the sm breakpoint (visually hidden) so a screen reader never
 * hears just "1 2 3 4".
 */
export function FlowProgress({ steps, label }: { steps: FlowStep[]; label: string }) {
  return (
    <nav aria-label={label} className="max-w-[600px]">
      <ol className="flex items-center gap-2.5 text-meta text-muted-foreground">
        {steps.map((s, i) => (
          <li
            key={s.id}
            aria-current={s.state === "current" ? "step" : undefined}
            className={cn("flex items-center gap-2.5", i < steps.length - 1 && "flex-1")}
          >
            <span
              className={cn(
                "flex items-center gap-1.5 whitespace-nowrap",
                s.state === "current" && "text-foreground",
                s.state === "done" && "text-success",
              )}
            >
              <span aria-hidden="true">{s.state === "done" ? <Check className="size-3.5" /> : i + 1}</span>
              <span className="sr-only font-semibold sm:not-sr-only">{s.label}</span>
            </span>
            {i < steps.length - 1 && <span aria-hidden="true" className="h-px flex-1 bg-border" />}
          </li>
        ))}
      </ol>
    </nav>
  );
}
