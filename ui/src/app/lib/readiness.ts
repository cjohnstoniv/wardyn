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
// the integration rows below still derive is no model path at all.
//
// composerReady reads the SAME rows /integrations itself derives
// (lib/api/integrations.ts) instead of a bespoke heuristic over raw
// SetupStatus fields — one source of truth. AI rows never depend on
// SiteConfig (only SCM/mirror/proxy rows do), so `null` is the right
// siteConfig to pass into deriveIntegrations.
//
// Honesty guard: a `wire: "fake"` composer backend — the default `make setup`
// demo config — can never satisfy composerReady. deriveAiRows only ever reads
// status.composer.backends for the Azure provider; a `fake` backend matches
// none of its rows, so it never becomes a row in the first place. Do not "fix"
// this by falling back to raw status.composer.backends.
import type { SetupStatus } from "./types";
import { deriveIntegrations, type IntegrationRow } from "./api/integrations";

const WARDYN_FEATURES_CAPABILITY = /^Wardyn features/;

function aiIntegrationRows(status: SetupStatus): IntegrationRow[] {
  return deriveIntegrations(status, null, status.secrets.present).ai;
}

// A row's credential is genuinely usable — excludes Bedrock/Azure's
// region/model-incomplete posture (IntegrationRow's "region_model_unset").
function credentialResolved(row: IntegrationRow): boolean {
  return row.posture.kind !== "region_model_unset";
}

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
  composerReady: boolean;
}

export function deriveReadiness(status: SetupStatus): Readiness {
  const barrierCount = status.runner?.confinement_classes?.length ?? 0;
  const aiRows = aiIntegrationRows(status);
  // ≥1 integration with the Wardyn-features capability ON and a resolved
  // credential.
  const composerReady = aiRows.some(
    (r) => credentialResolved(r) && r.chips.some((c) => !c.muted && WARDYN_FEATURES_CAPABILITY.test(c.label)),
  );
  const llmReady = hasLlmPath(status);
  // The first enabled provider serving a harness — the one llm_ready counted.
  const provider = llmReady ? status.model_providers?.find((p) => !p.disabled && p.harnesses.length > 0) : undefined;
  return {
    ready: status.ready,
    barrierReady: barrierCount > 0,
    barrierCount,
    llmReady,
    llmLabel: provider ? provider.name || provider.id : "",
    composerReady,
  };
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
