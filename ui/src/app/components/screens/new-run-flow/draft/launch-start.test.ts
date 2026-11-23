/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { SCHEDULE_REFUSAL } from "../../../../lib/schedule-refusals";
import { NR_RUN } from "../../../wardyn/copy/new-run/run";
import { launchStart, repeatLine, startHeld, startHeldText } from "./launch-start";
import type { RunIntent } from "./run-intent";

const NOW = Date.UTC(2026, 9, 20, 14, 0);
const schedule = { date: "2026-10-21", time: "02:00", zone: "UTC" };
const AUTONOMOUS: RunIntent = { shape: "agent_task", experience: "background" };

describe("launchStart", () => {
  it("is the schedule of an Autonomous agent run while the switch is on", () => {
    expect(launchStart({ intent: AUTONOMOUS, schedule }, true, NOW)).toEqual({ choice: schedule, problem: null });
    expect(launchStart({ intent: AUTONOMOUS, schedule: { ...schedule, date: "2026-10-19" } }, true, NOW)?.problem).toBe("past");
  });

  it("calls a start past 7 days less a background run's lead wait too far, with nothing else to wait for", () => {
    const week = { ...schedule, date: "2026-10-26", time: "20:00" };
    expect(launchStart({ intent: AUTONOMOUS, schedule: week }, true, NOW)?.problem).toBe("far");
    expect(launchStart({ intent: AUTONOMOUS, schedule: { ...week, time: "13:59" } }, true, NOW)?.problem).toBeNull();
  });

  it("is nothing when the switch is off or absent, and a Later start the draft still holds is held, not dropped", () => {
    expect(launchStart({ intent: AUTONOMOUS, schedule }, false, NOW)).toBeUndefined();
    expect(startHeld({ intent: AUTONOMOUS, schedule }, false)).toBe("off");
    expect(startHeld({ intent: AUTONOMOUS, schedule }, true)).toBeNull();
    expect(startHeld({ intent: AUTONOMOUS }, false)).toBeNull();
    expect(startHeld({ intent: { shape: "agent_task", experience: "interactive" }, schedule }, false)).toBeNull();
  });

  it("holds a Later start as not known, never as off, while the server has not said whether the switch is on", () => {
    expect(launchStart({ intent: AUTONOMOUS, schedule }, undefined, NOW)).toBeUndefined();
    expect(startHeld({ intent: AUTONOMOUS, schedule }, undefined)).toBe("unknown");
    expect(startHeld({ intent: AUTONOMOUS }, undefined)).toBeNull();
    expect(startHeldText("unknown")).toBe(NR_RUN.START_HELD_UNKNOWN);
    expect(startHeldText("off")).toBe(SCHEDULE_REFUSAL.OFF());
  });

  it("is nothing for a run that is not an Autonomous agent run, or one with no schedule", () => {
    expect(launchStart({ intent: { shape: "agent_task", experience: "interactive" }, schedule }, true, NOW)).toBeUndefined();
    expect(launchStart({ intent: { shape: "application", experience: "background" }, schedule }, true, NOW)).toBeUndefined();
    expect(launchStart({ intent: AUTONOMOUS }, true, NOW)).toBeUndefined();
  });
});

describe("a repeating start", () => {
  const repeat = { date: "", time: "09:00", zone: "Europe/London" };

  it("needs only a time: the server picks the first start, so no date and no 7-day cap", () => {
    expect(launchStart({ intent: AUTONOMOUS, schedule: { ...repeat, repeat: "daily" } }, true, NOW)?.problem).toBeNull();
    expect(launchStart({ intent: AUTONOMOUS, schedule: { ...repeat, date: "2026-10-19", repeat: "daily" } }, true, NOW)?.problem).toBeNull();
    expect(launchStart({ intent: AUTONOMOUS, schedule: { ...repeat, time: "", repeat: "daily" } }, true, NOW)?.problem).toBe("missing");
  });

  it("reads as its rule and first start, in the zone", () => {
    // NOW is Tue 20 Oct, 15:00 in London: 09:00 has passed, so the first start is the next day's.
    expect(repeatLine({ ...repeat, repeat: "weekdays" }, NOW)).toBe("Repeats every weekday at 09:00 Europe/London; first start Wed 21 Oct");
    expect(repeatLine({ ...repeat, repeat: "daily" }, NOW)).toBe("Repeats every day at 09:00 Europe/London; first start Wed 21 Oct");
    expect(repeatLine({ ...repeat, repeat: "weekly" }, NOW)).toBe("Repeats every week on Wednesday at 09:00 Europe/London; first start Wed 21 Oct");
  });

  it("skips the weekend for a weekdays rule", () => {
    const friday = Date.UTC(2026, 9, 23, 12, 0);
    expect(repeatLine({ ...repeat, repeat: "weekdays" }, friday)).toBe("Repeats every weekday at 09:00 Europe/London; first start Mon 26 Oct");
    expect(repeatLine({ ...repeat, repeat: "daily" }, friday)).toBe("Repeats every day at 09:00 Europe/London; first start Sat 24 Oct");
  });

  it("is no line for a one-time start, or before a time is typed", () => {
    expect(repeatLine(schedule, NOW)).toBeNull();
    expect(repeatLine({ ...repeat, time: "", repeat: "daily" }, NOW)).toBeNull();
  });
});
