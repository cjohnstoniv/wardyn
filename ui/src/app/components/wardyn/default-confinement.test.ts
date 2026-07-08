/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect, vi, beforeEach } from "vitest";
import {
  strongestAvailable,
  resolveDefaultCc,
  getDefaultCc,
  setDefaultCc,
} from "./default-confinement";

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
  it("prefers the persisted pick when it's still available", () => {
    expect(resolveDefaultCc("CC1", ["CC1", "CC2", "CC3"])).toBe("CC1");
  });

  it("falls back to strongest available when the persisted pick isn't available", () => {
    expect(resolveDefaultCc("CC3", ["CC1", "CC2"])).toBe("CC2");
  });

  it("falls back to CC1 when nothing is available, regardless of persisted", () => {
    expect(resolveDefaultCc("CC3", [])).toBe("CC1");
  });

  it("falls back to strongest available when there's no persisted pick", () => {
    expect(resolveDefaultCc(null, ["CC1", "CC2"])).toBe("CC2");
  });
});

describe("getDefaultCc/setDefaultCc localStorage round-trip", () => {
  beforeEach(() => {
    localStorage.clear();
  });

  it("round-trips a persisted class", () => {
    expect(getDefaultCc()).toBeNull();
    setDefaultCc("CC2");
    expect(getDefaultCc()).toBe("CC2");
  });

  it("ignores a garbage stored value", () => {
    localStorage.setItem("wardyn-default-confinement", "nonsense");
    expect(getDefaultCc()).toBeNull();
  });

  it("tolerates a throwing localStorage on read (private-mode/quota)", () => {
    const spy = vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("denied");
    });
    expect(getDefaultCc()).toBeNull();
    spy.mockRestore();
  });

  it("tolerates a throwing localStorage on write (private-mode/quota)", () => {
    const spy = vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("denied");
    });
    expect(() => setDefaultCc("CC3")).not.toThrow();
    spy.mockRestore();
  });
});
