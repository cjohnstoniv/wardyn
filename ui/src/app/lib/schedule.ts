/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// A scheduled run's start time: what the New Run picker holds (a date, a time and the zone they are in), whether
// it is a start the server will take, and how a start reads in the console ("Wed 21 Oct, 02:00 (UTC)").
// The server owns the final say (start_at_past, start_at_too_far); this is the pre-check and the words.

import type { SetupStatus } from "./types/setup";

/** How often a schedule repeats, as POST /run-schedules' `every` takes it. */
export type ScheduleEvery = "daily" | "weekdays" | "weekly";

/**
 * What the picker holds. A date and a time are "" until typed; zone is an IANA name. With `repeat` set the run is
 * a repeating schedule: the date is not used, and the first start is the next time the clock comes round.
 */
export interface StartChoice {
  date: string;
  time: string;
  zone: string;
  repeat?: ScheduleEvery;
}

/** The furthest ahead a start may be: the queue's longest wait. */
export const SCHEDULE_MAX_AHEAD_MS = 7 * 24 * 3600_000;

/**
 * WARDYN_SCHEDULED_RUNS as the server said it: true or false once it has answered (a daemon that sends no features
 * has the switch off), undefined while nothing it said is known: no status yet, or the stand-in the console makes
 * up when the server cannot be reached.
 */
export const scheduledRuns = (status: Pick<SetupStatus, "features" | "unreachable"> | null | undefined): boolean | undefined =>
  !status || status.unreachable ? undefined : status.features?.scheduled_runs === true;

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

// The typed date and time as one number, read as if they were UTC, or null until both are typed.
function wallClock(choice: StartChoice): number | null {
  const d = /^(\d{4})-(\d{2})-(\d{2})$/.exec(choice.date);
  const t = /^(\d{2}):(\d{2})$/.exec(choice.time);
  return d && t ? Date.UTC(Number(d[1]), Number(d[2]) - 1, Number(d[3]), Number(t[1]), Number(t[2])) : null;
}

/**
 * Every instant whose wall clock in the choice's zone reads as the one typed: one, none in the hour the clocks skip
 * (spring forward) or two in the hour they repeat (fall back). null until a date and time are typed, or for a zone
 * the browser does not know. internal/api/run_schedule.go refuses the same wall times.
 */
function wallInstants(choice: StartChoice): number[] | null {
  const wall = wallClock(choice);
  if (wall === null) return null;
  try {
    // The zone's offsets a day either side cover any change near the wall time; an offset whose instant reads
    // back as the typed wall clock is a match.
    const offsets = new Set([wall - 86_400_000, wall, wall + 86_400_000].map((ms) => offsetMs(choice.zone, ms)));
    return [...offsets].map((o) => wall - o).filter((at) => at + offsetMs(choice.zone, at) === wall);
  } catch {
    return null;
  }
}

/** The instant (ms since the epoch) a wall-clock date and time name in `zone`, or null when they name none or two. */
export function startInstant(choice: StartChoice): number | null {
  const at = wallInstants(choice);
  return at?.length === 1 ? at[0] : null;
}

export type StartProblem = "missing" | "clock" | "past" | "far";

/**
 * The wait a scheduled start must leave before the 7-day cap, as the server reads it (scheduleLeadWaitSec in
 * internal/api/run_schedule.go): the run type's default wait, never the run's own max_wait_sec. schedule.test.ts
 * keeps these two equal to the Go constants.
 */
export const SCHEDULE_LEAD_WAIT_SEC = { background: 24 * 60 * 60, interactive: 60 * 60 } as const;

/** Whole days ahead a start may be, given the wait it must leave before the 7-day cap. */
export const daysAhead = (leadWaitSec: number): number => Math.floor((SCHEDULE_MAX_AHEAD_MS - leadWaitSec * 1000) / 86_400_000);

/** The n in "Up to {n} days ahead": the whole days a run of this type may be scheduled ahead (Autonomous is a background run). */
export const scheduleDaysAhead = (interactive = false): number =>
  daysAhead(interactive ? SCHEDULE_LEAD_WAIT_SEC.interactive : SCHEDULE_LEAD_WAIT_SEC.background);

/**
 * Why a start is not one the server will take, or null when it looks fine: not typed, a wall time the clocks skip
 * or repeat, past, or too far. Only an Autonomous (background) run can be scheduled, so the latest start is 7 days
 * less a background run's lead wait.
 */
export function startProblem(choice: StartChoice, now: number): StartProblem | null {
  const instants = wallInstants(choice);
  if (instants === null) return "missing";
  if (instants.length !== 1) return "clock";
  const at = instants[0];
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

/** Why a repeating start is not one to send: no time typed yet. The server picks the first start. */
export const repeatProblem = (choice: StartChoice): StartProblem | null => (/^([01]\d|2[0-3]):[0-5]\d$/.test(choice.time) ? null : "missing");

// The day of the week of a yyyy-mm-dd date, 0 for Sunday.
const dateWeekday = (date: string): number => {
  const [y, m, d] = date.split("-").map(Number);
  return new Date(Date.UTC(y, m - 1, d)).getUTCDay();
};

/**
 * A repeating choice's first start: the next instant after now its time comes round in its zone, on a weekday
 * (Monday to Friday) for a weekdays rule, as internal/api/run_schedule_repeat.go's firstSlot finds it. null until a
 * time is typed. A day whose wall time the clocks skip or repeat is passed over here; the server books it anyway.
 */
export function firstRepeatStart(choice: StartChoice, now: number): number | null {
  if (repeatProblem(choice)) return null;
  for (let d = 0; d <= 8; d++) {
    const date = todayDate(choice.zone, now + d * 86_400_000);
    if (choice.repeat === "weekdays" && [0, 6].includes(dateWeekday(date))) continue;
    const at = startInstant({ date, time: choice.time, zone: choice.zone });
    if (at !== null && at > now) return at;
  }
  return null;
}

function zoneParts(ms: number, zone: string, opts: Intl.DateTimeFormatOptions): (type: string) => string {
  let parts: Intl.DateTimeFormatPart[];
  try {
    parts = new Intl.DateTimeFormat("en-GB", { timeZone: zone, ...opts }).formatToParts(new Date(ms));
  } catch {
    parts = new Intl.DateTimeFormat("en-GB", { timeZone: "UTC", ...opts }).formatToParts(new Date(ms));
  }
  return (type) => parts.find((x) => x.type === type)?.value ?? "";
}

/** An instant's date in a zone, as a first start says it: "Mon 30 Nov". */
export function dayText(ms: number, zone: string): string {
  const p = zoneParts(ms, zone, { weekday: "short", day: "numeric", month: "short" });
  return `${p("weekday")} ${p("day")} ${p("month")}`;
}

/** An instant's date and time in a zone, without the zone's name: "Fri 27 Nov, 09:00". */
export function dayTimeText(iso: string, zone: string): string {
  const p = zoneParts(Date.parse(iso), zone, { weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit", hourCycle: "h23" });
  return `${p("weekday")} ${p("day")} ${p("month")}, ${p("hour")}:${p("minute")}`;
}

const WEEKDAYS = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"] as const;

/** A weekday's English name, 0 for Sunday, as a weekly schedule's `weekday` counts. */
export const weekdayName = (day: number): string => WEEKDAYS[((day % 7) + 7) % 7];

/** The weekday an instant falls on in a zone, 0 for Sunday. */
export const zoneWeekday = (ms: number, zone: string): number => dateWeekday(todayDate(zone, ms));
