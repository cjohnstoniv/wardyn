/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// #483 — signing in again in place. Frozen canon: docs/design/reauth-in-place-canon.md
// (reauth-copy.test.ts parses that table back and compares every row).
//
// Reused rather than restated here: "Copy my changes" and its toast
// (workspace-providers-copy.ts PROVIDERS_DRAFT), "Not now"
// (model-access-copy.ts MODEL_ACCESS_BANNER.NOT_NOW) and "Sign in with SSO"
// (sign-in.tsx SSO_SIGN_IN). The full sign-in screen's own notice is
// SESSION_ENDED_REASON in lib/api/core.ts.
export const REAUTH_DIALOG = {
  TITLE: "Sign in to continue",
  BODY: "Your session ended. Sign in again and this page carries on — nothing here has been lost.",
  WAITING: "Waiting for you to finish signing in…",
  POPUP_BLOCKED: "Your browser blocked the sign-in window.",
  POPUP_FALLBACK: "Open it in a new tab",
  CLOSED_WITHOUT: "That window closed before you signed in.",
  UNREACHABLE: "Wardyn isn't answering. This page is still here — try again in a moment.",
  WRITE_DROPPED: "Your last save didn't go through. Everything you typed is still here — save again.",
  ROLE_CHANGED_BODY:
    "You're signed in, but this page is no longer yours to open. Copy anything you need — Wardyn will take you to Runs.",
} as const;

export const REAUTH_BAR = {
  BODY: "You're signed out. This page is read-only until you sign in again.",
  CTA: "Sign in",
} as const;

// DRAFT (canon pending): the role-changed view's continue action. The mock
// froze the body sentence but not the button under it.
export const REAUTH_DRAFT = {
  GO_TO_RUNS: "Go to Runs",
} as const;
