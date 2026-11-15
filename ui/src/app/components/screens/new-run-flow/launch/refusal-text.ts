/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { HttpError } from "../../../../lib/api/core";
import { getErrorMessage } from "../../../../lib/format";
import { scheduleDaysAhead } from "../../../../lib/schedule";
import { isScheduleReason, SCHEDULE_REFUSAL, type ScheduleReason } from "../../../../lib/schedule-refusals";
import { NR_RUN } from "../../../wardyn/copy/new-run/run";

/** The words for each start-time refusal when the server sent none. Past and too far name no time: the server's sentence does. */
const SCHEDULE_TEXT: Record<ScheduleReason, string> = {
  scheduled_runs_off: SCHEDULE_REFUSAL.OFF(),
  scheduled_runs_unavailable: SCHEDULE_REFUSAL.UNAVAILABLE(),
  start_at_invalid: SCHEDULE_REFUSAL.INVALID(),
  start_at_past: NR_RUN.START_PAST,
  start_at_too_far: NR_RUN.START_FAR(scheduleDaysAhead()),
};

/**
 * What the Name dialog says for a refused create: the server's own sentence, except a start-time refusal that came
 * back with no sentence (empty, or the code alone), which never shows its code, and a start the server's clocks
 * skip or repeat (detail clock_change, startAtClockChangeDetail in internal/api/run_schedule.go): the server's
 * sentence also offers an RFC 3339 offset, which the console never sends, so the console says its own.
 */
export function refusalText(e: unknown): string {
  if (e instanceof HttpError && e.reason === "start_at_invalid" && e.detail === "clock_change") return NR_RUN.START_CLOCK;
  // "" fallback: a blank message must reach the code-alone check below, not become the generic sentence.
  const message = getErrorMessage(e, "");
  if (e instanceof HttpError && isScheduleReason(e.reason) && !/\s/.test(message.trim())) return SCHEDULE_TEXT[e.reason];
  return message;
}
