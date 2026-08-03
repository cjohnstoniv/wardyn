/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Pure data layer for the Getting Started funnel. Holds the frozen step
// ids/labels (e2e tests target them), the phase grouping the rail renders, and
// the honest per-step badge/done derivation the orchestrator reads. One
// badge-semantics delta from the design spec is folded in (see the workspaces
// case below). No React here by design — data/derivation only.
import type { SetupStatus, Workspace } from "../../../lib/types";
import type { Readiness } from "../onboarding/intro";
import { DEMOS } from "../demos/demo-catalog";
import { isUsable } from "../../../lib/workspace-status";

// ------------------------------------------------------------
// Steps — ids/labels FROZEN (e2e tests target them). The single source of truth
// for the step contract; the orchestrator imports these rather than redefining.
// ------------------------------------------------------------
// The four hands-on demos are each their own funnel sub-step under "Demos" (so
// they render as separate items in the rail). Ids mirror the demo catalog so the
// orchestrator resolves a step's demo by id. FROZEN with the rest of the contract.
export const DEMO_STEP_IDS = [
  "sealed-box",
  "fail-then-approve",
  "held-at-the-door",
  "lines-that-cant-be-crossed",
] as const;
export type DemoStepId = (typeof DEMO_STEP_IDS)[number];

// 13 -> 9 collapse: `provider` (the two-level harness picker), `host_proxy`,
// `scm_provider`, `artifact_repo`, and `credentials` are GONE — their
// configuration now lives on /integrations (see ../integrations). `integrations`
// is the one new step: an embedded, thin view of that same page, so Getting
// Started never forks a second copy of that configuration surface.
export type SetupStepId = "environment" | "integrations" | DemoStepId | "workspaces" | "review" | "launch";

// demo id → title, from the catalog (single source of truth for the demo steps'
// labels + headings, so they can't drift from what the demo pages show). Scoped
// to the FROZEN four funnel demo steps — the catalog also carries the harness
// demo (agent-in-the-box, /demos-only, gated on a connected model), which is NOT
// a Getting-started step and must never enter STEP_LABEL/STEP_HEADING/STEP_ORDER.
const DEMO_TITLES = Object.fromEntries(
  DEMOS.filter((d) => (DEMO_STEP_IDS as readonly string[]).includes(d.id)).map((d) => [d.id, d.title]),
) as Record<DemoStepId, string>;

// id→label lookup — the rail and the layout footer both need it; export once
// here instead of each rebuilding the same map (F5).
export const STEP_LABEL: Record<SetupStepId, string> = {
  environment: "Environment",
  integrations: "Integrations",
  ...DEMO_TITLES,
  workspaces: "Workspaces",
  review: "Review",
  launch: "Launch",
};

export const STEP_HEADING: Record<SetupStepId, string> = {
  environment: "Pick your barrier",
  integrations: "Connect what's outside Wardyn",
  ...DEMO_TITLES,
  workspaces: "Onboard a workspace",
  review: "Review readiness",
  launch: "Launch your first run",
};

// ------------------------------------------------------------
// Phases (redesign) — groups the FROZEN steps above for the collapsible funnel
// layout. Translated 1:1 from the design's PHASES9 onto the real ids above.
// ------------------------------------------------------------
export interface PhaseDef {
  id: string;
  label: string;
  steps: SetupStepId[];
  /** Corporate phase collapses into one group row until expanded. */
  collapsible?: boolean;
}

// Walk order: essentials → demos → your work → finish. Integrations sits right
// after Environment in Essentials — it carries what used to be three separate
// prerequisite steps (host proxy / SCM host / artifact mirror) plus the model
// picker, and the same prerequisite reasoning that put those ahead of the model
// step still holds: a demo or a connected model needs egress, so an operator
// behind an unconfigured corporate proxy who meets that later reads an
// environmental failure as "Wardyn is broken". The conditional proxy-detected
// banner that used to be the host_proxy step's own check now lives in the
// embedded integrations list itself (T.PROXY_BANNER) — see integrations-step.tsx.
export const PHASES: PhaseDef[] = [
  { id: "essentials", label: "Essentials", steps: ["environment", "integrations"] },
  { id: "demos", label: "Demos", steps: [...DEMO_STEP_IDS] },
  { id: "work", label: "Your work", steps: ["workspaces"] },
  { id: "finish", label: "Finish", steps: ["review", "launch"] },
];

export const STEP_ORDER: SetupStepId[] = PHASES.flatMap((p) => p.steps);

// First step of the phase AFTER the given phase id (or null if it's the last) —
// powers the "skip this section" control for a collapsible phase. No phase is
// collapsible in the 9-step rail today; kept for the layout that reads it.
export function nextPhaseFirstStep(phaseId: string): SetupStepId | null {
  const i = PHASES.findIndex((p) => p.id === phaseId);
  return i >= 0 ? (PHASES[i + 1]?.steps[0] ?? null) : null;
}

// Steps that render an "Optional" chip in the shell (everything outside the two
// Essentials and two Finish steps). Exported so the layout and its test share
// one list instead of each hardcoding the same membership.
export const OPTIONAL_STEPS = new Set<SetupStepId>([
  // Integrations is OPTIONAL — every category it covers (model/harness, SCM
  // host, artifact mirror, host proxy) is itself skippable; Wardyn runs with
  // none of them connected. The barrier (Environment) is the sole hard
  // requirement.
  "integrations",
  ...DEMO_STEP_IDS,
  "workspaces",
]);

