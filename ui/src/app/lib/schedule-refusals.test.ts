/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// Go/TS byte parity for the scheduled-run refusals. Run:
//   cd ui && pnpm vitest run src/app/lib/schedule-refusals.test.ts

import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { HOME_OF_REASON } from "../components/screens/new-run-flow/draft/field-homes";
import { isScheduleReason, SCHEDULE_REASONS, SCHEDULE_REFUSAL, SCHEDULE_REFUSAL_REASON } from "./schedule-refusals";

function repoRoot(): string {
  let dir = resolve(process.cwd());
  for (let i = 0; i < 8; i++) {
    if (existsSync(join(dir, "go.mod"))) return dir;
    dir = dirname(dir);
  }
  throw new Error("go.mod not found");
}

const root = repoRoot();
const goSource = (file: string) => readFileSync(join(root, file), "utf8");

describe("scheduled-run refusals", () => {
  it("the reasons are the Go constants of internal/api/reasons.go", () => {
    const go = goSource("internal/api/reasons.go");
    const goReasons = [...go.matchAll(/reason(?:ScheduledRuns\w+|StartAt\w+)\s+=\s*"([a-z_]+)"/g)].map((m) => m[1]);
    expect(new Set(SCHEDULE_REASONS)).toEqual(new Set(goReasons));
  });

  it("each sentence is the Go message, byte for byte", () => {
    const go = goSource("internal/api/run_schedule.go");
    const msg = (name: string) => new RegExp(`${name}\\s+=\\s+"((?:[^"\\\\]|\\\\.)*)"`).exec(go)?.[1] ?? "";
    expect(SCHEDULE_REFUSAL.OFF()).toBe(msg("scheduledRunsOffMsg"));
    expect(SCHEDULE_REFUSAL.UNAVAILABLE()).toBe(msg("scheduledRunsUnavailableMsg"));
    expect(SCHEDULE_REFUSAL.INVALID()).toBe(msg("startAtInvalidMsg"));
    expect(SCHEDULE_REFUSAL.PAST("@1")).toBe(msg("startAtPastMsg").replace("%s", "@1"));
    expect(SCHEDULE_REFUSAL.TOO_FAR("@1", "@2", 24)).toBe(msg("startAtTooFarMsg").replace("%s", "@1").replace("%s", "@2").replace("%d", "24"));
  });

  it("each sentence belongs to a reason the server sends, and each reason has a sentence", () => {
    expect(Object.values(SCHEDULE_REFUSAL_REASON).sort()).toEqual([...SCHEDULE_REASONS].sort());
    expect(Object.keys(SCHEDULE_REFUSAL_REASON).sort()).toEqual(Object.keys(SCHEDULE_REFUSAL).sort());
    expect(SCHEDULE_REASONS.every(isScheduleReason)).toBe(true);
    expect(isScheduleReason("queue_full")).toBe(false);
  });

  it("every reason has a home on the Run step", () => {
    for (const reason of SCHEDULE_REASONS) expect(HOME_OF_REASON[reason], reason).toBe("run");
  });
});
