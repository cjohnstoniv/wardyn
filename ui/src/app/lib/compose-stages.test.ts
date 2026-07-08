/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";

import { hostLabel, stageLabel } from "./compose-stages";

describe("compose-stages", () => {
  it("hostLabel always includes the raw host (never drops it)", () => {
    expect(hostLabel("x.example.com")).toBe("x.example.com");
    const gh = hostLabel("api.github.com");
    expect(gh).toContain("api.github.com");
    expect(gh).toContain("GitHub");
  });

  it("stageLabel maps known keys and falls back to the raw key", () => {
    expect(stageLabel("clamp")).toBe("Applying your security policy");
    expect(stageLabel("brand_new_stage")).toBe("brand_new_stage");
    expect(stageLabel(undefined)).toBeTruthy();
  });
});
