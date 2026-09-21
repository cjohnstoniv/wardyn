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
  CONFIG_STEPS,
  DEMO_EGRESS_IDS,
  DEMO_SECRETS_IDS,
  REQUIRED_STEPS,
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

// One item's dot + label + badge — shared by every group below (and by the
// icon-only compact rail's own render, separately). `num` prints a small
// ordinal ahead of the dot for the Required group only (1-4); every other
// group's items carry none, matching the prototype's own railItem (a step
// off the required walk "is not on the way anywhere"). aria-hidden: the
// ordinal is decorative sequencing, not part of the button's accessible name
// — e2e clicks `getByRole("button", { name: /^People/ })`, and a leading "2"
// text node would break that anchor.
function RailItem({
  stepId,
  active,
  isDone,
  isVisited,
  badge,
  num,
  onSelect,
  refusal,
}: {
  stepId: SetupStepId;
  active: boolean;
  isDone: boolean;
  isVisited: boolean;
  badge: StepBadge;
  num?: number;
  onSelect: (step: SetupStepId) => void;
  refusal?: string;
}) {
  return (
    <li>
      <button
        onClick={() => onSelect(stepId)}
        aria-current={active ? "step" : undefined}
        disabled={!!refusal}
        title={refusal}
        className={cn(
          "group flex w-full items-start gap-2.5 rounded-lg border px-3 py-2 text-left transition-colors",
          active ? "border-primary/50 bg-primary/10" : "border-transparent hover:bg-muted",
          refusal && "opacity-50",
        )}
      >
        {num !== undefined && (
          <span aria-hidden className="mt-0.5 w-3.5 shrink-0 text-center text-xs text-muted-foreground">
            {num}
          </span>
        )}
        <span
          data-visited={isVisited || undefined}
          className={cn(
            "mt-0.5 flex size-4 shrink-0 items-center justify-center rounded-full border",
            isDone
              ? "border-success bg-success text-success-foreground"
              : isVisited
                ? "border-border-strong bg-muted-foreground/40 text-muted-foreground"
                : cn("border-border-strong", TONE_DOT[badge.tone]),
          )}
        >
          {isDone && <Check className="size-3" aria-hidden />}
        </span>
        <span className="min-w-0 flex-1">
          <span className={cn("block text-sm", active ? "text-foreground" : "text-foreground/90")}>
            {STEP_LABEL[stepId]}
          </span>
          <span className={cn("block text-xs", TONE_DOT[badge.tone])}>{badge.text}</span>
        </span>
      </button>
    </li>
  );
}

export function PhaseRail({
  current,
  badges,
  done,
  onSelect,
  order = STEP_ORDER,
  refuseNext,
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
  // F3-F3: the SAME crossing predicate the footer's Next button already
  // renders disabled+titled (setup-screen.tsx's refuseSelect) — without this a
  // rail click past an ungated corp_network read as a live, clickable step
  // whose onSelect just silently no-ops (a dead click, not a disabled one).
  // Undefined means "nothing is refused" (every existing caller/test).
  refuseNext?: (next: SetupStepId) => string | undefined;
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
          const refusal = refuseNext?.(stepId);
          const label = `${STEP_LABEL[stepId]} — ${badge.text}`;
          return (
            <button
              key={stepId}
              onClick={() => onSelect(stepId)}
              aria-current={active ? "step" : undefined}
              disabled={!!refusal}
              title={refusal ?? label}
              className={cn(
                "flex size-8 items-center justify-center rounded-full border transition-colors",
                active ? "border-primary bg-primary/10" : "border-transparent hover:bg-muted",
                refusal && "opacity-50",
              )}
            >
              <span
                data-visited={isVisited || undefined}
                className={cn(
                  "flex size-4 shrink-0 items-center justify-center rounded-full border",
                  isDone
                    ? "border-success bg-success text-success-foreground"
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
          band), back at xl+. #213 — three categories, not the old five phases:
          Required (numbered, what blocks a run), Optional setup (real
          configuration, unnumbered), Demos (unnumbered, split into its two
          existing sections so egress and secrets stay visually apart). Each
          list is only the walkable members of `order` — a phase left with
          none (Secrets demos on a host with no demo secret stored) renders
          nothing at all rather than an empty group heading over a 0. */}
      <nav aria-label="Setup steps" className="flex flex-col gap-5 lg:hidden xl:flex">
        {(() => {
          const item = (stepId: SetupStepId, num?: number) => {
            const badge = badges[stepId];
            const isDone = done[stepId];
            // A4: see the compact rail above for what this means.
            const isVisited = !isDone && badge.text === "Skipped";
            return (
              <RailItem
                key={stepId}
                stepId={stepId}
                num={num}
                active={current === stepId}
                isDone={isDone}
                isVisited={isVisited}
                badge={badge}
                onSelect={onSelect}
                refusal={refuseNext?.(stepId)}
              />
            );
          };
          const required = REQUIRED_STEPS.filter((id) => order.includes(id));
          const configSteps = CONFIG_STEPS.filter((id) => order.includes(id));
          const egressDemos = DEMO_EGRESS_IDS.filter((id) => order.includes(id));
          const secretsDemos = DEMO_SECRETS_IDS.filter((id) => order.includes(id));
          const demoCount = egressDemos.length + secretsDemos.length;

          return (
            <>
              {required.length > 0 && (
                <div>
                  <div className="mb-2 flex items-center justify-between gap-2">
                    <span className="text-xs uppercase tracking-wide text-muted-foreground">Required</span>
                    <span className="text-xs text-muted-foreground">· {required.length}</span>
                  </div>
                  <ul className="flex flex-col gap-1">
                    {required.map((stepId, i) => item(stepId, i + 1))}
                  </ul>
                </div>
              )}

              {configSteps.length > 0 && (
                <div>
                  <div className="mb-2 flex items-center justify-between gap-2">
                    <span className="text-xs uppercase tracking-wide text-muted-foreground">
                      Optional setup
                    </span>
                    <span className="text-xs text-muted-foreground">· {configSteps.length}</span>
                  </div>
                  <ul className="flex flex-col gap-1">{configSteps.map((stepId) => item(stepId))}</ul>
                </div>
              )}

              {demoCount > 0 && (
                <div>
                  <div className="mb-2 flex items-center justify-between gap-2">
                    <span className="text-xs uppercase tracking-wide text-muted-foreground">Demos</span>
                    <span className="text-xs text-muted-foreground">· {demoCount}</span>
                  </div>
                  {egressDemos.length > 0 && (
                    <>
                      <div className="label-eyebrow mb-1">
                        Egress
                      </div>
                      <ul className="mb-2 flex flex-col gap-1">{egressDemos.map((stepId) => item(stepId))}</ul>
                    </>
                  )}
                  {secretsDemos.length > 0 && (
                    <>
                      <div className="label-eyebrow mb-1">
                        Secrets
                      </div>
                      <ul className="flex flex-col gap-1">{secretsDemos.map((stepId) => item(stepId))}</ul>
                    </>
                  )}
                </div>
              )}
            </>
          );
        })()}
      </nav>
    </>
  );
}
