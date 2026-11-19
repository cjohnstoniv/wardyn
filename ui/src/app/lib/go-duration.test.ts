/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { goDuration, goDurationMs } from "./go-duration";

describe("go durations", () => {
  it("reads the canonical forms", () => {
    expect(goDurationMs("4h")).toBe(4 * 3_600_000);
    expect(goDurationMs("1h30m")).toBe(90 * 60_000);
    expect(goDurationMs("45m")).toBe(45 * 60_000);
    expect(goDurationMs("15m0s")).toBe(15 * 60_000);
  });
  it("refuses anything else", () => {
    expect(goDurationMs("")).toBeNaN();
    expect(goDurationMs("4 h")).toBeNaN();
    expect(goDurationMs("2d")).toBeNaN();
  });
  it("writes what CanonicalDuration writes", () => {
    expect(goDuration(4 * 3_600_000)).toBe("4h");
    expect(goDuration(90 * 60_000)).toBe("1h30m");
    expect(goDuration(45 * 60_000)).toBe("45m");
  });
});
