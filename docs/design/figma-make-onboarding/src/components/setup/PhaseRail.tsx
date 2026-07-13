/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { useState } from "react";
import { Check, ChevronDown, ChevronRight } from "lucide-react";
import { cn } from "../ui/utils";
import {
  Tooltip,
  TooltipContent,
  TooltipTrigger,
} from "../ui/tooltip";
import {
  PHASES,
  STEPS,
  deriveBadge,
  phaseProgress,
  type BadgeTone,
  type StepId,
} from "../../data/steps";
import type { SetupStatus } from "../../data/setupFixtures";

const TONE_DOT: Record<BadgeTone, string> = {
  ready: "text-success",
  "needs-setup": "text-warning",
  optional: "text-muted-foreground",
  warning: "text-warning",
  info: "text-info",
};

// Phased vertical rail (brief §7.1) — full labels always visible, per-step live badge, phase
// progress. The corporate phase collapses into one group row until expanded. Replaces the
// unreadable 9-chip horizontal stepper.
export function PhaseRail({
  status,
  current,
  onSelect,
}: {
  status: SetupStatus;
  current: StepId;
  onSelect: (step: StepId) => void;
}) {
  // Auto-expand a collapsed phase if the current step lives inside it.
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});

  return (
    <>
      {/* Compact icon rail — visible lg only (56px column) */}
      <div className="hidden lg:flex xl:hidden flex-col gap-2 items-center">
        {PHASES.map((phase) =>
          phase.steps.map((stepId) => {
            const badge = deriveBadge(stepId, status);
            const active = current === stepId;
            return (
              <Tooltip key={stepId}>
                <TooltipTrigger asChild>
                  <button
                    onClick={() => onSelect(stepId)}
                    aria-current={active ? "step" : undefined}
                    className={cn(
                      "flex size-8 items-center justify-center rounded-full border transition-colors",
                      active ? "border-primary bg-primary/10" : "border-transparent hover:bg-muted",
                    )}
                  >
                    <span
                      className={cn(
                        "flex size-4 shrink-0 items-center justify-center rounded-full border",
                        badge.done
                          ? "border-success bg-success text-white"
                          : cn("border-border-strong", TONE_DOT[badge.tone]),
                      )}
                    >
                      {badge.done && <Check className="size-3" aria-hidden />}
                    </span>
                  </button>
                </TooltipTrigger>
                <TooltipContent side="right">
                  {STEPS[stepId].label} — {badge.text}
                </TooltipContent>
              </Tooltip>
            );
          })
        )}
      </div>

      {/* Full rail — visible xl+ */}
    <nav aria-label="Setup steps" className="hidden xl:flex flex-col gap-5">
      {PHASES.map((phase) => {
        const { done, total } = phaseProgress(phase, status);
        const isOpen =
          !phase.collapsible ||
          expanded[phase.id] ||
          phase.steps.includes(current);

        return (
          <div key={phase.id}>
            <div className="mb-2 flex items-center justify-between gap-2">
              {phase.collapsible ? (
                <button
                  onClick={() =>
                    setExpanded((e) => ({ ...e, [phase.id]: !isOpen }))
                  }
                  className="inline-flex items-center gap-1 text-xs uppercase tracking-wide text-muted-foreground hover:text-foreground"
                >
                  {isOpen ? (
                    <ChevronDown className="size-3.5" aria-hidden />
                  ) : (
                    <ChevronRight className="size-3.5" aria-hidden />
                  )}
                  {phase.label}
                </button>
              ) : (
                <span className="text-xs uppercase tracking-wide text-muted-foreground">
                  {phase.label}
                </span>
              )}
              <span className="text-xs text-muted-foreground">
                {phase.collapsible ? "all optional" : `${done}/${total}`}
              </span>
            </div>

            {isOpen && (
              <ul className="flex flex-col gap-1">
                {phase.steps.map((stepId) => {
                  const step = STEPS[stepId];
                  const badge = deriveBadge(stepId, status);
                  const active = current === stepId;
                  return (
                    <li key={stepId}>
                      <button
                        onClick={() => onSelect(stepId)}
                        aria-current={active ? "step" : undefined}
                        className={cn(
                          "group flex w-full items-start gap-2.5 rounded-lg border px-3 py-2 text-left transition-colors",
                          active
                            ? "border-primary/50 bg-primary/10"
                            : "border-transparent hover:bg-muted",
                        )}
                      >
                        <span
                          className={cn(
                            "mt-0.5 flex size-4 shrink-0 items-center justify-center rounded-full border",
                            badge.done
                              ? "border-success bg-success text-white"
                              : cn("border-border-strong", TONE_DOT[badge.tone]),
                          )}
                        >
                          {badge.done && <Check className="size-3" aria-hidden />}
                        </span>
                        <span className="min-w-0 flex-1">
                          <span
                            className={cn(
                              "block text-sm",
                              active ? "text-foreground" : "text-foreground/90",
                            )}
                          >
                            {step.label}
                          </span>
                          <span
                            className={cn("block text-xs", TONE_DOT[badge.tone])}
                          >
                            {badge.text}
                          </span>
                        </span>
                      </button>
                    </li>
                  );
                })}
              </ul>
            )}
          </div>
        );
      })}
    </nav>
    </>
  );
}
