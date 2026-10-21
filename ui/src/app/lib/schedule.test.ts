/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { scheduledRunsOn, SCHEDULE_MAX_AHEAD_MS, startInstant, startProblem, startWire, whenText, zoneOptions } from "./schedule";

const at = (date: string, time: string, zone: string) => ({ date, time, zone });
const NOW = Date.UTC(2026, 9, 20, 14, 0);

describe("scheduledRunsOn", () => {
  it("is on only when the setup status says features.scheduled_runs is true", () => {
    expect(scheduledRunsOn({ features: { scheduled_runs: true } })).toBe(true);
    expect(scheduledRunsOn({ features: { scheduled_runs: false } })).toBe(false);
    expect(scheduledRunsOn({})).toBe(false);
    expect(scheduledRunsOn(null)).toBe(false);
    expect(scheduledRunsOn(undefined)).toBe(false);
  });
});

describe("startInstant", () => {
  it("reads a wall clock in the zone it was given in", () => {
    expect(startInstant(at("2026-10-21", "02:00", "UTC"))).toBe(Date.UTC(2026, 9, 21, 2, 0));
    // London is on summer time until 25 Oct 2026, New York until 1 Nov.
    expect(startInstant(at("2026-10-21", "02:00", "Europe/London"))).toBe(Date.UTC(2026, 9, 21, 1, 0));
    expect(startInstant(at("2026-10-21", "02:00", "America/New_York"))).toBe(Date.UTC(2026, 9, 21, 6, 0));
    expect(startInstant(at("2026-12-01", "02:00", "Europe/London"))).toBe(Date.UTC(2026, 11, 1, 2, 0));
  });

  it("names no instant for a date or time not typed, or a zone that does not exist", () => {
    expect(startInstant(at("", "02:00", "UTC"))).toBeNull();
    expect(startInstant(at("2026-10-21", "", "UTC"))).toBeNull();
    expect(startInstant(at("2026-10-21", "02:00", "Mars/Olympus"))).toBeNull();
  });
});

describe("startProblem", () => {
  it("says missing, past and too far ahead, and nothing for a start in between", () => {
    expect(startProblem(at("", "", "UTC"), NOW)).toBe("missing");
    expect(startProblem(at("2026-10-20", "14:00", "UTC"), NOW)).toBe("past");
    expect(startProblem(at("2026-10-20", "13:59", "UTC"), NOW)).toBe("past");
    expect(startProblem(at("2026-10-27", "14:01", "UTC"), NOW, 0)).toBe("far");
    expect(startProblem(at("2026-10-27", "14:00", "UTC"), NOW, 0)).toBeNull();
    expect(startProblem(at("2026-10-21", "02:00", "UTC"), NOW)).toBeNull();
    expect(SCHEDULE_MAX_AHEAD_MS).toBe(7 * 24 * 3600_000);
  });

  it("keeps the run's own wait out of the 7 days, as the server answered it, and never guesses one", () => {
    // 86400 s of wait: the latest start is 6 days out, 2026-10-26 14:00 UTC.
    expect(startProblem(at("2026-10-26", "14:01", "UTC"), NOW, 86_400)).toBe("far");
    expect(startProblem(at("2026-10-26", "14:00", "UTC"), NOW, 86_400)).toBeNull();
    // An Interactive run's 3600 s leaves nearly the whole week.
    expect(startProblem(at("2026-10-26", "14:01", "UTC"), NOW, 3600)).toBeNull();
    expect(startProblem(at("2026-10-27", "12:01", "UTC"), NOW, 3600)).toBe("far");
    // No answer yet: no far check at all, the create's start_at_too_far answers.
    expect(startProblem(at("2026-12-01", "14:00", "UTC"), NOW)).toBeNull();
    expect(startProblem(at("2026-12-01", "14:00", "UTC"), NOW, undefined)).toBeNull();
  });

  it("judges the start in its own zone", () => {
    // 14:30 London is 13:30 UTC: already past at 14:00 UTC.
    expect(startProblem(at("2026-10-20", "14:30", "Europe/London"), NOW)).toBe("past");
    expect(startProblem(at("2026-10-20", "14:30", "America/New_York"), NOW)).toBeNull();
  });
});

describe("startWire", () => {
  it("sends the wall clock and the zone, for the server to read", () => {
    expect(startWire(at("2026-10-21", "02:00", "Europe/London"))).toEqual({ start_at: "2026-10-21T02:00", start_time_zone: "Europe/London" });
  });
});

describe("whenText", () => {
  it("says a start the way the boards draw it, in the zone it was given in", () => {
    expect(whenText("2026-10-21T02:00:00Z", "UTC")).toBe("Wed 21 Oct, 02:00 (UTC)");
    expect(whenText("2026-10-21T02:00:00Z", "Europe/London")).toBe("Wed 21 Oct, 03:00 (Europe/London)");
    expect(whenText(Date.UTC(2026, 9, 27, 14, 0), "UTC")).toBe("Tue 27 Oct, 14:00 (UTC)");
  });
});

describe("zoneOptions", () => {
  it("offers the browser's zone first, then UTC, each once", () => {
    const zones = zoneOptions("Europe/London");
    expect(zones.slice(0, 2)).toEqual(["Europe/London", "UTC"]);
    expect(new Set(zones).size).toBe(zones.length);
  });
});
