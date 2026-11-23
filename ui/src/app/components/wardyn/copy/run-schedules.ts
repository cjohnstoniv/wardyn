/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Repeating schedules: the Repeat choice on New Run's Run step and Review, the Schedules screen and the Runs
// header's link to it. Never re-exported through the copy.ts barrel. The server's own
// sentences (internal/api/reasons_run_schedules.go) are the words the console falls back to when a refusal comes
// back without one.

import type { ScheduleEvery } from "../../../lib/schedule";

export const RUN_SCHEDULE = {
  REPEAT: "Repeat",
  REPEAT_NONE: "Does not repeat",
  REPEAT_DAILY: "Every day",
  REPEAT_WEEKDAYS: "Every weekday",
  REPEAT_WEEKLY: (weekday: string) => `Every week on ${weekday}`,
  /** A rule in words, as the Schedules list's Repeats cell says it. */
  RULE: (every: ScheduleEvery, time: string, weekday: string) =>
    every === "daily" ? `Every day at ${time}` : every === "weekdays" ? `Every weekday at ${time}` : `Every week on ${weekday} at ${time}`,
  /** The Run step's line under the picker and Review's: "Repeats every weekday at 09:00 Europe/London; first start Mon 30 Nov". */
  REPEATS: (every: ScheduleEvery, time: string, weekday: string, zone: string, first: string) =>
    `Repeats ${every === "daily" ? "every day" : every === "weekdays" ? "every weekday" : `every week on ${weekday}`} at ${time} ${zone}; first start ${first}`,
  REPEAT_HELP: "Policy and your access are checked again at every start.",
  REPEAT_MISSING: "Choose a time.",
  CHECKED_EVERY: "Checked again at every start.",
  CREATE: "Create schedule",

  TITLE: "Schedules",
  LEDE: "Your repeating runs.",
  LEDE_ADMIN: "Every person's repeating runs.",
  LINK: "Schedules",
  COL_NAME: "Name",
  COL_OWNER: "Owner",
  COL_REPEATS: "Repeats",
  COL_NEXT: "Next start",
  COL_LAST: "Last start",
  COL_STATE: "State",
  ACTIVE: "Active",
  PAUSED: "Paused",
  PAUSE: "Pause",
  RESUME: "Resume",
  DELETE: "Delete",
  OPEN_RUN: "Open run",
  /** Per-row accessible names; the admin view names the owner too. */
  ROW_NAME: (verb: string, name: string, owner?: string) => (owner ? `${verb} ${name} by ${owner}` : `${verb} ${name}`),
  OPEN_LAST: (name: string, owner?: string) => (owner ? `Open the last run of ${name} by ${owner}` : `Open the last run of ${name}`),
  OUTCOME_BOOKED: "Booked",
  OUTCOME_MISSED: "Missed",
  OUTCOME_SKIPPED: "Skipped",
  OUTCOME_REFUSED: "Refused",
  OUTCOME_CANCELLED: "Cancelled",
  EMPTY_TITLE: "No schedules yet",
  EMPTY_BODY: "Choose Schedule for later and a repeat when you start a run.",
  OFF_BANNER: "Scheduled runs are off on this server, so none of these schedules will start until an admin turns them on.",
  RESUME_OFF_REASON: "Resume works again once scheduled runs are on.",
  BOOKED_NOTE: "Pausing or deleting a schedule does not cancel a start that is already booked. Cancel it on the Runs list.",
  BOOKED_NOTE_ADMIN: "Pausing or deleting a schedule does not cancel a start that is already booked. Open that run to cancel it.",
  DELETE_TITLE: (name: string) => `Delete ${name}?`,
  DELETE_BODY: "It will not start again.",
  DELETE_CONFIRM: "Delete schedule",
  CANCEL: "Cancel",

  // Pause reasons. The member form says what to do next; the off form drops it (Resume is unavailable while scheduled
  // runs are off); the admin form says only the owner can resume.
  PAUSED_REFUSED_LEAD: "Paused because a start was refused:",
  REFUSED_RESUME: "Resume it once that is fixed.",
  ONLY_OWNER: "Only its owner can resume it.",
} as const;

/** A pause reason's sentence in each form, by the reason the server stores. */
export const PAUSE_REASON: Record<string, { member: string; off: string; admin: string }> = {
  by_owner: {
    member: "You paused this schedule. Resume it to start it again.",
    off: "You paused this schedule.",
    admin: "Its owner paused this schedule. Only its owner can resume it.",
  },
  by_admin: {
    member: "An admin paused this schedule. Resume it to start it again.",
    off: "An admin paused this schedule.",
    admin: "An admin paused this schedule. Only its owner can resume it.",
  },
  owner_idle: {
    member: "Paused because you had not signed in for 30 days. Sign in, then resume it.",
    off: "Paused because you had not signed in for 30 days.",
    admin: "Paused because its owner has not signed in for 30 days. Only its owner can resume it.",
  },
  identity_inactive: {
    member: "Paused because your account was deactivated or you signed out everywhere. Resume it to start it again.",
    off: "Paused because your account was deactivated or you signed out everywhere.",
    admin: "Paused because its owner's account was deactivated or they signed out everywhere. Only its owner can resume it.",
  },
  groups_changed: {
    member: "Paused because your groups changed since this schedule was made. A new group can add limits. Sign in again, then resume it to confirm.",
    off: "Paused because your groups changed since this schedule was made. A new group can add limits.",
    admin: "Paused because its owner's groups changed since it was made. Only its owner can resume it.",
  },
  "refused:scheduled_runs_denied": {
    member: "Paused because a start was refused: your admin turned off scheduled runs for your team. Resume it once they are on again.",
    off: "Paused because a start was refused: your admin turned off scheduled runs for your team.",
    admin: "Paused because a start was refused: scheduled runs are off for its owner's team. Only its owner can resume it.",
  },
};

/** The server's sentences for the four schedule reasons, said when a refusal comes back with none. */
export const RUN_SCHEDULE_REFUSAL: Record<string, string> = {
  run_schedule_invalid:
    "A schedule needs every (daily, weekdays or weekly), time (HH:MM) and time_zone (an IANA zone such as Europe/London), and its request carries no start_at.",
  run_schedule_owner_unsupported: "Only a person signed in with single sign-on can keep a repeating schedule.",
  run_schedule_limit: "You have 20 schedules, the most allowed. Delete one first.",
  run_schedule_not_found: "No such schedule.",
};

/** The resume refusal's own sentence while scheduled runs are off. */
export const RESUME_OFF_SENTENCE = "Scheduled runs are off on this server, so this schedule can't resume.";
