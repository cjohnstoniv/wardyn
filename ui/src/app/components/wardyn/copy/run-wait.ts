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
// #3040: one phrase for "a person has to decide" wherever a person can act: "Needs you", with the count
// after it when there is more than one, and "sandbox held" only as the modifier for a parked sandbox.
function needsYou(n: number): string {
  return n === 1 ? "Needs you" : `Needs you · ${n}`;
}

export const RUN_WAIT = {
  waiting: needsYou,
  waitingHeld: (n: number) => `${needsYou(n)} · sandbox held`,
} as const;
