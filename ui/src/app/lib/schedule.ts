/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A scheduled run's start time: what the New Run picker holds (a date, a time and the zone they are in), whether
// it is a start the server will take, and how a start reads in the console ("Wed 21 Oct, 02:00 (UTC)").
// The server owns the final say (start_at_past, start_at_too_far); this is the pre-check and the words.

import type { SetupStatus } from "./types/setup";

/** What the picker holds. A date and a time are "" until typed; zone is an IANA name. */
export interface StartChoice {
  date: string;
  time: string;
  zone: string;
}

/** The furthest ahead a start may be: the queue's longest wait. */
export const SCHEDULE_MAX_AHEAD_MS = 7 * 24 * 3600_000;

/** WARDYN_SCHEDULED_RUNS, as the setup status carries it. Absent reads as off. */
export const scheduledRunsOn = (status: Pick<SetupStatus, "features"> | null | undefined): boolean =>
  status?.features?.scheduled_runs === true;

/** The browser's own time zone, where the picker starts. */
export function browserZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

/**
 * The zones the picker offers: the browser's first, then UTC, then every other zone the browser knows. A picked
 * zone the browser does not list is added last; the pick is the select's value and never moves the list.
 */
export function zoneOptions(picked?: string): string[] {
  let known: string[] = [];
  try {
    known = (Intl as unknown as { supportedValuesOf?: (key: string) => string[] }).supportedValuesOf?.("timeZone") ?? [];
  } catch {
    known = [];
  }
  return [...new Set([browserZone(), "UTC", ...known, ...(picked ? [picked] : [])])];
}

/** The picker as the person finds it: nothing typed, in the browser's zone. */
export const emptyStart = (): StartChoice => ({ date: "", time: "", zone: browserZone() });

// How far `zone`'s wall clock is ahead of UTC at the instant `ms`.
function offsetMs(zone: string, ms: number): number {
  const parts = new Intl.DateTimeFormat("en-US", {
    timeZone: zone, hourCycle: "h23", year: "numeric", month: "numeric", day: "numeric", hour: "numeric", minute: "numeric", second: "numeric",
  }).formatToParts(new Date(ms));
  const n = (type: string) => Number(parts.find((p) => p.type === type)?.value);
  return Date.UTC(n("year"), n("month") - 1, n("day"), n("hour"), n("minute"), n("second")) - Math.floor(ms / 1000) * 1000;
}

/** The instant (ms since the epoch) a wall-clock date and time name in `zone`, or null when they name none. */
export function startInstant(choice: StartChoice): number | null {
  const d = /^(\d{4})-(\d{2})-(\d{2})$/.exec(choice.date);
  const t = /^(\d{2}):(\d{2})$/.exec(choice.time);
  if (!d || !t) return null;
  const wall = Date.UTC(Number(d[1]), Number(d[2]) - 1, Number(d[3]), Number(t[1]), Number(t[2]));
  try {
    // A zone's offset can change within the day, so the guess is corrected once with the offset found at the guess.
    const first = wall - offsetMs(choice.zone, wall);
    return wall - offsetMs(choice.zone, first);
  } catch {
    return null;
  }
}

export type StartProblem = "missing" | "past" | "far";

/**
 * The wait a scheduled start must leave before the 7-day cap, as the server reads it (scheduleLeadWaitSec in
 * internal/api/run_schedule.go): the run type's default wait, never the run's own max_wait_sec. schedule.test.ts
 * keeps these two equal to the Go constants.
 */
export const SCHEDULE_LEAD_WAIT_SEC = { background: 24 * 60 * 60, interactive: 60 * 60 } as const;

/**
 * Why a start is not one the server will take, or null when it looks fine. Only an Autonomous (background) run
 * can be scheduled, so the latest start is 7 days less a background run's lead wait.
 */
export function startProblem(choice: StartChoice, now: number): StartProblem | null {
  const at = startInstant(choice);
  if (at === null) return "missing";
  if (at <= now) return "past";
  if (at - now > SCHEDULE_MAX_AHEAD_MS - SCHEDULE_LEAD_WAIT_SEC.background * 1000) return "far";
  return null;
}

/** The start_at and start_time_zone a create carries. */
export interface StartWire {
  start_at: string;
  start_time_zone: string;
}

/** The wall clock, read by the server in the zone. */
export const startWire = (choice: StartChoice): StartWire => ({
  start_at: `${choice.date}T${choice.time}`,
  start_time_zone: choice.zone,
});

/**
 * An instant as the console says a start: "Wed 21 Oct, 02:00 (UTC)", in the zone it was given in and with that
 * zone's name. English day and month names and a 24-hour clock whatever the browser's language.
 */
export function whenText(iso: string | number, zone: string): string {
  const ms = typeof iso === "number" ? iso : Date.parse(iso);
  let parts: Intl.DateTimeFormatPart[];
  try {
    parts = new Intl.DateTimeFormat("en-GB", {
      timeZone: zone, weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit", hourCycle: "h23",
    }).formatToParts(new Date(ms));
  } catch {
    return whenText(ms, "UTC");
  }
  const p = (type: string) => parts.find((x) => x.type === type)?.value ?? "";
  return `${p("weekday")} ${p("day")} ${p("month")}, ${p("hour")}:${p("minute")} (${zone})`;
}

/** Today's date in `zone`, as the date field's earliest choice. An unknown zone reads as the browser's own. */
export function todayDate(zone?: string, now: number = Date.now()): string {
  let parts: Intl.DateTimeFormatPart[];
  try {
    parts = new Intl.DateTimeFormat("en-GB", { timeZone: zone, year: "numeric", month: "2-digit", day: "2-digit" }).formatToParts(new Date(now));
  } catch {
    return todayDate(undefined, now);
  }
  const p = (type: string) => parts.find((x) => x.type === type)?.value ?? "";
  return `${p("year")}-${p("month")}-${p("day")}`;
}
