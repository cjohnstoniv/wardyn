/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { DEMOS } from "./demo-catalog";

describe("demo catalog", () => {
  it("ships exactly four demos with distinct ids/titles", () => {
    expect(DEMOS).toHaveLength(4);
    expect(new Set(DEMOS.map((d) => d.id)).size).toBe(4);
    expect(new Set(DEMOS.map((d) => d.title)).size).toBe(4);
  });

  it("every policy is CC1, auto-stops, and grants/mounts/repos nothing", () => {
    for (const d of DEMOS) {
      expect(d.policy.min_confinement_class).toBe("CC1");
      expect(d.policy.auto_stop_after_sec ?? 0).toBeGreaterThan(0);
      // Workspace-free / grant-free by construction — these fields must be absent.
      expect(d.policy.eligible_grants).toBeUndefined();
      expect(d.policy.workspace_mounts).toBeUndefined();
      expect(d.policy.workspace_repos).toBeUndefined();
    }
  });

  it("every demo pins an empty allowed_domains (deny-all base)", () => {
    for (const d of DEMOS) {
      expect(d.policy.allowed_domains).toEqual([]);
    }
  });

  it("the four demos cover distinct first_use_approval / allow-all combos", () => {
    const combos = DEMOS.map((d) => `${d.policy.first_use_approval}:${d.policy.allow_all_egress ?? false}`);
    expect(new Set(combos).size).toBe(4);
    // The showcase quartet in order.
    expect(combos).toEqual([
      "always_deny:false",
      "deny_with_review:false",
      "wait_for_review:false",
      "always_deny:true",
    ]);
  });

  it("only the open-egress demo carries a caution", () => {
    const withCaution = DEMOS.filter((d) => d.caution);
    expect(withCaution).toHaveLength(1);
    expect(withCaution[0].policy.allow_all_egress).toBe(true);
    expect(withCaution[0].caution!.length).toBeGreaterThan(40);
  });

  it("every demo has at least one command step to paste", () => {
    for (const d of DEMOS) {
      expect(d.steps.some((s) => s.cmd)).toBe(true);
      for (const s of d.steps) expect(s.text.length).toBeGreaterThan(0);
    }
  });
});