// ------------------------------------------------------------
// Honest per-step badges (B4) — reflect reality, never a false "Done".
// stepBadges/stepDone carry one design delta (see the workspaces case below).
// ------------------------------------------------------------
export type StepBadge = { text: string; tone: "success" | "warning" | "neutral" | "info" };

export function stepBadges(
  status: SetupStatus,
  r: Readiness,
  workspaces: Workspace[],
  // Count of connected integrations (any category) — see
  // lib/api/integrations.ts's allRows(). The orchestrator owns fetching the
  // integrations data (siteConfig + secret names) this count derives from;
  // this pure function only needs the resulting number.
  integrationsCount: number,
): Record<SetupStepId, StepBadge> {
  const readyWorkspaces = workspaces.filter((w) => isUsable(w.status)).length;
  // Each demo sub-step is a "try it" step. The pure badge stays advisory (neutral
  // "Optional"); the orchestrator upgrades a demo to a green "Done · demo run" once
  // it's been launched (a per-browser signal that doesn't belong in this pure fn).
  const demoBadges = Object.fromEntries(
    DEMO_STEP_IDS.map((id) => [id, { text: "Optional", tone: "neutral" } as StepBadge]),
  ) as Record<DemoStepId, StepBadge>;
  return {
    environment: r.barrierReady
      ? { text: `Ready · ${r.barrierCount} of 3 barriers`, tone: "success" }
      : { text: "Needs setup", tone: "warning" },
    // Ladder: Optional -> Skipped (visited, left unconfigured — applied by the
    // orchestrator's generic visited-steps override, see setup-screen.tsx) ->
    // Ready · N connected.
    integrations:
      integrationsCount > 0
        ? { text: `Ready · ${integrationsCount} connected`, tone: "success" }
        : { text: "Optional", tone: "neutral" },
    ...demoBadges,
    // Count only READY workspaces, not merely onboarded ones — a workspace stuck
    // mid-import isn't attachable to a run yet, so it earns its own honest "In
    // progress" state instead of a premature green "Ready · N onboarded".
    workspaces: readyWorkspaces
      ? {
          // Honest count: when some onboarded workspaces aren't ready yet, say so
          // ("2 of 5") instead of an undercounting "2 onboarded".
          text:
            readyWorkspaces === workspaces.length
              ? `Ready · ${readyWorkspaces} onboarded`
              : `Ready · ${readyWorkspaces} of ${workspaces.length} onboarded`,
          tone: "success",
        }
      : workspaces.length
        ? { text: "In progress", tone: "info" }
        : { text: "Optional", tone: "neutral" },
    // Review rolls up every check. It's "warning" only when a real blocker exists
    // (a failing check), else a neutral/green summary — the readiness verdict, not
    // a per-topic nag (those live on their own steps now). The one hard requirement
    // is the BARRIER (backend `ready`); a model is optional, so it never blocks the
    // "ready" verdict here (the fast-path banner still needs one — it advertises a
    // one-click run — but that's a nudge, not a gate).
    review: status.checks.some((c) => c.status === "fail")
      ? { text: "Needs attention", tone: "warning" }
      : r.ready
        ? { text: "Ready to launch", tone: "success" }
        : { text: "Set up the barrier first", tone: "neutral" },
    launch: status.has_runs
      ? { text: "First run launched", tone: "success" }
      : r.ready
        ? { text: "Ready to launch", tone: "success" }
        : { text: "Set up the barrier first", tone: "neutral" },
  };
}

export function stepDone(
  status: SetupStatus,
  r: Readiness,
  workspaces: Workspace[],
  integrationsCount: number,
): Record<SetupStepId, boolean> {
  // Demos: advisory here (all false). The orchestrator ORs in the per-browser
  // "launched demos" set to earn each demo's checkmark — kept out of this pure fn
  // so its signature (and every steps.test call site) stays unchanged.
  const demoDone = Object.fromEntries(DEMO_STEP_IDS.map((id) => [id, false])) as Record<
    DemoStepId,
    boolean
  >;
  return {
    // Environment = "Pick your barrier" — barrier-only, the same signal its badge
    // reads. An unrelated failing check must not blank this dot while the badge
    // stays green (Review owns the whole-checks rollup).
    environment: r.barrierReady,
    // An explicit "Skip this step" click (see setup-gate's markIntegrationsSkipped)
    // ORs in from the orchestrator, exactly like the old model-skip override —
    // this pure fn only knows about a real connected integration.
    integrations: integrationsCount > 0,
    ...demoDone,
    // Design delta: done only once a workspace is actually READY, matching the
    // badge above — merely onboarding one (still scanning/building/verifying)
    // no longer earns the stepper checkmark.
    workspaces: workspaces.some((w) => isUsable(w.status)),
    // Barrier is the only hard requirement; a model is optional (skippable), so
    // Review is done once the barrier is up and no check is failing.
    review: r.ready && !status.checks.some((c) => c.status === "fail"),
    launch: status.has_runs,
  };
}
