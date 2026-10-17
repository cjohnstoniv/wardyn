/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Readiness derivation from the REAL SetupStatus. barrierCount drives the honest
// "N of 3 barriers" line; llmReady drives the model-provider row and the
// record-pane's model warning.
//
// This lived in screens/onboarding/intro.tsx alongside that screen's React
// components. It is used by the app shell, the demos screen and workspace
// detail — none of which are the onboarding funnel, all of which outlive it —
// so it moved here when the funnel was deleted. Pure functions, no React.
//
// llmReady is the server's own answer (SetupStatus.llm_ready: an enabled model
// provider serves a harness — internal/api/setup.go's llmPathExists), and
// llmLabel names the first such provider. Since 0.8 (#548) a run's model
// credential comes only from its model provider, so an operator key or login
// is no model path at all.
import type { SetupStatus } from "./types";

// Whether a coding agent (Claude Code / Codex CLI) has somewhere to call: the
// server's llm_ready, which a member's redacted status keeps too. Used
// directly by callers that only need the boolean (workspace-detail.tsx,
// feeding record-pane.tsx's model-readiness warning) without the rest of
// Readiness.
export function hasLlmPath(status: SetupStatus): boolean {
  return status.llm_ready === true;
}

export interface Readiness {
  /** The backend's own boot readiness (status.ready). */
  ready: boolean;
  barrierReady: boolean;
  barrierCount: number;
  llmReady: boolean;
  /** Human label for the connected LLM path, "" when none. */
  llmLabel: string;
}

export function deriveReadiness(status: SetupStatus): Readiness {
  const barrierCount = status.runner?.confinement_classes?.length ?? 0;
  const llmReady = hasLlmPath(status);
  // The first enabled provider serving a harness — the one llm_ready counted.
  const provider = llmReady ? status.model_providers?.find(servesAHarness) : undefined;
  return {
    ready: status.ready,
    barrierReady: barrierCount > 0,
    barrierCount,
    llmReady,
    llmLabel: provider ? provider.name || provider.id : "",
  };
}

// The providers llm_ready counts: enabled, and serving at least one harness.
const servesAHarness = (p: NonNullable<SetupStatus["model_providers"]>[number]) => !p.disabled && p.harnesses.length > 0;

// How many model providers the Secrets step reads as connected (its rail badge,
// its auto-skip and its Skipped override): the server's own answer, so 0
// whenever llm_ready is false. An operator key a status still lists is no model
// path since 0.8 (#548) and never counts.
export function modelProviderCount(status: SetupStatus): number {
  return hasLlmPath(status) ? (status.model_providers ?? []).filter(servesAHarness).length : 0;
}

// deploymentMode — single-user (one admin credential, no per-person identity)
// vs multi-user (SSO, each person their own admin/member role). Derived, no
// wire change: only `sso` widens the audience — an unknown/future auth.mode
// reads single-user, the narrower/safer default.
type DeploymentMode = "single-user" | "multi-user";

export function deploymentMode(s: SetupStatus): DeploymentMode {
  return s.auth.mode === "sso" ? "multi-user" : "single-user";
}

// lastCheckedLabel — the relative "Checked Ns ago" line for the host-status
// strip and any re-check control.
export function lastCheckedLabel(at: Date | null): string {
  if (!at) return "";
  const s = Math.round((Date.now() - at.getTime()) / 1000);
  if (s < 5) return "Checked just now";
  if (s < 60) return `Checked ${s}s ago`;
  return `Last checked ${at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`;
}
