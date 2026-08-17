/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { UNLISTED_RULES } from "../new-run/network-dialog";
import { DEMOS } from "./demo-catalog";

// Keyless, workspace-free, LLM-free sandboxes an operator drives by hand.
// DEMOS also carries a seventh, harness-aware demo (needsModel) — kept out of
// this subset since it trades the shared invariants below (empty
// allowed_domains, a pasted command) for a real, egress-scoped agent task.
const KEYLESS = DEMOS.filter((d) => !d.needsModel);

// The original showcase quartet — the first four keyless demos, each covering
// a distinct first_use_approval / allow-all combo. Two later keyless demos sit
// on different axes entirely and deliberately REUSE an earlier combo rather
// than inventing a new mode to distinguish, so both are excluded from the
// distinctness checks below and asserted on separately:
// - record-a-policy reuses the open-egress combo (Record Mode, not an
//   egress-approval-mode showcase — recording needs egress wide open, that's
//   the point).
// - once-or-for-good reuses fail-then-approve's deny_with_review (the
//   decision-SCOPE axis, not a new first_use_approval mode — scope is
//   orthogonal to FirstUseMode).
const SHOWCASE_QUARTET = KEYLESS.filter((d) => d.id !== "record-a-policy" && d.id !== "once-or-for-good");

describe("demo catalog", () => {
  it("ships exactly six keyless demos with distinct ids/titles", () => {
    expect(KEYLESS).toHaveLength(6);
    expect(new Set(KEYLESS.map((d) => d.id)).size).toBe(6);
    expect(new Set(KEYLESS.map((d) => d.title)).size).toBe(6);
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

  it("the original showcase quartet covers distinct first_use_approval / allow-all combos", () => {
    const combos = SHOWCASE_QUARTET.map((d) => `${d.policy.first_use_approval}:${d.policy.allow_all_egress ?? false}`);
    expect(new Set(combos).size).toBe(4);
    // The showcase quartet in order.
    expect(combos).toEqual([
      "always_deny:false",
      "deny_with_review:false",
      "wait_for_review:false",
      "always_deny:true",
    ]);
  });

  it("every wide-open-egress demo carries a caution", () => {
    const withCaution = KEYLESS.filter((d) => d.caution);
    const openEgress = KEYLESS.filter((d) => d.policy.allow_all_egress);
    // Every demo that opens egress wide carries the honest Fence-plus-open
    // caution — lines-that-cant-be-crossed AND record-a-policy both do.
    expect(withCaution.map((d) => d.id).sort()).toEqual(openEgress.map((d) => d.id).sort());
    expect(withCaution.length).toBeGreaterThan(0);
    for (const d of withCaution) expect(d.caution!.length).toBeGreaterThan(40);
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
    // Egress is scoped to Anthropic, not deny-all like the keyless six.
    expect(d.policy.allowed_domains.length).toBeGreaterThan(0);
    expect(d.policy.allowed_domains.every((h) => h.includes("anthropic.com"))).toBe(true);
  });

  // Regression: setupUi copy pointed users at a nonexistent "Access → Egress"
  // subsection (Egress is its own sibling wizard step, not nested under Access).
  it("setupUi never claims Egress lives under the Access step", () => {
    for (const d of DEMOS) {
      for (const line of d.setupUi) {
        expect(line).not.toMatch(/Access\s*→\s*Egress/);
      }
    }
  });

  // Regression: setupUi quoted "Deny with review", but the real dropdown
  // option text (step-egress.tsx) is "Deny + review".
  // setupUi tells an operator which control to click, so it must quote the
  // words the UI actually shows. This used to hardcode the wizard's Select
  // label ("Deny + review"); the New run page names each rule by its
  // CONSEQUENCE instead, so the assertion now reads UNLISTED_RULES — the one
  // place those titles live — and a future rewording updates both at once.
  it("setupUi quotes the real unlisted-host rule titles verbatim", () => {
    for (const mode of ["deny_with_review", "wait_for_review"] as const) {
      const demo = DEMOS.find((d) => d.policy.first_use_approval === mode);
      if (!demo) continue;
      const title = UNLISTED_RULES.find((r) => r.id === mode)!.title;
      expect(
        demo.setupUi.some((line) => line.includes(`'${title}'`)),
        `${demo.id} should quote "${title}"`,
      ).toBe(true);
    }
  });

  // once-or-for-good is a SECOND deny_with_review demo (fail-then-approve is
  // the first), so the DEMOS.find(...) above — first match wins — never
  // reaches its setupUi. Address this one by id rather than assuming the
  // generic check above covers it.
  it("once-or-for-good's setupUi also quotes the deny_with_review rule title verbatim", () => {
    const demo = DEMOS.find((d) => d.id === "once-or-for-good")!;
    const title = UNLISTED_RULES.find((r) => r.id === "deny_with_review")!.title;
    expect(
      demo.setupUi.some((line) => line.includes(`'${title}'`)),
      `${demo.id} should quote "${title}"`,
    ).toBe(true);
  });

  // The steps must not send anyone to a screen that no longer exists.
  it("no setupUi line names the retired wizard's steps", () => {
    for (const d of DEMOS) {
      for (const line of d.setupUi) {
        expect(line).not.toMatch(/Egress step|Basics step|Confinement step|Review step|wizard/i);
      }
    }
  });
});
