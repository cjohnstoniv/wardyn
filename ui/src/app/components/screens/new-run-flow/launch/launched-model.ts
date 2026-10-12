/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// What the Launched screen says about a run that is waiting: the place in line,
// since when, and when it gives up. Numbers become words here; the sentences are
// canon (NR_LAUNCH) and the server's.

import { NR_LAUNCH } from "../../../wardyn/copy/new-run/launch";
import { NR_SHARED } from "../../../wardyn/copy/new-run/shared";
import { RUN_WAIT, RUN_WAIT_REASON } from "../../../../lib/runner-pool-refusals";
import type { AgentRun } from "../../../../lib/types/runs";

/** Go: types.RunQueue. `position` is how many runs wait ahead in the pool: 0 is next in line. */
export interface RunQueue {
  pool_id: string;
  queued_at: string;
  deadline_at: string;
  position: number;
  reason: string;
}
/** A run as the Launched screen reads it: AgentRun plus the queue a waiting run carries, and the create's warnings. */
export type LaunchedRun = AgentRun & { queue?: RunQueue; warnings?: string[] };

const SUFFIX: Record<string, string> = { one: "st", two: "nd", few: "rd", other: "th" };
const ORDINALS = new Intl.PluralRules("en-US", { type: "ordinal" });
export const ordinal = (n: number): string => `${n}${SUFFIX[ORDINALS.select(n)]}`;

/** The place in line the person is told: the wire counts the runs ahead, so the run itself is one more. */
export const placeInLine = (queue: RunQueue): string => NR_LAUNCH.IN_LINE(ordinal(queue.position + 1));

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
const AGO = new Intl.RelativeTimeFormat("en-US", { numeric: "always" });
const MINUTES = new Intl.NumberFormat("en-US", { style: "unit", unit: "minute", unitDisplay: "long" });

export function waitingSince(queuedAt: string, now: number): string {
  const ms = now - Date.parse(queuedAt);
  if (!(ms >= MINUTE)) return NR_LAUNCH.JUST_NOW;
  if (ms < HOUR) return AGO.format(-Math.floor(ms / MINUTE), "minute");
  if (ms < 48 * HOUR) return AGO.format(-Math.floor(ms / HOUR), "hour");
  return AGO.format(-Math.floor(ms / (24 * HOUR)), "day");
}

export function givesUp(deadlineAt: string, now: number): string {
  const ms = Math.max(Date.parse(deadlineAt) - now, MINUTE);
  const hours = Math.round(ms / HOUR);
  if (hours < 1) return NR_LAUNCH.GIVES_UP_IN(MINUTES.format(Math.ceil(ms / MINUTE)));
  return NR_LAUNCH.GIVES_UP_IN(hours === 1 ? NR_SHARED.HOUR_ONE : NR_SHARED.HOURS(hours));
}

/** Why the run waits, in the server's own sentence for that reason. Undefined until the pool is known. */
export function waitingBody(reason: string, pool: { name: string; max?: number } | undefined): string | undefined {
  if (!pool) return undefined;
  if (reason === RUN_WAIT_REASON.NO_RUNNER_FREE) return RUN_WAIT.NO_RUNNER_FREE(pool.name);
  return pool.max === undefined ? undefined : NR_LAUNCH.WAITING_BODY(pool.name, pool.max);
}
