/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { DEMOS } from "./demo-catalog";

// The showcase quartet: keyless, workspace-free, LLM-free sandboxes an operator
// drives by hand. DEMOS also carries a fifth, harness-aware demo (needsModel) —
// kept out of this subset since it trades the shared invariants below (empty
// allowed_domains, a pasted command) for a real, egress-scoped agent task.
const KEYLESS = DEMOS.filter((d) => !d.needsModel);

describe("demo catalog", () => {
  it("ships exactly four keyless demos with distinct ids/titles", () => {
    expect(KEYLESS).toHaveLength(4);
    expect(new Set(KEYLESS.map((d) => d.id)).size).toBe(4);
    expect(new Set(KEYLESS.map((d) => d.title)).size).toBe(4);
  });

  it("every demo (including the harness one) is CC1, auto-stops, and grants/mounts/repos nothing", () => {
    for (const d of DEMOS) {
      expect(d.policy.min_confinement_class).toBe("CC1");
      expect(d.policy.auto_stop_after_sec ?? 0).toBeGreaterThan(0);
      // Workspace-free / grant-free by construction — these fields must be absent.
      expect(d.policy.eligible_grants).toBeUndefined();
      expect(d.policy.workspace_mounts).toBeUndefined();
      expect(d.policy.workspace_repos).toBeUndefined();
    }
  });

  it("every keyless demo pins an empty allowed_domains (deny-all base) and needs no model", () => {
    for (const d of KEYLESS) {
      expect(d.policy.allowed_domains).toEqual([]);
      expect(d.needsModel).toBeFalsy();
    }
  });

  it("the four keyless demos cover distinct first_use_approval / allow-all combos", () => {
    const combos = KEYLESS.map((d) => `${d.policy.first_use_approval}:${d.policy.allow_all_egress ?? false}`);
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
    const withCaution = KEYLESS.filter((d) => d.caution);
    expect(withCaution).toHaveLength(1);
    expect(withCaution[0].policy.allow_all_egress).toBe(true);
    expect(withCaution[0].caution!.length).toBeGreaterThan(40);
  });

  it("every keyless demo has at least one command step to paste", () => {
    for (const d of KEYLESS) {
      expect(d.steps.some((s) => s.cmd)).toBe(true);
      for (const s of d.steps) expect(s.text.length).toBeGreaterThan(0);
    }
  });

  it("ships exactly one harness demo — needs a model, drives the agent via a terminal command", () => {
    const harness = DEMOS.filter((d) => d.needsModel);
    expect(harness).toHaveLength(1);
    const [d] = harness;
    // Interactive like the rest (the operator runs `claude` in the attached
    // terminal) — a command step to paste, not an autonomous task.
    expect(d.steps.some((s) => s.cmd?.includes("claude"))).toBe(true);
    // Egress is scoped to Anthropic, not deny-all like the keyless four.
    expect(d.policy.allowed_domains.length).toBeGreaterThan(0);
    expect(d.policy.allowed_domains.every((h) => h.includes("anthropic.com"))).toBe(true);
  });
});
