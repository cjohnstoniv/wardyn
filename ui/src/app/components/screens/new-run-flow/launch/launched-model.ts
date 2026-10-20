/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the Launched screen says about a run that is waiting: the place in line,
// since when, and when it gives up. Numbers become words here; the sentences are
// canon (NR_LAUNCH) and the server's.

import { NR_LAUNCH } from "../../../wardyn/copy/new-run/launch";
import { NR_SHARED } from "../../../wardyn/copy/new-run/shared";
import { mediumDate, shortTime } from "../../../../lib/format";
import { ordinal } from "../../../../lib/ordinal";
import { RUN_WAIT, RUN_WAIT_REASON } from "../../../../lib/runner-pool-refusals";
import type { AgentRun, RunQueue } from "../../../../lib/types/runs";

/** A run as the Launched screen reads it: AgentRun plus the warnings a create answers with. */
export type LaunchedRun = AgentRun & { warnings?: string[] };

export { ordinal };

/** The place in line the person is told: the wire counts the runs ahead, so the run itself is one more. */
export const placeInLine = (queue: RunQueue): string => NR_LAUNCH.IN_LINE(ordinal(queue.position + 1));

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
/** "Just now" for the first minute; after that the time it joined the line. */
export const waitingSince = (queuedAt: string, now: number): string =>
  now - Date.parse(queuedAt) >= MINUTE ? shortTime(queuedAt) : NR_LAUNCH.JUST_NOW;

/** "In N hours" while an hour or more is left; inside the last hour, the time it gives up. */
export function givesUp(deadlineAt: string, now: number): string {
  const left = Date.parse(deadlineAt) - now;
  const hours = left < HOUR ? 0 : Math.round(left / HOUR);
  return hours >= 1 ? NR_LAUNCH.GIVES_UP_IN(hours === 1 ? NR_SHARED.HOUR_ONE : NR_SHARED.HOURS(hours)) : shortTime(deadlineAt);
}

/** Why the run waits, in the server's own sentence for that reason. Undefined until the pool is known.
 *  startAt is a scheduled run's queue.start_at, when the wire carries it. */
export function waitingBody(reason: string, pool: { name: string; max?: number } | undefined, startAt?: string): string | undefined {
  // These sentences name no pool, so they need none.
  if (reason === RUN_WAIT_REASON.RECORDING_STORE_OFF) return RUN_WAIT.RECORDING_STORE_OFF();
  if (reason === RUN_WAIT_REASON.SCHEDULED) return RUN_WAIT.SCHEDULED(startAt && `${mediumDate(startAt)}, ${shortTime(startAt)}`);
  if (reason === RUN_WAIT_REASON.OWNER_SIGN_IN) return RUN_WAIT.OWNER_SIGN_IN();
  if (!pool) return undefined;
  if (reason === RUN_WAIT_REASON.NO_RUNNER_FREE) return RUN_WAIT.NO_RUNNER_FREE(pool.name);
  if (reason === RUN_WAIT_REASON.RUNNERS_AT_CAPACITY) return RUN_WAIT.RUNNERS_AT_CAPACITY(pool.name);
  return pool.max === undefined ? undefined : NR_LAUNCH.WAITING_BODY(pool.name, pool.max);
}
