/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

/** The governance profile's deny_scheduled_runs door (internal/types/governance.go DenyScheduledRuns). */
export const SCHEDULE_DOOR = {
  LABEL: "Deny scheduled runs",
  HINT: "Stops people on this profile giving a run a start time, and ends a run already booked when its time comes. Has no effect while scheduled runs are off for this install.",
  CHIP: "Can't schedule runs",
} as const;
