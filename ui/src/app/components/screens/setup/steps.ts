/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Pure data layer for the Getting Started funnel. Holds the frozen step
// ids/labels (e2e tests target them), the phase grouping the rail renders, and
// the honest per-step badge/done derivation the orchestrator reads. Two
// badge-semantics deltas from the design spec are folded in (see the
// workspaces/credentials cases). No React here by design — data/derivation only.
import type { SetupStatus, SiteConfig, Workspace } from "../../../lib/types";
import type { Readiness } from "../onboarding/intro";
import { DEMOS } from "../demos/demo-catalog";

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

export type SetupStepId =
  | "environment"
  | "provider"
  | DemoStepId
  | "host_proxy"
  | "scm_provider"
  | "artifact_repo"
  | "workspaces"
  | "credentials"
  | "review"
  | "launch";

// demo id → title, from the catalog (single source of truth for the demo steps'
// labels + headings, so they can't drift from what the demo pages show).
const DEMO_TITLES = Object.fromEntries(DEMOS.map((d) => [d.id, d.title])) as Record<DemoStepId, string>;

// id→label lookup — the rail and the layout footer both need it; export once
// here instead of each rebuilding the same map (F5).
export const STEP_LABEL: Record<SetupStepId, string> = {
  environment: "Environment",
  provider: "Model/Harness Provider",
  ...DEMO_TITLES,
  host_proxy: "Host Proxy",
  scm_provider: "SCM Provider",
  artifact_repo: "Artifact Redirect",
  workspaces: "Workspaces",
  credentials: "Credentials",
  review: "Review",
  launch: "Launch",
};

export const STEP_HEADING: Record<SetupStepId, string> = {
  environment: "Pick your barrier",
  provider: "Connect a model or agent harness",
  ...DEMO_TITLES,
  host_proxy: "Corporate host proxy",
  scm_provider: "Source control provider",
  artifact_repo: "Artifact registry redirection",
  workspaces: "Onboard a workspace",
  credentials: "Repo & cloud credentials",
  review: "Review readiness",
  launch: "Launch your first run",
};

// ------------------------------------------------------------
// Phases (redesign) — groups the FROZEN steps above for the collapsible funnel
// layout. Translated 1:1 from the design's PHASES onto the real ids above.
// ------------------------------------------------------------
export interface PhaseDef {
  id: string;
  label: string;
  steps: SetupStepId[];
  /** Corporate phase collapses into one group row until expanded. */
  collapsible?: boolean;
}

// Walk order: essentials → demos → corporate network → your work → finish.
// After the essentials (barrier + model), Demos lets a first-timer SEE Wardyn
// govern a throwaway sandbox before investing in their own repos; the niche
// corporate-network steps (a quick opt-out for everyone else) come next so a
// corporate operator wires proxy/mirror BEFORE onboarding workspaces; then their
// actual work. Step ids/labels above remain frozen — only the phase composition
// (and therefore STEP_ORDER) moves.
export const PHASES: PhaseDef[] = [
  { id: "essentials", label: "Essentials", steps: ["environment", "provider"] },
  { id: "demos", label: "Demos", steps: [...DEMO_STEP_IDS] },
  {
    id: "corporate",
    label: "Corporate network",
    steps: ["host_proxy", "artifact_repo"],
    collapsible: true,
  },
  { id: "work", label: "Your work", steps: ["scm_provider", "workspaces", "credentials"] },
  { id: "finish", label: "Finish", steps: ["review", "launch"] },
];

export const STEP_ORDER: SetupStepId[] = PHASES.flatMap((p) => p.steps);

// First step of the phase AFTER the given phase id (or null if it's the last) —
// powers the "skip this section" control for the collapsible corporate phase.
export function nextPhaseFirstStep(phaseId: string): SetupStepId | null {
  const i = PHASES.findIndex((p) => p.id === phaseId);
  return i >= 0 ? (PHASES[i + 1]?.steps[0] ?? null) : null;
}

