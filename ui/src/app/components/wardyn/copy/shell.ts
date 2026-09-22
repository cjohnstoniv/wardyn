/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { MEMBER_GETTING_STARTED } from "./getting-started";

// DRAFT (M2 canon pending) — staged in workspace-providers-prompt.md §7.6
// ("B-γ → wardyn/copy.ts"), parsed by nothing today.
//
// The shell's identity states (B1, R4-F107). Both are about the ONE question
// the console cannot answer for itself: who is signed in. A failed /me must
// not render the admin nav off a fail-open guess — indistinguishable from an
// authz breach to the human reading it — and a failed sign-out must not
// silently console.error with nobody seeing it.
export const SHELL = {
  // B1: settled, but /me never answered. Rendered instead of a guessed nav, so
  // it has to say that the emptiness is ignorance and not a denial.
  UNKNOWN_BODY:
    "We couldn't confirm who you are. Nothing here is hidden from you on purpose — reload, or sign in again.",
  // M2 §1: no new word — the banner's action re-fires whoami(), which is
  // exactly what the member Getting Started page's Retry already means.
  UNKNOWN_ACTION: MEMBER_GETTING_STARTED.RETRY,
  // R4-F107: POST /auth/logout failed, so the HttpOnly OIDC session cookie may
  // still be live — the local token is gone either way, which is why the title
  // says "here".
  // DRAFT (M2) — DIVERGES from the §7.6 staging ("Couldn't sign you out" /
  // "Your session is still live…"): these two are the M2 sitting sheet's §2
  // texts, which do not overclaim — a failed POST does not PROVE the session
  // survived, only that nothing confirmed it died.
  SIGN_OUT_FAILED_TITLE: "Signed out here, but not on the server",
  SIGN_OUT_FAILED_BODY: "Your session may still be active on the server. Close the browser, or try signing out again.",
} as const;

