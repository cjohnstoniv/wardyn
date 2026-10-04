/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The admin People page (0.8.6 ppl-p2, mock packet M12, approved 2026-10-03). Strings are the
// packet's, character for character; the drawer's two count lines (SESSIONS_ACTIVE, RUNNING_COUNT)
// are the mock's own "1 active" / "0 running". The nav label shares this file with the screen's
// heading, so the two cannot drift.

export const PEOPLE_PAGE = {
  NAV: "People",
  TITLE: "People",
  LEAD: "Everyone who can reach this deployment, and what each person holds.",
  ACCESS_TITLE: "Who can sign in",
  ACCESS_SUPER_ONLY: "Only the super admin can see and change who can sign in.",
  TABLE_TITLE: "People",
  SEARCH: "Search people",
  FILTER_ALL: "All",
  FILTER_ACTIVE: "Active",
  FILTER_DEACTIVATED: "Deactivated",
  COL_PERSON: "Person",
  COL_ROLE: "Role",
  COL_LAST: "Last sign-in",
  COL_STATE: "State",
  COL_HOLDS: "Holds",
  STATE_ACTIVE: "Active",
  STATE_DEACTIVATED: "Deactivated",
  STATE_NEVER: "Never signed in",
  // The row's "Holds" cell: only what the person holds, "—" for nothing.
  HOLDS: (p: { sessions: number; tokens: number; keys: number; credentials: number; running: number }) => {
    const parts = [
      p.sessions && `${p.sessions} session${p.sessions === 1 ? "" : "s"}`,
      p.tokens && `${p.tokens} token${p.tokens === 1 ? "" : "s"}`,
      p.keys && `${p.keys} key${p.keys === 1 ? "" : "s"}`,
      p.credentials && `${p.credentials} credential${p.credentials === 1 ? "" : "s"}`,
      p.running && `${p.running} running`,
    ].filter(Boolean);
    return parts.length ? parts.join(" · ") : "—";
  },
  EMPTY: "Nobody has signed in yet. People appear here after their first sign-in, or when you add them.",
  ADD: "Add a person",
  ADD_HINT: "Add someone before their first sign-in, so you can assign them a profile or key domain ahead of time. They mint their own API tokens after they sign in.",
  SESSIONS: "Sessions",
  SESSIONS_ACTIVE: (n: number) => `${n} active`,
  SIGN_OUT: "Sign out everywhere",
  SIGN_OUT_CONFIRM: (p: string) =>
    `Sign ${p} out of every browser and the CLI? They can sign in again unless their access is removed.`,
  TOKENS: "API tokens",
  SSH: "SSH keys",
  SSH_REMOVE: "Remove all",
  SSH_REMOVE_CONFIRM: (p: string) => `Remove every SSH key ${p} has added? Their open SSH sessions end.`,
  CREDS: "Stored credentials",
  CREDS_ERASE: "Erase",
  CREDS_ERASE_CONFIRM: (p: string) => `Erase every credential ${p} has stored? This can't be undone.`,
  RUNS: "Runs",
  RUNNING_COUNT: (n: number) => `${n} running`,
  RUNS_LINK: "View their runs",
  SETUP_LINK: "Manage people any time under People in the admin view.",
} as const;
