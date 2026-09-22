/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// waitingReauth — the ONE string the RUNS BOARD carries for a run that is
// waiting on its owner's AWS sign-in.
//
// IT LIVES IN A LEAF MODULE, AND THAT IS THE WHOLE POINT. The board card is on
// the EAGER graph (App -> AppShell -> RunsScreen -> runs/run-card), while
// wardyn/model-access-copy.ts belongs to the LAZY side: the door dialog, the
// New Run rail, the failure block and the model-access banner, which app-shell
// loads through React.lazy on purpose. Importing the board's one string from
// there dragged model-access-copy.ts (6.7 kB) into the entry chunk and, through
// its own AGENTS import, lib/workspace-providers-copy.ts (16.1 kB) with it —
// 22.7 kB of lazy-side copy in the first load, which pushed the entry chunk
// past bundle-split.test.ts's budget (578,279 B against 573,440).
//
// This module imports NOTHING except ado-capability-copy.ts, which is itself
// import-free (a flat `as const` object, no React, no other copy modules) —
// so the eager graph pays only for the sentences it actually renders, the
// same property that made this a leaf module in the first place.
// bundle-split.test.ts is the gate that would catch a regression here.
// model-access-copy.ts RE-EXPORTS waitingReauth, so there is still one
// definition and the lazy-side surfaces keep the import path their canon row
// names.
import { ADO_CAPABILITY } from "./ado-capability-copy";
//
// DRAFT (M2 canon pending) — round-2 UX S8: the COUNT stays. A count-free
// string would hide a co-pending egress approval, and the person would sign in
// and watch the run sit there.
//
// `mine` is the VIEWER's relation to the run, and it is the difference between
// a true sentence and a false one (W6-U SHOULD-1): the board and the cockpit
// header addressed every reader as the owner, so a shared-lane member read
// "Waiting for your AWS sign-in" on the same screen whose row told them to ask
// their admin, and an admin opening a member's held run read it about a
// sign-in of theirs that could never clear it. Only the run's owner can.
//
// Defaulted TRUE rather than required: the one caller this module cannot reach
// is ui/e2e/live/sso-reauth-hold.spec.ts, which lane e2e-sso-path owns and
// which asserts this string on the OWNER's own run. The two console callers
// (run-detail-summary-header.tsx, runs/run-card.tsx) both pass it explicitly.
export const waitingReauth = (n: number, mine = true): string => {
  const head = mine ? "Waiting for your AWS sign-in" : "Waiting for the owner's AWS sign-in";
  return n > 1 ? `${head} · ${n - 1} more waiting` : head;
};

// waitingAdoConsent — the SAME string shape as waitingReauth, for the
// Azure DevOps Entra-consent hold (S10 round 2, F13). Its own function, not
// a `provider` argument on waitingReauth: the two must never share a call
// site that could silently pass the wrong provider's mine/count pair.
// WAITING_ADO_MINE/OWNER are canon (§10.5, round-2 fix N6) — the count
// suffix (`· {n-1} more waiting`) is composed here, same as waitingReauth,
// and is not itself a frozen string (a number is not canon).
export const waitingAdoConsent = (n: number, mine = true): string => {
  const head = mine ? ADO_CAPABILITY.WAITING_ADO_MINE : ADO_CAPABILITY.WAITING_ADO_OWNER;
  return n > 1 ? `${head} · ${n - 1} more waiting` : head;
};
