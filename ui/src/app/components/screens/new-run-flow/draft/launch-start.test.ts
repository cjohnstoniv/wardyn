/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { SCHEDULE_REFUSAL } from "../../../../lib/schedule-refusals";
import { NR_RUN } from "../../../wardyn/copy/new-run/run";
import { firstRepeatStart, zoneWeekday } from "../../../../lib/schedule";
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

// The first start and the weekday a weekly schedule repeats on, as the server's firstSlot stores them
// (internal/api/run_schedule_repeat.go, time.Date over calendar days), around Europe/London's clock changes.
describe("a repeating start's first slot across a clock change", () => {
  const london = (time: string, repeat: "daily" | "weekly" = "weekly") => ({ date: "", time, zone: "Europe/London", repeat });
  const first = (now: number, time: string) => {
    const at = firstRepeatStart(london(time), now);
    return at === null ? null : { at: new Date(at).toISOString(), weekday: zoneWeekday(at, "Europe/London") };
  };

  it("takes the next calendar day in the last hour before spring forward, not the day after", () => {
    // Sat 27 Mar 2027 23:30 GMT; 09:00 on Sun 28 Mar is 08:00Z (BST).
    expect(first(Date.UTC(2027, 2, 27, 23, 30), "09:00")).toEqual({ at: "2027-03-28T08:00:00.000Z", weekday: 0 });
    expect(firstRepeatStart(london("09:00", "daily"), Date.UTC(2027, 2, 27, 23, 30))).toBe(Date.UTC(2027, 2, 28, 8, 0));
  });

  it("moves a wall time the clocks skip forward by the gap, on that same day", () => {
    // 01:30 on Sun 28 Mar 2027 does not exist in London; the server starts at 02:30 BST, 01:30Z.
    expect(first(Date.UTC(2027, 2, 27, 12, 0), "01:30")).toEqual({ at: "2027-03-28T01:30:00.000Z", weekday: 0 });
  });

  it("books a wall time the clocks repeat on that same day", () => {
    // 01:30 on Sun 31 Oct 2027 happens twice in London; the server starts at 01:30 GMT, 01:30Z.
    expect(first(Date.UTC(2027, 9, 30, 12, 0), "01:30")).toEqual({ at: "2027-10-31T01:30:00.000Z", weekday: 0 });
  });

  it("is the next day when the time has just passed, with no clock change near", () => {
    // Mon 23 Nov 2026 09:01 GMT; 09:00 is next on Tue 24 Nov.
    expect(first(Date.UTC(2026, 10, 23, 9, 1), "09:00")).toEqual({ at: "2026-11-24T09:00:00.000Z", weekday: 2 });
  });
});
