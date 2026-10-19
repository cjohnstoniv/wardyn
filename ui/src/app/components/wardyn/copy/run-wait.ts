/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The pending-approval chip's two strings, split out of run-cockpit.ts (#498):
// run-card.tsx (eager, part of the Runs board) needs only these two, but an
// object literal's properties can't be tree-shaken individually — importing
// RUN_COCKPIT for them pulled the whole /runs/:id cockpit copy table (terminal
// states, evidence widgets, focus mode, ...) into the entry chunk. RUN_COCKPIT
// spreads this in below, so run-detail's cockpit still reads
// RUN_COCKPIT.waiting/.waitingHeld unchanged.
// #3040: one phrase for "a person has to decide" everywhere a person can act — the noun is "decision",
// the verb is "needs" — and "sandbox held" only ever follows it as the modifier for a parked sandbox.
function needsDecision(n: number): string {
  return n === 1 ? "Needs a decision" : `Needs ${n} decisions`;
}

export const RUN_WAIT = {
  waiting: needsDecision,
  waitingHeld: (n: number) => `${needsDecision(n)} · sandbox held`,
} as const;
