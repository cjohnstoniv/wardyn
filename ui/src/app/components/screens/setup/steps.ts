/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Pure data layer for the Getting Started funnel. Holds the frozen step
// ids/labels (e2e tests target them), the phase grouping the rail renders, and
// the honest per-step badge/done derivation the orchestrator reads. Two
// badge-semantics deltas from the Figma Make design are folded in (see the
// workspaces/credentials cases). No React here by design — data/derivation only.
import type { SetupStatus, SiteConfig, Workspace } from "../../../lib/types";
import type { Readiness } from "../onboarding/intro";

// ------------------------------------------------------------
// Steps — ids/labels FROZEN (e2e tests target them). The single source of truth
// for the step contract; the orchestrator imports these rather than redefining.
// ------------------------------------------------------------
export type SetupStepId =
  | "environment"
  | "provider"
  | "host_proxy"
  | "scm_provider"
  | "artifact_repo"
  | "workspaces"
  | "credentials"
  | "review"
  | "launch";

export const SETUP_STEPS: { id: SetupStepId; label: string }[] = [
  { id: "environment", label: "Environment" },
  { id: "provider", label: "Model/Harness Provider" },
  { id: "host_proxy", label: "Host Proxy" },
  { id: "scm_provider", label: "SCM Provider" },
  { id: "artifact_repo", label: "Artifact Redirect" },
  { id: "workspaces", label: "Workspaces" },
  { id: "credentials", label: "Credentials" },
  { id: "review", label: "Review" },
  { id: "launch", label: "Launch" },
];

// id→label lookup — the rail and the layout footer both need it; export once
// here instead of each rebuilding the same Object.fromEntries map (F5).
export const STEP_LABEL: Record<SetupStepId, string> = Object.fromEntries(
  SETUP_STEPS.map((s) => [s.id, s.label]),
) as Record<SetupStepId, string>;

export const STEP_HEADING: Record<SetupStepId, string> = {
  environment: "Pick your barrier",
  provider: "Connect a model or agent harness",
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

export const PHASES: PhaseDef[] = [
  { id: "essentials", label: "Essentials", steps: ["environment", "provider"] },
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

// Steps that render an "Optional" chip in the shell (everything outside the two
// Essentials and two Finish steps). Exported so the layout and its test share
// one list instead of each hardcoding the same membership.
export const OPTIONAL_STEPS = new Set<SetupStepId>([
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
// Redirect): always non-blocking (B8-style) — these backend checks are
// hardcoded "info"-tier (see hostProxyCheck/scm_provider/artifactRepoCheck in
// internal/api/setup.go), never "ok", so a check.status==='ok' read could
// NEVER show "Configured" even once the operator had wired it up (M21). The
// badge instead derives readiness client-side from the actual SiteConfig field
// each step's own body edits — the honest default stays a neutral "Optional"
// nudge until that field is genuinely set.
export function siteConfigBadge(
  cfg: SiteConfig | null,
  checkId: "host_proxy" | "scm_provider" | "artifact_repo",
): StepBadge {
  const configured =
    checkId === "host_proxy"
      ? !!cfg?.upstream_proxy_secret_ref
      : checkId === "scm_provider"
        ? !!cfg?.scm_hosts?.length
        : !!cfg?.artifact_overrides && Object.keys(cfg.artifact_overrides).length > 0;
  return configured
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
  return {
    environment: r.barrierReady
      ? { text: `Ready · ${r.barrierCount} of 3 barriers`, tone: "success" }
      : { text: "Needs setup", tone: "warning" },
    provider: r.llmReady
      ? { text: r.llmLabel ? `Ready · ${r.llmLabel}` : "Ready", tone: "success" }
      : { text: "Needs setup", tone: "warning" },
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
    // a per-topic nag (those live on their own steps now).
    // "Essentials" = a barrier AND a connected model — the same pair canLaunch
    // and the fast-path banner gate on. Backend `ready` alone is barrier-only
    // (setup.go), so it must never claim launch-readiness by itself.
    review: status.checks.some((c) => c.status === "fail")
      ? { text: "Needs attention", tone: "warning" }
      : r.ready && r.llmReady
        ? { text: "All essentials ready", tone: "success" }
        : { text: "Review what's left", tone: "neutral" },
    launch: status.has_runs
      ? { text: "First run launched", tone: "success" }
      : r.ready && r.llmReady
        ? { text: "Ready to launch", tone: "success" }
        : { text: "Set up the essentials first", tone: "neutral" },
  };
}

export function stepDone(
  status: SetupStatus,
  r: Readiness,
  workspaces: Workspace[],
): Record<SetupStepId, boolean> {
  return {
    environment: r.barrierReady && !status.checks.some((c) => c.status === "fail"),
    provider: r.llmReady,
    // Non-blocking, so "done" is purely cosmetic — true only once the backend
    // itself reports the check as "ok" (never inferred client-side).
    host_proxy: status.checks.find((c) => c.id === "host_proxy")?.status === "ok",
    scm_provider: status.checks.find((c) => c.id === "scm_provider")?.status === "ok",
    artifact_repo: status.checks.find((c) => c.id === "artifact_repo")?.status === "ok",
    // Design delta: done only once a workspace is actually READY, matching the
    // badge above — merely onboarding one (still scanning/building/verifying)
    // no longer earns the stepper checkmark.
    workspaces: workspaces.some((w) => w.status === "ready"),
    // Honesty law: credentials is hard-pinned to false, full stop. This step is
    // advisory-only — a git PAT/GitHub App never earns it a checkmark, so it can
    // never visually imply it's required or that it gates readiness.
    credentials: false,
    review: r.ready && r.llmReady && !status.checks.some((c) => c.status === "fail"),
    launch: status.has_runs,
  };
}
