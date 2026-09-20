/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// waitingReauth — the ONE string the RUNS BOARD carries for a run that is
// waiting on its owner's AWS sign-in.
//
// It lives in a leaf module, and that is the whole point. The board card is on
// the EAGER graph (App -> AppShell -> RunsScreen -> runs/run-card), while
// wardyn/model-access-copy.ts belongs to the LAZY side: the door dialog, the
// New Run rail, the failure block and the model-access banner, which app-shell
// loads through React.lazy on purpose. Importing the board's one string from
// there dragged model-access-copy.ts (6.7 kB) into the entry chunk and, through
// its own AGENTS import, lib/workspace-providers-copy.ts (16.1 kB) with it —
// 22.7 kB of lazy-side copy in the first load, which pushed the entry chunk
// past bundle-split.test.ts's budget (578,279 B against 573,440).
//
// This module imports NOTHING, so the eager graph pays only for the sentence it
// actually renders. model-access-copy.ts RE-EXPORTS it, so there is still one
// definition and the lazy-side surfaces keep the import path their canon row
// names.
//
// DRAFT (M2 canon pending) — round-2 UX S8: the COUNT stays. A count-free
// string would hide a co-pending egress approval, and the person would sign in
// and watch the run sit there.
//
// `mine` is the VIEWER's relation to the run, and it is the difference between
// a true sentence and a false one (W6-U SHOULD-1): addressing every reader as
// the owner would have a shared-lane member read "Waiting for your AWS
// sign-in" on the same screen whose row tells them to ask their admin, and an
// admin opening a member's held run read it about a sign-in of theirs that
// could never clear it. Only the run's owner can.
//
// Defaulted TRUE rather than required: the one caller this module cannot reach
// is ui/e2e/live/sso-reauth-hold.spec.ts, which lane e2e-sso-path owns and
// which asserts this string on the OWNER's own run. The two console callers
// (run-detail-summary-header.tsx, runs/run-card.tsx) both pass it explicitly.
export const waitingReauth = (n: number, mine = true): string => {
  const head = mine ? "Waiting for your AWS sign-in" : "Waiting for the owner's AWS sign-in";
  return n > 1 ? `${head} · ${n - 1} more waiting` : head;
};
