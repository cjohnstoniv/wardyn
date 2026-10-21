/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A run that waits for its time, or for its owner's sign-in once the time has come (queue reasons scheduled and
// owner_sign_in), and how the Runs list and the run page say so. Pure: the rows and the run page read it.

import { browserZone, whenText } from "../../../lib/schedule";
import type { AgentRun } from "../../../lib/types";
import { RUNS_ROW_WORD } from "../../wardyn/copy/runs-landing";
import { SCHEDULED_RUN } from "../../wardyn/copy/scheduled-runs";

export type ScheduledWait = "scheduled" | "sign_in";

/** Go: runnerpool.WaitScheduled and runnerpool.WaitOwnerSignIn. */
const REASON: Record<string, ScheduledWait> = { scheduled: "scheduled", owner_sign_in: "sign_in" };

/** Go: types.RunEndQueueSignInTimeout, the status_reason of a scheduled run that waited out its sign-in. */
export const SIGN_IN_TIMEOUT = "queue_sign_in_timeout";

/** Which scheduled wait a run is in, or null for any other run. */
export function scheduledWait(run: Pick<AgentRun, "state" | "queue">): ScheduledWait | null {
  return run.state === "QUEUED" && run.queue ? (REASON[run.queue.reason] ?? null) : null;
}

/** A failed run whose scheduled start came and whose owner never signed in. */
export const signInTimedOut = (run: Pick<AgentRun, "state" | "status_reason">): boolean =>
  run.state === "FAILED" && run.status_reason === SIGN_IN_TIMEOUT;

/** An instant of the run's queue as the console says a start, in the zone the start was given in. */
export function queueWhen(queue: NonNullable<AgentRun["queue"]>, iso: string | undefined): string | undefined {
  return iso ? whenText(iso, queue.time_zone || browserZone()) : undefined;
}

/** The part of a row's meta line that says when a scheduled run starts (or was due), if the wire carries it. */
export function scheduledMeta(run: Pick<AgentRun, "state" | "queue">): string | undefined {
  const kind = scheduledWait(run);
  const when = run.queue && queueWhen(run.queue, run.queue.start_at);
  if (!kind || !when) return undefined;
  return kind === "scheduled" ? SCHEDULED_RUN.ROW_STARTS(when) : SCHEDULED_RUN.ROW_DUE(when);
}

/** The run page badge's word for a scheduled wait, or undefined to keep the state's own ("Queued"). */
export function scheduledBadgeWord(run: Pick<AgentRun, "state" | "queue">): string | undefined {
  const kind = scheduledWait(run);
  return kind === "scheduled" ? SCHEDULED_RUN.WORD : kind === "sign_in" ? RUNS_ROW_WORD.WAITING_SIGN_IN : undefined;
}
