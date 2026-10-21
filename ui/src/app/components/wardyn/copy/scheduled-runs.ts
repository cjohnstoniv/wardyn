/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A scheduled run on the Runs list and the run page, byte for byte from docs/design/scheduled-runs-canon.md
// (the FEAT-2 boards F2SCHED-2 and F2SCHED-3). Small and eager: the Runs list needs it. The start-time picker's
// strings are New Run's (copy/new-run/run.ts). scheduled-runs-copy.test.ts keeps this file and the doc equal.
// `when` is whenText() in lib/schedule.ts: "Wed 21 Oct, 02:00 (UTC)".

export const SCHEDULED_RUN = {
  /** The state word, on the Runs list row and the run page's badge. */
  WORD: "Scheduled",
  ROW_STARTS: (when: string | number) => `starts ${when}`,
  PAGE_STARTS: (when: string | number, wait: string | number) => `Starts ${when} · in ${wait}`,
  PAGE_NOTE: "Policy and your access are checked again when it starts. Nothing is set up and no credential is held until then.",
  /** The same words as NR_LAUNCH.CANCEL_RUN, which is lazy-only. */
  CANCEL_RUN: "Cancel run",
  ROW_DUE: (when: string | number) => `was due ${when}`,
  SIGN_IN_TITLE: "Waiting for your sign-in",
  SIGN_IN_BODY: (when: string | number, deadline: string | number) =>
    `This run was due to start at ${when}. It starts when you sign in again; nothing runs until then. If you don't sign in by ${deadline}, the run fails without starting. Cancel it if you no longer need it.`,
  SIGN_IN_TIMED_OUT: "This run waited too long for your sign-in and was not started.",
} as const;
