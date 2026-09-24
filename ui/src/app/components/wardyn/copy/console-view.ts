/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Admin view / User view pages (packet M-A, approved 2026-09-23; frozen
// strings verbatim from modes-a.html). The switch, eyebrow and tab-title
// strings arrive with the switch itself (M-2).
export const CONSOLE_VIEW = {
  SWITCH_FAILED: "Could not switch — try again.",
} as const;

// A user on /admin/*. Nothing behind it is fetched; the server refuses every
// admin read and write anyway, so this page is a courtesy.
export const VIEW_REFUSAL = {
  TITLE: "Admin view",
  BODY: "This page is part of the admin view, which is for Wardyn admins. You're signed in as a user.",
  CTA: "Go to your runs",
} as const;

// An admin in the user view opens /admin/*. Entering admin authority is
// always a click, never a redirect.
export const VIEW_TO_ADMIN = {
  TITLE: "This page is in the admin view",
  BODY: "Switch to the admin view to open it.",
  GO: "Switch to admin view",
  STAY: "Stay in user view",
} as const;

// An admin in the admin view opens /runs/new, /account or /setup.
export const VIEW_TO_USER = {
  TITLE: "This page is in the user view",
  BODY: "Starting runs and your own connections are in the user view.",
  GO: "Switch to user view",
  STAY: "Stay in admin view",
} as const;

// The admin token on an SSO install opens a user page: it is not a person.
export const VIEW_ADMIN_TOKEN = {
  BODY: "Runs and connections belong to a person. Sign in with SSO to use them.",
  CTA: "Back to the admin view",
} as const;
