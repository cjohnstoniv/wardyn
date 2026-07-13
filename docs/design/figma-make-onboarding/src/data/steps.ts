/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The 9 step ids and labels are FROZEN (e2e tests target them) — group and re-layout freely,
// but each keeps its identity and full label. Grouped into the four phases from brief §7.1.

import { hasConnectedModel, type SetupStatus } from "./setupFixtures";

export type StepId =
  | "environment"
  | "model"
  | "proxy"
  | "scm"
  | "artifacts"
  | "workspaces"
  | "credentials"
  | "review"
  | "launch";

export interface StepDef {
  id: StepId;
  /** Frozen full label — always rendered somewhere visible. */
  label: string;
  /** Short heading used in the content area. */
  heading: string;
}

export interface PhaseDef {
  id: string;
  label: string;
  steps: StepId[];
  /** Corporate phase collapses into one group row until expanded. */
  collapsible?: boolean;
}

export const STEPS: Record<StepId, StepDef> = {
  environment: { id: "environment", label: "Environment", heading: "Pick your barrier" },
  model: {
    id: "model",
    label: "Model/Harness Provider",
    heading: "Connect a model or agent harness",
  },
  proxy: { id: "proxy", label: "Host Proxy", heading: "Corporate host proxy" },
  scm: { id: "scm", label: "SCM Provider", heading: "Source control provider" },
  artifacts: {
    id: "artifacts",
    label: "Artifact Redirect",
    heading: "Artifact registry redirection",
  },
  workspaces: { id: "workspaces", label: "Workspaces", heading: "Onboard a workspace" },
  credentials: {
    id: "credentials",
    label: "Credentials",
    heading: "Repo & cloud credentials",
  },
  review: { id: "review", label: "Review", heading: "Review readiness" },
  launch: { id: "launch", label: "Launch", heading: "Launch your first run" },
};

export const PHASES: PhaseDef[] = [
  { id: "essentials", label: "Essentials", steps: ["environment", "model"] },
  {
    id: "corporate",
    label: "Corporate network",
    steps: ["proxy", "artifacts"],
    collapsible: true,
  },
  { id: "work", label: "Your work", steps: ["scm", "workspaces", "credentials"] },
  { id: "finish", label: "Finish", steps: ["review", "launch"] },
];

export const STEP_ORDER: StepId[] = PHASES.flatMap((p) => p.steps);

export type BadgeTone = "ready" | "needs-setup" | "optional" | "warning" | "info";

export interface StepBadge {
  text: string;
  tone: BadgeTone;
  /** ✓ done-state derives from live probes only, never visit history. */
  done: boolean;
}

// Derive each step's live badge from SetupStatus (brief §6.10). Only Essentials + Finish
// carry real logic here; the stubbed corporate/work steps get neutral "Optional" badges.
export function deriveBadge(step: StepId, status: SetupStatus): StepBadge {
  const modelConnected = hasConnectedModel(status);
  const readyBarriers = status.barriers.filter((b) => b.state === "ready").length;
  const totalBarriers = status.barriers.length || 3;

  switch (step) {
    case "environment":
      if (status.noRunner) return { text: "Needs setup", tone: "needs-setup", done: false };
      if (readyBarriers === 0) return { text: "Needs setup", tone: "needs-setup", done: false };
      return {
        text: `Ready · ${readyBarriers} of ${totalBarriers} barriers`,
        tone: "ready",
        done: true,
      };
    case "model": {
      const connectedFamily = status.models.find((m) => m.connected);
      return modelConnected && connectedFamily
        ? { text: `Ready · ${connectedFamily.label.split(" /")[0]} connected`, tone: "ready", done: true }
        : { text: "Needs setup", tone: "needs-setup", done: false };
    }
    case "proxy":
      return status.proxy.upstreamSecretName
        ? { text: "Configured", tone: "ready", done: false }
        : { text: "Optional", tone: "optional", done: false };
    case "scm":
      return status.scm.some((p) => p.state === "configured")
        ? { text: "Configured", tone: "ready", done: false }
        : { text: "Optional", tone: "optional", done: false };
    case "artifacts":
      return status.artifacts.length > 0
        ? { text: "Configured", tone: "ready", done: false }
        : { text: "Optional", tone: "optional", done: false };
    case "workspaces": {
      const readyCount = status.workspaces.filter((w) => w.state === "ready").length;
      return readyCount > 0
        ? {
            text: `Ready · ${readyCount} onboarded`,
            tone: "ready",
            done: true,
          }
        : status.workspaces.length > 0
        ? { text: "In progress", tone: "info", done: false }
        : { text: "Optional", tone: "optional", done: false };
    }
    case "credentials":
      // Always Optional, always excluded from readiness (brief §4.2).
      return { text: "Optional", tone: "optional", done: false };
    case "review":
      if (status.noRunner) return { text: "Needs attention", tone: "warning", done: false };
      return readyBarriers > 0 && modelConnected
        ? { text: "All essentials ready", tone: "ready", done: true }
        : { text: "Review what's left", tone: "info", done: false };
    case "launch":
      if (status.hasRuns) return { text: "First run launched", tone: "ready", done: true };
      return readyBarriers > 0 && modelConnected
        ? { text: "Ready to launch", tone: "ready", done: false }
        : { text: "Set up the essentials first", tone: "info", done: false };
  }
}

/** Phase progress like "Essentials 2/2" (counts done steps). */
export function phaseProgress(phase: PhaseDef, status: SetupStatus): { done: number; total: number } {
  const done = phase.steps.filter((s) => deriveBadge(s, status).done).length;
  return { done, total: phase.steps.length };
}