// Steps that render an "Optional" chip in the shell (everything outside the two
// Essentials and two Finish steps). Exported so the layout and its test share
// one list instead of each hardcoding the same membership.
export const OPTIONAL_STEPS = new Set<SetupStepId>([
  // A model/harness provider is OPTIONAL — it's only needed for the AI Composer
  // or a managed agent harness. A run works with no model (you drive it, or bring
  // your own container), so the barrier (Environment) is the sole hard requirement.
  "provider",
  ...DEMO_STEP_IDS,
  "host_proxy",
  "scm_provider",
  "artifact_repo",
  "workspaces",
  "credentials",
]);

// ------------------------------------------------------------
// Honest per-step badges (B4) — reflect reality, never a false "Done".
// stepBadges / stepDone / siteConfigBadge carry two design deltas (see the
// workspaces/credentials cases below for exactly what they are and why).
// ------------------------------------------------------------
export type StepBadge = { text: string; tone: "success" | "warning" | "neutral" | "info" };

// Badge for a corporate-baseline step (Host Proxy / SCM Provider / Artifact
// Redirect): always non-blocking (B8-style). host_proxy/artifact_repo are
// hardcoded "info"-tier; scm_provider is GRADED (ok for a GitHub App, warn for
// a standing ssh-key-* secret, info otherwise — scmProviderCheck in
// internal/api/setup.go) but still never gates readiness. The badge derives
// readiness client-side from the actual SiteConfig field each step's own body
// edits — the honest default stays a neutral "Optional" nudge until that
// field is genuinely set; the graded check row carries the safety framing.
export function siteConfigConfigured(
  cfg: SiteConfig | null,
  checkId: "host_proxy" | "scm_provider" | "artifact_repo",
): boolean {
  return checkId === "host_proxy"
    ? !!cfg?.upstream_proxy_secret_ref
    : checkId === "scm_provider"
      ? !!cfg?.scm_hosts?.length
      : !!cfg?.artifact_overrides && Object.keys(cfg.artifact_overrides).length > 0;
}

export function siteConfigBadge(
  cfg: SiteConfig | null,
  checkId: "host_proxy" | "scm_provider" | "artifact_repo",
): StepBadge {
  return siteConfigConfigured(cfg, checkId)
    ? { text: "Configured", tone: "success" }
    : { text: "Optional", tone: "neutral" };
}

export function stepBadges(
  status: SetupStatus,
  r: Readiness,
  workspaces: Workspace[],
  siteConfig: SiteConfig | null,
): Record<SetupStepId, StepBadge> {
  const readyWorkspaces = workspaces.filter((w) => w.status === "ready").length;
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
    provider: r.llmReady
      ? { text: r.llmLabel ? `Ready · ${r.llmLabel}` : "Ready", tone: "success" }
      : { text: "Optional", tone: "neutral" },
    ...demoBadges,
    host_proxy: siteConfigBadge(siteConfig, "host_proxy"),
    scm_provider: siteConfigBadge(siteConfig, "scm_provider"),
    artifact_repo: siteConfigBadge(siteConfig, "artifact_repo"),
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
    credentials: { text: "Optional", tone: "neutral" },
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
  siteConfig: SiteConfig | null,
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
    provider: r.llmReady,
    ...demoDone,
    // Corporate baseline: done derives from the SAME SiteConfig predicate as the
    // badge (siteConfigConfigured), so the green "Configured" badge and the rail
    // checkmark can never disagree. Still honest — the value read is the actual
    // saved config the step's own body edits, not an inferred guess.
    host_proxy: siteConfigConfigured(siteConfig, "host_proxy"),
    scm_provider: siteConfigConfigured(siteConfig, "scm_provider"),
    artifact_repo: siteConfigConfigured(siteConfig, "artifact_repo"),
    // Design delta: done only once a workspace is actually READY, matching the
    // badge above — merely onboarding one (still scanning/building/verifying)
    // no longer earns the stepper checkmark.
    workspaces: workspaces.some((w) => w.status === "ready"),
    // Honesty law: credentials is hard-pinned to false, full stop. This step is
    // advisory-only — a git PAT/GitHub App never earns it a checkmark, so it can
    // never visually imply it's required or that it gates readiness.
    credentials: false,
    // Barrier is the only hard requirement; a model is optional (skippable), so
    // Review is done once the barrier is up and no check is failing.
    review: r.ready && !status.checks.some((c) => c.status === "fail"),
    launch: status.has_runs,
  };
}
