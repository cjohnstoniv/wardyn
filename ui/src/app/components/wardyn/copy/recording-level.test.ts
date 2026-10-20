/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { gapDuration, RECORDING_LEVEL, RECORDING_REPLAY } from "./recording-level";

describe("recording-level copy", () => {
  it("spells the boards' duration and gap sentences", () => {
    expect(gapDuration(134_000)).toBe("2 min 14 s");
    expect(RECORDING_REPLAY.PAUSED_FOR(gapDuration(134_000))).toBe("Recording paused for 2 min 14 s");
    expect(RECORDING_REPLAY.PAUSED_BODY("you took over to sign in")).toBe(
      "You took over to sign in, so no screen frames were kept. The action log kept going. The gap is marked in the audit log.",
    );
    expect(RECORDING_LEVEL.MINIMUM_ABOVE("Engineering", "Terminal", ["Off"])).toBe(
      "Engineering requires at least Terminal. Off isn't available here.",
    );
  });

  it("offers exactly the three levels, none of them keystrokes", () => {
    expect(RECORDING_LEVEL.LEVELS.map((l) => l.value)).toEqual(["off", "terminal", "screen_actions"]);
  });
});
