/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The one nav label that app-shell.tsx (eager) needs from governance-copy.ts
// (screen-only canon, reached only through lazy governance/drives/providers/
// setup screens). Rollup can't tree-shake an OBJECT's individual properties —
// pulling GOVERNANCE.TITLE off the big canon table pulled the whole §7.2-§7.9
// object into the entry chunk (#498). governance-copy.ts's GOVERNANCE.TITLE
// reads this constant, so it stays the one string for both the nav label and
// the screen heading, per governance-prompt.md §7.2.
export const GOVERNANCE_NAV_TITLE = "Governance";

// Same reasoning, one entry later (UT-7a): the User types screen's own
// heading (user-types-copy.ts's USER_TYPES.TITLE) reads this constant too, so
// the nav label and the screen heading can't drift apart.
export const USER_TYPES_NAV_TITLE = "User types";

// The Credentials nav label (design F-1, canon NAV.CREDENTIALS) — kept here,
// not in the screen's own copy module, for the SAME entry-chunk reason as the
// two constants above. The screen's heading (INVENTORY.TITLE in
// wardyn/copy/credentials.ts) is the same string.
export const CREDENTIALS_NAV_TITLE = "Credentials";

// The Runners nav label, kept here for the entry-chunk reason above (the sidebar is eager,
// wardyn/copy/runners.ts is lazy). copy/runners.ts's RUNNERS.NAV reads this constant, so the nav
// label and the page heading stay one string.
export const RUNNERS_NAV_TITLE = "Runners";

// The Admin view's sidebar group headings (#3056), eager for the same entry-chunk reason.
export const ADMIN_NAV_GROUPS = {
  activity: "Activity",
  build: "Build",
  access: "Access",
  limits: "Limits",
  keys: "Keys",
} as const;
export type AdminNavGroup = keyof typeof ADMIN_NAV_GROUPS;

// The Runs board is in the entry chunk and names a local run's computer in each row's meta line (0.9,
// M-RN-3 RS8), so the three small things that row needs live here, for the same entry-chunk reason, and the
// lazy wardyn/copy/runners.ts reads them from here (RUN_PLACEMENT.CHIP, RUNNER.OFFLINE, runnerLabel).
export const RUNNER_OFFLINE_WORD = "Offline";
export const runOnChip = (name: string): string => `On ${name}`;
/**
 * What the run page calls a local run's runner: the name the server projected, else a short form of its id
 * (a read that failed leaves the name off), never a guess. Data, so it reads as a name anywhere in a sentence.
 * pending H11-P4: the fallbacks ("Runner 3f2a1c9e", "Its runner") are recommended text until the owner answers;
 * the packet draws only the case where the name is known.
 */
export function runnerLabel(run: { runner_name?: string; runner_id?: string }): string {
  return run.runner_name || (run.runner_id ? `Runner ${run.runner_id.slice(0, 8)}` : "Its runner");
}
