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

// #3040: one phrase for "a decision is waiting". The noun is "decision", the verb is "needs", and
// "sandbox held" is only ever the modifier for a parked sandbox.
describe("a waiting decision has one phrase", () => {
  it("the chip, the row and the count line say it the same way", () => {
    expect(RUN_WAIT.waiting(1)).toBe("Needs a decision");
    expect(RUN_WAIT.waiting(3)).toBe("Needs 3 decisions");
    expect(RUN_WAIT.waitingHeld(1)).toBe("Needs a decision · sandbox held");
    expect(RUNS_ROW_WORD.NEEDS_APPROVAL).toBe("Needs a decision");
    expect(RUN_COCKPIT.held(1)).toBe("1 needs a decision");
    expect(RUN_COCKPIT.held(2)).toBe("2 need a decision");
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
