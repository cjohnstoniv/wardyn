/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import { timeLeft } from "./time-left";

describe("time left", () => {
  it("spells hours and minutes", () => {
    expect(timeLeft((3 * 60 + 42) * 60_000)).toBe("3 h 42 min");
  });
  it("leaves out zero minutes", () => {
    expect(timeLeft(4 * 3_600_000)).toBe("4 h");
  });
  it("spells minutes alone", () => {
    expect(timeLeft(25 * 60_000)).toBe("25 min");
  });
  it("never goes below zero", () => {
    expect(timeLeft(-5 * 60_000)).toBe("0 min");
  });
});
