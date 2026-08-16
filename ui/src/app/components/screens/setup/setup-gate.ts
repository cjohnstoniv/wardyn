/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The first-run funnel's own per-browser state (dismissed / skipped /
// visited), split out of setup-screen.tsx so App.tsx (and anything else)
// can read it without pulling the funnel itself into the entry chunk.
// setup-screen reaches integrations-step → integrations-screen →
// add-integration-dialog → harness-login-pane → attach-terminal → xterm, so
// importing these from the screen module dragged the whole terminal stack
// into the initial bundle and defeated route-level code-splitting.
//
// There is NO hard gate here any more: /setup is a route an operator chooses
// to visit, not one every other route redirects to. The old setupGateActive
// (mandatory first-run redirect) was deleted along with App.tsx's
// RequireSetupComplete — do not reintroduce either.
//
// setup-screen re-exports these, so existing importers/tests are unaffected.
import { lsGet, lsSet } from "../../../lib/storage";
import type { SetupStepId } from "./steps";

// ------------------------------------------------------------
// Dismiss flag — via lib/storage's private-mode-tolerant lsGet/lsSet.
// ------------------------------------------------------------
const DISMISS_KEY = "wardyn-setup-dismissed";

export function setupDismissed(): boolean {
  return lsGet(DISMISS_KEY) === "1";
}

export function dismissSetup(): void {
  lsSet(DISMISS_KEY, "1");
}

// Integrations-skip flag — the operator explicitly chose to move past the
// Integrations step with nothing connected (no model/harness, SCM host,
// artifact mirror, or host proxy). It earns the step its checkmark without any
// integration, so a deliberate skip reads as a decision rather than an
// unfinished "Optional". Per-browser (like the dismiss + onboarding-seen
// flags); a real connected integration makes it moot. Generalized from the old
// per-step "model-skipped" flag now that the provider step is folded into
// Integrations.
const INTEGRATIONS_SKIPPED_KEY = "wardyn-integrations-skipped";

export function integrationsSkipped(): boolean {
  return lsGet(INTEGRATIONS_SKIPPED_KEY) === "1";
}

export function markIntegrationsSkipped(): void {
  lsSet(INTEGRATIONS_SKIPPED_KEY, "1");
}

// Visited-step set (A4) — steps the operator has navigated AWAY from at least
// once (per browser), regardless of direction (Next, Back, a rail jump, or an
// in-step jump all count — leaving a step backward still means you've seen it).
// Powers the rail's neutral "Skipped" chip + muted dot for an OPTIONAL step left
// unconfigured, so it stops reading as a perpetual, un-acted-on "Optional". Same
// JSON-array-over-lsGet/lsSet shape as loadLaunchedDemos/markDemoLaunched
// (../demos/demo-catalog.ts) — this is a Set of ids, not a single flag.
const VISITED_KEY = "wardyn-setup-visited";

export function loadVisitedSteps(): SetupStepId[] {
  try {
    const parsed = JSON.parse(lsGet(VISITED_KEY) ?? "[]");
    return Array.isArray(parsed) ? (parsed as SetupStepId[]) : [];
  } catch {
    return [];
  }
}

export function markStepVisited(stepId: SetupStepId): void {
  const set = new Set(loadVisitedSteps());
  if (!set.has(stepId)) {
    set.add(stepId);
    lsSet(VISITED_KEY, JSON.stringify([...set]));
  }
}
