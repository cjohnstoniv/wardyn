/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { shortTime } from "../../../../lib/format";
import type { RunQueue } from "../../../../lib/types/runs";
import { givesUp, ordinal, placeInLine, waitingBody, waitingSince } from "./launched-model";

const NOW = Date.parse("2026-10-12T12:00:00Z");
const at = (ms: number) => new Date(NOW + ms).toISOString();
const MIN = 60_000;
const HOUR = 60 * MIN;
const queue = (position: number): RunQueue => ({ pool_id: "p", queued_at: at(0), deadline_at: at(HOUR), position, reason: "runner_pool_at_capacity" });

describe("the place in line", () => {
  it.each([[1, "1st"], [2, "2nd"], [3, "3rd"], [4, "4th"], [11, "11th"], [12, "12th"], [13, "13th"], [21, "21st"], [22, "22nd"], [101, "101st"]])(
    "ordinal %i is %s", (n, word) => expect(ordinal(n)).toBe(word),
  );

  it("counts the runs ahead, so the run itself is one more", () => {
    expect(placeInLine(queue(0))).toBe("1st in line");
    expect(placeInLine(queue(1))).toBe("2nd in line");
    expect(placeInLine(queue(3))).toBe("4th in line");
  });
});

describe("since and gives up", () => {
  it("says Just now inside the first minute, then the time the run joined the line", () => {
    expect(waitingSince(at(-20_000), NOW)).toBe("Just now");
    expect(waitingSince(at(-5 * MIN), NOW)).toBe(shortTime(at(-5 * MIN)));
    expect(waitingSince(at(-72 * HOUR), NOW)).toBe(shortTime(at(-72 * HOUR)));
  });

  it("reads the hours left from deadline_at in the canon hour words, then the time it gives up", () => {
    expect(givesUp(at(24 * HOUR), NOW)).toBe("In 24 hours");
    expect(givesUp(at(HOUR), NOW)).toBe("In 1 hour");
    expect(givesUp(at(20 * MIN), NOW)).toBe(shortTime(at(20 * MIN)));
    expect(givesUp(at(-5 * MIN), NOW)).toBe(shortTime(at(-5 * MIN)));
  });
});

describe("why it waits", () => {
  it("is the server's sentence for the reason, once the pool is known", () => {
    expect(waitingBody("runner_pool_at_capacity", { name: "CI jobs", max: 40 })).toBe("CI jobs is running its limit of 40. This run starts when there's room.");
    expect(waitingBody("runner_pool_no_runner_free", { name: "Laptops" })).toBe("None of your runners in Laptops is online. This run starts when one is.");
    expect(waitingBody("runner_pool_runners_at_capacity", { name: "Laptops" })).toBe("Every runner in Laptops is at its limit. This run starts when one has room.");
    expect(waitingBody("runner_pool_at_capacity", undefined)).toBeUndefined();
    expect(waitingBody("runner_pool_at_capacity", { name: "CI jobs" })).toBeUndefined();
  });
});
