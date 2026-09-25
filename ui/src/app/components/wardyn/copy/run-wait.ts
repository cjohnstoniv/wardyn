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
export const RUN_WAIT = {
  waiting: (n: number) => `${n} waiting`,
  waitingHeld: (n: number) => `${n} waiting · sandbox held`,
} as const;
