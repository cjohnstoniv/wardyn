/**
 * Copyright 2026 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, expect, it } from "vitest";
import golden from "./tool-effect.golden.json";
import { effectiveToolEffect, type ToolEffect } from "./tool-effect";

describe("effectiveToolEffect", () => {
  it("answers every golden case the server's policyToolEffect answers", () => {
    const cases = golden as { name: string; policy_rules: { tool: string; effect: string }[]; lane_default: ToolEffect; tool: string; effect: string }[];
    expect(cases.length).toBeGreaterThan(5);
    for (const c of cases) {
      expect(effectiveToolEffect(c.policy_rules, c.lane_default, c.tool), c.name).toBe(c.effect);
    }
  });
});
