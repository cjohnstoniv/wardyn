/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join, relative } from "node:path";
import { formatDuration } from "./format";
import { timeLeft } from "../components/screens/runners/runner-display";

describe("formatDuration", () => {
  it("short: hours and minutes from an hour up, minutes and seconds below", () => {
    expect(formatDuration(3723_000)).toBe("1h 2m");
    expect(formatDuration(312_000)).toBe("5m 12s");
    expect(formatDuration(-5)).toBe("0m 0s");
  });

  it("words: the largest whole unit", () => {
    const w = (sec: number) => formatDuration(sec * 1000, "words");
    expect([w(3600), w(86400), w(1800), w(172800), w(90), w(60)]).toEqual([
      "1 hour",
      "1 day",
      "30 minutes",
      "2 days",
      "90 seconds",
      "1 minute",
    ]);
  });

  it("clock: a player position", () => {
    expect(formatDuration(247_000, "clock")).toBe("4:07");
    expect(formatDuration(0, "clock")).toBe("0:00");
  });

  it("time left rounds to the nearest hour from an hour up", () => {
    const now = Date.now();
    expect(timeLeft(new Date(now + 40 * 60_000).toISOString(), now)).toBe("40 minutes");
    expect(timeLeft(new Date(now + 23 * 3600_000).toISOString(), now)).toBe("23 hours");
    expect(timeLeft(new Date(now + 24 * 3600_000).toISOString(), now)).toBe("24 hours");
  });
});

// One home for dates, times and durations (#3085): a screen that formats a
// timestamp calls lib/format.ts, so the next date fix is one edit. The ADO PAT
// clock is British 24-hour copy on purpose and is the one exception.
describe("date and duration formatting lives in lib/format.ts", () => {
  const root = join(__dirname, "..");
  const sources = (dir: string): string[] =>
    readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
      const p = join(dir, e.name);
      if (e.isDirectory()) return sources(p);
      return /\.tsx?$/.test(e.name) && !/\.test\.tsx?$/.test(e.name) ? [p] : [];
    });
  const files = sources(root).filter((p) => !["lib/format.ts", "lib/ado-pat-display.ts"].includes(relative(root, p)));

  it("no inline toLocale(Date|Time)String", () => {
    const offenders = files.filter((p) => /\.toLocale(Date|Time)String\(/.test(readFileSync(p, "utf8"))).map((p) => relative(root, p));
    expect(offenders).toEqual([]);
  });

  it("no second duration helper", () => {
    const offenders = files.filter((p) => /function (formatElapsed|formatDuration|fmtDur)\b/.test(readFileSync(p, "utf8"))).map((p) => relative(root, p));
    expect(offenders).toEqual([]);
  });
});
