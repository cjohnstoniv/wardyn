/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { NR_RUN } from "../components/wardyn/copy/new-run/run";
import { browserZone, daysAhead, scheduleDaysAhead, scheduledRunsOn, SCHEDULE_LEAD_WAIT_SEC, SCHEDULE_MAX_AHEAD_MS, startInstant, startProblem, startWire, todayDate, whenText, zoneOptions } from "./schedule";

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
    expect(startProblem(at("2026-10-21", "02:00", "UTC"), NOW)).toBeNull();
    expect(SCHEDULE_MAX_AHEAD_MS).toBe(7 * 24 * 3600_000);
  });

  it("leaves a background run's 24-hour lead wait out of the 7 days, as the server does", () => {
    // 7 days less 24 h from 2026-10-20 14:00 UTC is 2026-10-26 14:00 UTC.
    expect(startProblem(at("2026-10-26", "14:00", "UTC"), NOW)).toBeNull();
    expect(startProblem(at("2026-10-26", "14:01", "UTC"), NOW)).toBe("far");
    expect(startProblem(at("2026-10-27", "14:00", "UTC"), NOW)).toBe("far");
  });

  it("judges the start in its own zone", () => {
    // 14:30 London is 13:30 UTC: already past at 14:00 UTC.
    expect(startProblem(at("2026-10-20", "14:30", "Europe/London"), NOW)).toBe("past");
    expect(startProblem(at("2026-10-20", "14:30", "America/New_York"), NOW)).toBeNull();
  });
});

describe("the days ahead a start may be", () => {
  it("are the whole days in 7 days less the run type's lead wait", () => {
    expect(daysAhead(0)).toBe(7);
    expect(daysAhead(24 * 3600)).toBe(6);
    expect(daysAhead(3600)).toBe(6);
    expect(daysAhead(6 * 86_400)).toBe(1);
    expect(daysAhead(6 * 86_400 + 1)).toBe(0);
  });

  it("come from the server's lead wait for each run type: Autonomous is a background run", () => {
    expect(scheduleDaysAhead()).toBe(6);
    expect(scheduleDaysAhead(false)).toBe(6);
    expect(scheduleDaysAhead(true)).toBe(6);
  });

  it("read as '1 day' for one and 'N days' otherwise, in the help and the too-far sentence", () => {
    expect(NR_RUN.START_HELP(6)).toBe("Up to 6 days ahead. Policy and your access are checked again when it starts.");
    expect(NR_RUN.START_HELP(1)).toBe("Up to 1 day ahead. Policy and your access are checked again when it starts.");
    expect(NR_RUN.START_FAR(6)).toBe("Choose a time up to 6 days ahead.");
    expect(NR_RUN.START_FAR(1)).toBe("Choose a time up to 1 day ahead.");
  });
});

describe("the lead wait is the server's", () => {
  const root = (() => {
    let dir = resolve(process.cwd());
    for (let i = 0; i < 8; i++) {
      if (existsSync(join(dir, "go.mod"))) return dir;
      dir = dirname(dir);
    }
    throw new Error("go.mod not found");
  })();
  const go = (file: string) => readFileSync(join(root, file), "utf8");
  /** A Go constant written as a product of integers ("24 * 60 * 60"). */
  const seconds = (src: string, name: string): number => {
    const expr = new RegExp(`\\b${name}\\s*=\\s*([0-9 *]+)`).exec(src)?.[1] ?? "";
    return expr.split("*").reduce((n, f) => n * Number(f.trim()), 1);
  };

  it("is the default wait of each run type in run_queue.go, and the cap its longest wait", () => {
    const queue = go("internal/api/run_queue.go");
    expect(SCHEDULE_LEAD_WAIT_SEC.background).toBe(seconds(queue, "queueBackgroundWaitSec"));
    expect(SCHEDULE_LEAD_WAIT_SEC.interactive).toBe(seconds(queue, "queueInteractiveWaitSec"));
    expect(SCHEDULE_MAX_AHEAD_MS).toBe(seconds(queue, "queueMaxWaitSec") * 1000);
  });

  it("is what run_schedule.go leaves, by run type, and is the cap less that wait, not the run's own max_wait_sec", () => {
    const sched = go("internal/api/run_schedule.go");
    const fn = /func scheduleLeadWaitSec\(interactive bool\) int \{[\s\S]*?\n\}/.exec(sched)?.[0] ?? "";
    expect(fn).toContain("if interactive {\n\t\treturn queueInteractiveWaitSec\n\t}\n\treturn queueBackgroundWaitSec");
    expect(sched).toContain("latest := now.Add(scheduleMaxAhead - time.Duration(waitSec)*time.Second)");
    expect(sched).toContain("scheduleMaxAhead = queueMaxWaitSec * time.Second");
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
  it("offers the browser's zone first, then UTC, each once, whichever zone is picked", () => {
    for (const picked of [undefined, "Europe/London", "Pacific/Kiritimati"]) {
      const zones = zoneOptions(picked);
      expect(zones[0]).toBe(browserZone());
      if (browserZone() !== "UTC") expect(zones[1]).toBe("UTC");
      expect(new Set(zones).size).toBe(zones.length);
      if (picked) expect(zones).toContain(picked);
    }
  });

  it("keeps a picked zone the browser does not list, last, without moving the rest", () => {
    const base = zoneOptions();
    expect(zoneOptions("Mars/Olympus")).toEqual([...base, "Mars/Olympus"]);
  });
});

describe("todayDate", () => {
  it("is the date in the zone asked for, not the browser's", () => {
    const now = Date.UTC(2026, 9, 20, 23, 30);
    expect(todayDate("UTC", now)).toBe("2026-10-20");
    expect(todayDate("Pacific/Kiritimati", now)).toBe("2026-10-21");
    expect(todayDate("Pacific/Pago_Pago", now)).toBe("2026-10-20");
    expect(todayDate("America/Los_Angeles", Date.UTC(2026, 9, 20, 5, 0))).toBe("2026-10-19");
  });

  it("reads an unknown zone as the browser's", () => {
    expect(todayDate("Mars/Olympus", Date.UTC(2026, 9, 20, 12, 0))).toBe(todayDate(undefined, Date.UTC(2026, 9, 20, 12, 0)));
  });
});
