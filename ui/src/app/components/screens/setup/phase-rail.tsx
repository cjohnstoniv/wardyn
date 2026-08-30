/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Phased vertical rail (brief §7.1) — full labels always visible, per-step live
// badge, phase progress. Derived from the deleted Figma Make onboarding
// snapshot's PhaseRail onto the real (frozen) step ids/labels in ./steps. Pure presentational: the caller
// (setup-screen orchestrator) computes badges/done via stepBadges/stepDone.
import { Check } from "lucide-react";
import { cn } from "../../ui/utils";
import {
  OPTIONAL_STEPS,
  PHASES,
  STEP_LABEL,
  STEP_ORDER,
  type SetupStepId,
  type StepBadge,
} from "./steps";

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
  order = STEP_ORDER,
}: {
  current: SetupStepId;
  badges: Record<SetupStepId, StepBadge>;
  done: Record<SetupStepId, boolean>;
  onSelect: (step: SetupStepId) => void;
  // The steps actually walkable right now (steps.ts's stepOrder(status)) — a
  // demo whose precondition is unmet is dropped from it, and the rail must not
  // offer a step whose Start is closed. Defaults to the full contract order,
  // which is also what the selector returns before status lands.
  order?: SetupStepId[];
}) {
  return (
    <>
      {/* Compact icon rail — visible lg only (56px column) */}
      <nav aria-label="Setup steps" className="hidden lg:flex xl:hidden flex-col gap-2 items-center">
        {order.map((stepId) => {
          const badge = badges[stepId];
          const isDone = done[stepId];
          // A4: visited-without-configuring (see setup-screen's Skipped override)
          // reads as a muted dot — distinct from both the untouched tone-outline
          // ring and the green done checkmark. isDone still wins outright (an
          // explicitly-skipped model step earns its checkmark elsewhere).
          const isVisited = !isDone && badge.text === "Skipped";
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
                data-visited={isVisited || undefined}
                className={cn(
                  "flex size-4 shrink-0 items-center justify-center rounded-full border",
                  isDone
                    ? "border-success bg-success text-white"
                    : isVisited
                      ? "border-border-strong bg-muted-foreground/40 text-muted-foreground"
                      : cn("border-border-strong", TONE_DOT[badge.tone]),
                )}
              >
                {isDone && <Check className="size-3" aria-hidden />}
              </span>
              <span className="sr-only">{label}</span>
            </button>
          );
        })}
      </nav>

      {/* Full rail — stacked above content on mobile, hidden at lg (the icon-rail
          band), back at xl+. */}
      <nav aria-label="Setup steps" className="flex flex-col gap-5 lg:hidden xl:flex">
        {PHASES.map((phase) => {
          // Only the walkable members (see `order`). A phase left with none —
          // Secrets demos on a host with no demo secret stored — renders
          // nothing at all rather than an empty group heading with a 0/0.
          const steps = phase.steps.filter((id) => order.includes(id));
          if (steps.length === 0) return null;
          // A phase made only of optional steps reads "all optional" instead of a
          // progress counter: credentials is done-pinned false (honesty law in
          // steps.ts), so "Your work" would show a counter that structurally can
          // never reach N/N. Per-step dots still track real progress inside it.
          const allOptional = steps.every((id) => OPTIONAL_STEPS.has(id));
          const doneCount = steps.filter((id) => done[id]).length;

          return (
            <div key={phase.id}>
              <div className="mb-2 flex items-center justify-between gap-2">
                <span className="text-xs uppercase tracking-wide text-muted-foreground">
                  {phase.label}
                </span>
                <span className="text-xs text-muted-foreground">
                  {allOptional ? "all optional" : `${doneCount}/${steps.length}`}
                </span>
              </div>

              <ul className="flex flex-col gap-1">
                {steps.map((stepId) => {
                  const badge = badges[stepId];
                  const isDone = done[stepId];
                  // A4: see the compact rail above for what this means.
                  const isVisited = !isDone && badge.text === "Skipped";
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
                          data-visited={isVisited || undefined}
                          className={cn(
                            "mt-0.5 flex size-4 shrink-0 items-center justify-center rounded-full border",
                            isDone
                              ? "border-success bg-success text-white"
                              : isVisited
                                ? "border-border-strong bg-muted-foreground/40 text-muted-foreground"
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
            </div>
          );
        })}
      </nav>
    </>
  );
}
