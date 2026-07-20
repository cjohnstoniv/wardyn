/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The first-run funnel's DECISION helpers, split out of setup-screen.tsx so
// App.tsx can ask "should the funnel open?" on mount without pulling the funnel
// itself into the entry chunk. setup-screen reaches llm-access →
// harness-login-pane → attach-terminal → xterm, so importing these two helpers
// from the screen module dragged the whole terminal stack into the initial
// bundle and defeated route-level code-splitting.
//
// setup-screen re-exports both, so existing importers/tests are unaffected.
import { lsGet, lsSet } from "../../../lib/storage";
import type { SetupStatus } from "../../../lib/types";

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

// Model-skip flag — the operator explicitly chose to set up NO model/harness
// provider (they'll bring their own container, or drive an interactive run). It
// earns the provider step its checkmark without a connected model, so a skipped
// provider reads as a deliberate decision rather than an unfinished "Optional".
// Per-browser (like the dismiss + onboarding-seen flags); a real connected model
// makes it moot. Cleared automatically once llmReady, so reconnecting supersedes.
const MODEL_SKIPPED_KEY = "wardyn-model-skipped";

export function modelSkipped(): boolean {
  return lsGet(MODEL_SKIPPED_KEY) === "1";
}

export function markModelSkipped(): void {
  lsSet(MODEL_SKIPPED_KEY, "1");
}

// Pure decision helper (unit-testable, and used verbatim by App.tsx): guide the
// operator into "Getting started" on a fresh, local, single-operator console.
// `ready` means runs *can* launch on this host — NOT that the operator has been
// onboarded — so a brand-new control plane with no runs yet still opens even when
// ready (that is the whole point of first-run setup). An explicit dismissal
// (Finish later / launch) or a first launched run stops it, and it never
// force-opens on a hosted/SSO multi-admin control plane.
export function shouldOpenSetup(status: SetupStatus, dismissed: boolean): boolean {
  // A synthetic fallback status (daemon didn't answer) proves nothing about the
  // host — never auto-open on it. Its has_runs:false would otherwise force the
  // funnel open with danger cards built from made-up fields.
  if (status.unreachable) return false;
  if (dismissed || status.auth.mode !== "local") return false;
  return !status.has_runs || !status.ready;
}

// The HARD first-run gate: while active, the app nav is hidden and every route
// except Getting Started (and the demos that are part of it) redirects to
// /setup, so a fresh local operator must go THROUGH setup rather than landing in
// an unconfigured app. Same predicate as the soft auto-open — finishing the flow
// (dismissSetup), a first launched run, an already-onboarded console (has_runs),
// SSO/team mode, or an unreachable daemon all clear it. Reads the dismiss flag
// live so completing the funnel unlocks nav on the next render.
export function setupGateActive(status: SetupStatus): boolean {
  return shouldOpenSetup(status, setupDismissed());
}
