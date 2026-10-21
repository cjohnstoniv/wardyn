/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The refusals a create answers with when it schedules a run, in the bytes of internal/api/run_schedule.go
// (schedule-refusals.test.ts reads that file). The console prints the server's own sentence on a refusal; these
// are the words it falls back to, so a refusal never shows its reason code.

/** The reasons of internal/api/reasons.go that a start time can draw. */
export const SCHEDULE_REASONS = [
  "scheduled_runs_off",
  "scheduled_runs_unavailable",
  "start_at_invalid",
  "start_at_past",
  "start_at_too_far",
] as const;

export type ScheduleReason = (typeof SCHEDULE_REASONS)[number];

export const SCHEDULE_REFUSAL = {
  OFF: () => "Scheduled runs are off on this deployment (WARDYN_SCHEDULED_RUNS is not set). Start the run now, or ask an admin to turn scheduled runs on.",
  UNAVAILABLE: () => "This deployment places no runs on runners, so a run cannot be scheduled here.",
  INVALID: () => "start_at needs a date and time (2026-10-21T02:00, or RFC 3339) and start_time_zone an IANA time zone (Europe/London).",
  PAST: (shown: string) => `The start time ${shown} has already passed. Choose a time in the future.`,
  TOO_FAR: (shown: string, latest: string, waitHours: number) =>
    `The start time ${shown} is too far ahead. The latest start is ${latest}: 7 days from now, less this run's ${waitHours}-hour wait. Choose an earlier time.`,
} as const;

/** The reason each sentence belongs to; the console matches on these, never on the text. */
export const SCHEDULE_REFUSAL_REASON: Record<keyof typeof SCHEDULE_REFUSAL, ScheduleReason> = {
  OFF: "scheduled_runs_off",
  UNAVAILABLE: "scheduled_runs_unavailable",
  INVALID: "start_at_invalid",
  PAST: "start_at_past",
  TOO_FAR: "start_at_too_far",
};

export const isScheduleReason = (reason: string): reason is ScheduleReason => (SCHEDULE_REASONS as readonly string[]).includes(reason);
