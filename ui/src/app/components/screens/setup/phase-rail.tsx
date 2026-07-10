/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Phased vertical rail (brief §7.1) — full labels always visible, per-step live
// badge, phase progress. The corporate phase collapses into one group row until
// expanded. Ported from docs/design/figma-make-onboarding/src/components/setup/PhaseRail.tsx
// onto the real (frozen) step ids/labels in ./steps. Pure presentational: the
// caller (setup-screen orchestrator) computes badges/done via stepBadges/stepDone.
import { useState } from "react";
import { Check, ChevronDown, ChevronRight } from "lucide-react";
import { cn } from "../../ui/utils";
import { PHASES, STEP_LABEL, STEP_ORDER, type SetupStepId, type StepBadge } from "./steps";

const TONE_DOT: Record<StepBadge["tone"], string> = {
  success: "text-success",
  warning: "text-warning",
  neutral: "text-muted-foreground",
  info: "text-info",
};

export function PhaseRail({
  current,
  badges,
  done,
  onSelect,
}: {
  current: SetupStepId;
  badges: Record<SetupStepId, StepBadge>;
  done: Record<SetupStepId, boolean>;
  onSelect: (step: SetupStepId) => void;
}) {
  // Auto-expand a collapsed phase if the current step lives inside it.
  const [expanded, setExpanded] = useState<Record<string, boolean>>({});

  return (
    <>
      {/* Compact icon rail — visible lg only (56px column) */}
      <div className="hidden lg:flex xl:hidden flex-col gap-2 items-center">
        {STEP_ORDER.map((stepId) => {
          const badge = badges[stepId];
          const isDone = done[stepId];
          const active = current === stepId;
          const label = `${STEP_LABEL[stepId]} — ${badge.text}`;
          return (
            <button
              key={stepId}
              onClick={() => onSelect(stepId)}
              aria-current={active ? "step" : undefined}
              title={label}
              className={cn(
                "flex size-8 items-center justify-center rounded-full border transition-colors",
                active ? "border-primary bg-primary/10" : "border-transparent hover:bg-muted",
              )}
            >
              <span
                className={cn(
                  "flex size-4 shrink-0 items-center justify-center rounded-full border",
                  isDone
                    ? "border-success bg-success text-white"
                    : cn("border-border-strong", TONE_DOT[badge.tone]),
                )}
              >
                {isDone && <Check className="size-3" aria-hidden />}
              </span>
              <span className="sr-only">{label}</span>
            </button>
          );
        })}
      </div>

      {/* Full rail — stacked above content on mobile, hidden at lg (the icon-rail
          band), back at xl+. */}
      <nav aria-label="Setup steps" className="flex flex-col gap-5 lg:hidden xl:flex">
        {PHASES.map((phase) => {
          // done[] never marks credentials true (honesty law in steps.ts), so a
          // phase containing it ("Your work") caps below N/N by design — the
          // counter can't reach full and that's intentional, not a bug.
          const doneCount = phase.steps.filter((id) => done[id]).length;
          const isOpen =
            !phase.collapsible || expanded[phase.id] || phase.steps.includes(current);

          return (
            <div key={phase.id}>
              <div className="mb-2 flex items-center justify-between gap-2">
                {phase.collapsible ? (
                  <button
                    onClick={() => setExpanded((e) => ({ ...e, [phase.id]: !isOpen }))}
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
                  {phase.collapsible ? "all optional" : `${doneCount}/${phase.steps.length}`}
                </span>
              </div>

              {isOpen && (
                <ul className="flex flex-col gap-1">
                  {phase.steps.map((stepId) => {
                    const badge = badges[stepId];
                    const isDone = done[stepId];
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
                              isDone
                                ? "border-success bg-success text-white"
                                : cn("border-border-strong", TONE_DOT[badge.tone]),
                            )}
                          >
                            {isDone && <Check className="size-3" aria-hidden />}
                          </span>
                          <span className="min-w-0 flex-1">
                            <span
                              className={cn(
                                "block text-sm",
                                active ? "text-foreground" : "text-foreground/90",
                              )}
                            >
                              {STEP_LABEL[stepId]}
                            </span>
                            <span className={cn("block text-xs", TONE_DOT[badge.tone])}>
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
