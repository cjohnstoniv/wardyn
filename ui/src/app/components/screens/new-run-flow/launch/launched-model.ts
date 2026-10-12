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
import type { AgentRun, RunQueue } from "../../../../lib/types/runs";

/** A run as the Launched screen reads it: AgentRun plus the warnings a create answers with. */
export type LaunchedRun = AgentRun & { warnings?: string[] };

const SUFFIX: Record<string, string> = { one: "st", two: "nd", few: "rd", other: "th" };
const ORDINALS = new Intl.PluralRules("en-US", { type: "ordinal" });
export const ordinal = (n: number): string => `${n}${SUFFIX[ORDINALS.select(n)]}`;

/** The place in line the person is told: the wire counts the runs ahead, so the run itself is one more. */
export const placeInLine = (queue: RunQueue): string => NR_LAUNCH.IN_LINE(ordinal(queue.position + 1));

const MINUTE = 60_000;
const HOUR = 60 * MINUTE;
export function waitingSince(queuedAt: string, now: number): string {
  const ms = now - Date.parse(queuedAt);
  if (!(ms >= MINUTE)) return NR_LAUNCH.JUST_NOW;
  const minutes = Math.floor(ms / MINUTE);
  if (ms < HOUR) return minutes === 1 ? NR_LAUNCH.SINCE_MINUTE_ONE : NR_LAUNCH.SINCE_MINUTES(minutes);
  const hours = Math.floor(ms / HOUR);
  if (ms < 48 * HOUR) return hours === 1 ? NR_LAUNCH.SINCE_HOUR_ONE : NR_LAUNCH.SINCE_HOURS(hours);
  const days = Math.floor(hours / 24);
  return NR_LAUNCH.SINCE_DAYS(days);
}

export function givesUp(deadlineAt: string, now: number): string {
  const ms = Math.max(Date.parse(deadlineAt) - now, MINUTE);
  const hours = Math.round(ms / HOUR);
  if (hours >= 1) return NR_LAUNCH.GIVES_UP_IN(hours === 1 ? NR_SHARED.HOUR_ONE : NR_SHARED.HOURS(hours));
  const minutes = Math.ceil(ms / MINUTE);
  return NR_LAUNCH.GIVES_UP_IN(minutes === 1 ? NR_LAUNCH.MINUTE_ONE : NR_LAUNCH.MINUTES(minutes));
}

/** Why the run waits, in the server's own sentence for that reason. Undefined until the pool is known. */
export function waitingBody(reason: string, pool: { name: string; max?: number } | undefined): string | undefined {
  if (!pool) return undefined;
  if (reason === RUN_WAIT_REASON.NO_RUNNER_FREE) return RUN_WAIT.NO_RUNNER_FREE(pool.name);
  return pool.max === undefined ? undefined : NR_LAUNCH.WAITING_BODY(pool.name, pool.max);
}
