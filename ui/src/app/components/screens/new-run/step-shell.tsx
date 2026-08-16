/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The New Run wizard's step rail. Field/OptionCard/DomainPillList moved to
// components/wardyn/form-primitives.tsx — they outlive this wizard; this does not.
import * as React from "react";
import { Check } from "lucide-react";
import { cn } from "../../ui/utils";
import { WIZARD_STEPS, type WizardStepId } from "./wizard-types";

// Horizontal numbered step indicator with a connecting rail. The active step is
// primary-tinted; completed steps show a check. Generalized (steps? prop,
// defaulting to WIZARD_STEPS) so the first-run setup screen can reuse it with
// its own step id union without forking — the run-wizard call site (no `steps`
// prop) is unchanged and still infers T=WizardStepId from `current`.
export function StepIndicator<T extends string = WizardStepId>({
  current,
  onJump,
  steps = WIZARD_STEPS as unknown as { id: T; label: string }[],
}: {
  current: T;
  onJump?: (id: T) => void;
  steps?: { id: T; label: string }[];
}) {
  const currentIdx = steps.findIndex((s) => s.id === current);
  return (
    // gap-y widens on wrap: a 7-step rail in a width-capped dialog folds to a
    // second row instead of clipping steps past the panel edge.
    <ol className="flex flex-wrap items-center gap-x-1.5 gap-y-2">
      {steps.map((step, i) => {
        const state = i < currentIdx ? "done" : i === currentIdx ? "active" : "todo";
        const clickable = onJump && i <= currentIdx;
        // ui-newrun-2 / ui-wsWizard-7: state was color-only (no ARIA signal)
        // and the visible label is `hidden sm:inline` — removed from the
        // accessibility tree below sm, not just unpainted — with no fallback,
        // so a screen reader (at any width) or a narrow viewport (any state)
        // only ever heard/saw a bare ordinal. aria-current names the active
        // step for landmark/step navigation; aria-label carries the step name
        // AND state on the button itself, so the accessible name never
        // depends on the collapsible span at lines below.
        const stateSuffix = state === "active" ? " — current step" : state === "done" ? " — completed" : "";
        return (
          <React.Fragment key={step.id}>
            <li>
              <button
                type="button"
                disabled={!clickable}
                onClick={() => clickable && onJump?.(step.id)}
                aria-current={state === "active" ? "step" : undefined}
                aria-label={`${step.label}${stateSuffix}`}
                className={cn(
                  "flex items-center gap-2 rounded-md px-1.5 py-1 text-xs transition-colors",
                  clickable && "hover:bg-accent",
                  !clickable && "cursor-default",
                )}
              >
                <span
                  className={cn(
                    "flex size-7 shrink-0 items-center justify-center rounded-full border text-[0.6875rem] font-semibold",
                    state === "active" && "border-primary bg-primary text-primary-foreground",
                    state === "done" && "border-primary bg-primary text-primary-foreground",
                    state === "todo" && "border-border text-muted-foreground",
                  )}
                >
                  {state === "done" ? <Check className="size-3" /> : i + 1}
                </span>
                <span
                  className={cn(
                    "hidden font-medium sm:inline",
                    state === "active" ? "text-foreground" : "text-muted-foreground",
                  )}
                >
                  {step.label}
                </span>
              </button>
            </li>
            {i < steps.length - 1 && (
              <li
                aria-hidden
                className={cn(
                  "h-0.5 w-3 shrink-0 sm:w-5",
                  i < currentIdx ? "bg-primary" : "bg-border",
                )}
              />
            )}
          </React.Fragment>
        );
      })}
    </ol>
  );
}
