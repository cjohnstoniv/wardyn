/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { RUNS_ROW_WORD } from "./runs-landing";

// The run header's status sentence and its time fact: what the run is doing, said as visible text.
export const RUN_HEADER = {
  MORE: "More",
  REVIEW: "Review",
  WORKING: "Working. Nothing is waiting on you.",
  /** The Runs list's wording for a decision this viewer cannot make. */
  WAITING_FOR_ADMIN: `${RUNS_ROW_WORD.WAITING_FOR_ADMIN}.`,
  WAITING: "Waiting on you to decide.",
  FINISHED: "Finished.",
  STOPPED: "Stopped. Nothing is running.",
  FAILED: "Did not finish.",
  KILLED: "Killed. Nothing is running.",
  ARCHIVED: "Archived.",
  QUEUED: "Waiting for a free slot.",
  STARTING: "Starting the sandbox.",
  RAN: (duration: string, endedAt: string) => `Ran ${duration} · ended ${endedAt}`,
  // The decision phrase every "a decision is waiting" surface shares.
  DECISIONS_NEED_YOU: (n: number) => (n === 1 ? "1 decision needs you" : `${n} decisions need you`),
} as const;
