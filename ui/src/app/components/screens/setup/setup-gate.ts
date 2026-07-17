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
