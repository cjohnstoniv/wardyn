/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { launchStart } from "./launch-start";
import type { RunIntent } from "./run-intent";

const NOW = Date.UTC(2026, 9, 20, 14, 0);
const schedule = { date: "2026-10-21", time: "02:00", zone: "UTC" };
const AUTONOMOUS: RunIntent = { shape: "agent_task", experience: "background" };

describe("launchStart", () => {
  it("is the schedule of an Autonomous agent run while the switch is on", () => {
    expect(launchStart({ intent: AUTONOMOUS, schedule }, true, NOW)).toEqual({ choice: schedule, problem: null });
    expect(launchStart({ intent: AUTONOMOUS, schedule: { ...schedule, date: "2026-10-19" } }, true, NOW)?.problem).toBe("past");
  });

  it("is nothing when the switch is off or absent, whatever the draft last held", () => {
    expect(launchStart({ intent: AUTONOMOUS, schedule }, false, NOW)).toBeUndefined();
  });

  it("is nothing for a run that is not an Autonomous agent run, or one with no schedule", () => {
    expect(launchStart({ intent: { shape: "agent_task", experience: "interactive" }, schedule }, true, NOW)).toBeUndefined();
    expect(launchStart({ intent: { shape: "application", experience: "background" }, schedule }, true, NOW)).toBeUndefined();
    expect(launchStart({ intent: AUTONOMOUS }, true, NOW)).toBeUndefined();
  });
});
