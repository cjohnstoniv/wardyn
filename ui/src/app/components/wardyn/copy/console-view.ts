/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The Admin view / User view pages (packet M-A, approved 2026-09-23; frozen
// strings verbatim from modes-a.html).
export const CONSOLE_VIEW = {
  GROUP: "Console view",
  ADMIN: "Admin view",
  USER: "User view",
  SWITCH_FAILED: "Could not switch — try again.",
  // The Admin view's sidebar eyebrow. The User view's "User view · {type} ▾"
  // arrives with the type picker (UT-13 / UT-7a).
  EYEBROW_ADMIN: "Admin view",
  TITLE_ADMIN: "Wardyn admin",
  TITLE_USER: "Wardyn",
  // Packet M-B (QM-7, modes-b.html): a link straight to the same object in
  // User view — the admin's own rows (M-7, now built — see live-approvals.tsx
  // and console-view.tsx), the Record dependency line (M-6, §4.6) and #543's
  // failure block or reauth card on the admin's own run (whose door is User
  // view only) all reuse this one string.
  OPEN_IN_USER: "Open in user view",
} as const;

export const NAV = {
  YOUR_ACCOUNT: "Your account",
} as const;

// M-5 (#636, packet S-3, approved 2026-09-27): the /account page header's
// description — every console page header has one, and this says what's here
// and whose it is, which is the point of the split.
export const YOUR_ACCOUNT = {
  LEDE: "The connections and keys your own runs use. No one else's runs can use them.",
} as const;

// M-5 (#636, packet S-5, approved 2026-09-27): a security admin reaches
// /admin/settings from a stale link — that tier has no Settings in its nav
// (app-shell.tsx's navItemsForView), so this only ever renders from a typed
// or bookmarked URL. Same shape as VIEW_REFUSAL: title "Admin view", a body
// naming the tier, one CTA back to the caller's own Runs. Nothing behind the
// page is fetched (AdminSettingsScreen checks this before its first read).
export const SETTINGS_SUPER_ONLY = {
  BODY: "Settings is for super admins. You're signed in as a security admin.",
  CTA: "Back to Runs",
} as const;

// The no-credential preview (0.7.5), now entered from the Permissions header.
// Its band is the one kept: sign-in is refused there, which is abnormal.
export const USER_PREVIEW = {
  MENU_NEW: "Preview as a new user",
  BANNER:
    "Viewing as a new user — not signed in to AWS; signing in is refused until you exit; your usual role is paused for this session",
  EXIT: "Exit preview",
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

// M-7 (packet M-B, approved 2026-09-23; frozen verbatim from modes-b.html) —
// the switch link a "not yours" sentence carries on the admin's own row: the
// admin view has no personal doors, even there (admin-member-modes-design.md
// §4.6), so this is the one way back to the door instead.
export const OPEN_IN_USER_VIEW = CONSOLE_VIEW.OPEN_IN_USER;

// M-7 (modes-b.html §1, verbatim): the admin's own row on /admin/runs names its
// owner "ann@acme.example (you)".
export const ownerLabel = (owner: string, own: boolean) => (own ? `${owner} (you)` : owner);
