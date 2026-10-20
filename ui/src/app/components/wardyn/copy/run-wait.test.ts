/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { readFileSync, readdirSync } from "node:fs";
import { join, relative } from "node:path";
import { RUN_COCKPIT } from "./run-cockpit";
import { RUN_WAIT } from "./run-wait";
import { RUNS_ROW_WORD } from "./runs-landing";

// #3040: one phrase for "a decision is waiting": "Needs you", with "sandbox held" only as the modifier for a
// parked sandbox. The Egress widget keeps its own "N held" count.
describe("a waiting decision has one phrase", () => {
  it("the chip, the row and the count line say it the same way", () => {
    expect(RUN_WAIT.waiting(1)).toBe("Needs you");
    expect(RUN_WAIT.waiting(3)).toBe("Needs you · 3");
    expect(RUN_WAIT.waitingHeld(1)).toBe("Needs you · sandbox held");
    expect(RUNS_ROW_WORD.NEEDS_APPROVAL).toBe("Needs you");
    expect(RUNS_ROW_WORD.NEEDS_DECISION).toBe(RUNS_ROW_WORD.NEEDS_APPROVAL);
    expect(RUN_COCKPIT.held(2)).toBe("2 held");
  });

  it("the retired phrases appear in no screen or copy file", () => {
    const root = join(__dirname, "..", "..", "..");
    const sources = (dir: string): string[] =>
      readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
        const p = join(dir, e.name);
        if (e.isDirectory()) return sources(p);
        return /\.tsx?$/.test(e.name) && !/\.test\.tsx?$/.test(e.name) ? [p] : [];
      });
    const retired = /Needs your approval|Sandbox is waiting|Awaiting confirmation|[0-9N] waiting · sandbox/;
    const offenders = sources(root).filter((p) => retired.test(readFileSync(p, "utf8"))).map((p) => relative(root, p));
    expect(offenders).toEqual([]);
  });
});
