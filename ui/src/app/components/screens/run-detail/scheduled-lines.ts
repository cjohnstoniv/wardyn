/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The sentences of a scheduled run's two waits, for the run page's heroes and the Launched screen. The words are
// the FEAT-2 boards' (copy/scheduled-runs.ts); only the times are filled in, in the zone the start was given in.

import { relativeTime } from "../../../lib/format";
import type { RunQueue } from "../../../lib/types";
import { NR_RUN } from "../../wardyn/copy/new-run/run";
import { SCHEDULED_RUN } from "../../wardyn/copy/scheduled-runs";
import { queueWhen } from "../runs/scheduled-state";

/** "Starts Wed 21 Oct, 02:00 (UTC) · in 12h"; the time alone once it has come, nothing when the wire has no start. */
export function startsLine(queue: RunQueue, now: number = Date.now()): string | undefined {
  const when = queueWhen(queue, queue.start_at);
  if (!when || !queue.start_at) return undefined;
  return Date.parse(queue.start_at) > now ? SCHEDULED_RUN.PAGE_STARTS(when, relativeTime(queue.start_at).replace(/^in /, "")) : NR_RUN.STARTS(when);
}

/** What a due run waiting for its owner's sign-in tells them, or nothing when the wire has no start or deadline. */
export function signInBody(queue: RunQueue): string | undefined {
  const due = queueWhen(queue, queue.start_at);
  const deadline = queueWhen(queue, queue.deadline_at);
  return due && deadline ? SCHEDULED_RUN.SIGN_IN_BODY(due, deadline) : undefined;
}
