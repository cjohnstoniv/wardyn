/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { strongestAvailable, resolveDefaultCc } from "./default-confinement";

describe("strongestAvailable", () => {
  it("picks the last CC_ORDER member present", () => {
    expect(strongestAvailable(["CC1", "CC2", "CC3"])).toBe("CC3");
    expect(strongestAvailable(["CC1", "CC2"])).toBe("CC2");
    expect(strongestAvailable(["CC1"])).toBe("CC1");
  });

  it("returns undefined when nothing is available", () => {
    expect(strongestAvailable([])).toBeUndefined();
  });
});

describe("resolveDefaultCc", () => {
  it("prefers the in-session pick when it's still available", () => {
    expect(resolveDefaultCc("CC1", ["CC1", "CC2", "CC3"])).toBe("CC1");
  });

  it("falls back to strongest available when the pick isn't available", () => {
    expect(resolveDefaultCc("CC3", ["CC1", "CC2"])).toBe("CC2");
  });

  it("falls back to CC1 when nothing is available, regardless of the pick", () => {
    expect(resolveDefaultCc("CC3", [])).toBe("CC1");
  });

  it("falls back to strongest available when there's no pick", () => {
    expect(resolveDefaultCc(null, ["CC1", "CC2"])).toBe("CC2");
  });
});
