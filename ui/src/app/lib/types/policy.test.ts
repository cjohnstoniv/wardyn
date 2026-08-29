/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import {
  asFirstUseMode,
  firstUseRaisesApproval,
  firstUseLabel,
  toolRulesProblem,
  MAX_TOOL_RULES,
  MAX_TOOL_RULE_NAME_LEN,
  TOOL_RULE_DEFAULT,
  type ToolEffect,
} from "./policy";

// The first-use egress-approval normalizer is the fail-closed trust boundary
// between whatever the wire/policy JSON carries and the three modes the UI acts
// on. It must accept the legacy boolean form, the enum form, AND coerce anything
// unrecognized to the SAFE default (always_deny) — never silently escalate an
// unlisted domain to "allowed" or drop it into a review mode it did not ask for.
// These functions had zero coverage; a regression that made the default
// permissive would be invisible.
describe("asFirstUseMode — fail-closed normalization", () => {
  it("maps the legacy boolean form", () => {
    expect(asFirstUseMode(true)).toBe("deny_with_review");
    expect(asFirstUseMode(false)).toBe("always_deny");
  });

  it("passes the three canonical enum values through unchanged", () => {
    expect(asFirstUseMode("always_deny")).toBe("always_deny");
    expect(asFirstUseMode("deny_with_review")).toBe("deny_with_review");
    expect(asFirstUseMode("wait_for_review")).toBe("wait_for_review");
  });

  it("treats absent/empty as the safe default (always_deny)", () => {
    expect(asFirstUseMode(null)).toBe("always_deny");
    expect(asFirstUseMode(undefined)).toBe("always_deny");
    expect(asFirstUseMode("")).toBe("always_deny");
  });

  it.each([
    ["unknown string", "allow_everything"],
    ["near-miss casing", "Deny_With_Review"],
    ["number", 1],
    ["object", { mode: "wait_for_review" }],
    ["array", ["deny_with_review"]],
    ["truthy-looking string", "true"],
  ])("fails closed on an unrecognized %s", (_label, input) => {
    // The whole point: an attacker-influenced or corrupted value must land on
    // the hard-deny mode, never a permissive or review mode.
    expect(asFirstUseMode(input)).toBe("always_deny");
    expect(firstUseRaisesApproval(input)).toBe(false);
  });
});

describe("firstUseRaisesApproval", () => {
  it("is true only for the two review modes", () => {
    expect(firstUseRaisesApproval("deny_with_review")).toBe(true);
    expect(firstUseRaisesApproval("wait_for_review")).toBe(true);
    expect(firstUseRaisesApproval(true)).toBe(true);
  });
  it("is false for hard-deny and unknown input", () => {
    expect(firstUseRaisesApproval("always_deny")).toBe(false);
    expect(firstUseRaisesApproval(false)).toBe(false);
    expect(firstUseRaisesApproval(null)).toBe(false);
    expect(firstUseRaisesApproval("garbage")).toBe(false);
  });
});

// N4: always_deny used to read "Off" unconditionally — the most restrictive
// choice reading as though nothing were set. "Off" is only honest under
// allow-all egress, where the setting is genuinely inert (buildSpec forces
// always_deny and the Egress step hides the control) — every other caller
// must see the real word for the real choice.
describe("firstUseLabel", () => {
  it("labels each review mode", () => {
    expect(firstUseLabel("wait_for_review")).toBe("Ask & wait");
    expect(firstUseLabel("deny_with_review")).toBe("Ask");
  });

  it("labels always_deny/unknown as 'Always deny' by default — a real, restrictive choice, not an inversion of it", () => {
    expect(firstUseLabel("always_deny")).toBe("Always deny");
    expect(firstUseLabel(false)).toBe("Always deny");
    expect(firstUseLabel("garbage")).toBe("Always deny");
  });

  it("labels it 'Off (allow-all)' only when the caller says this run's egress is allow-all", () => {
    expect(firstUseLabel("always_deny", true)).toBe("Off (allow-all)");
    expect(firstUseLabel(undefined, true)).toBe("Off (allow-all)");
    // The review modes are unaffected by allowAll — they're never forced under
    // allow-all in the first place (buildSpec always forces always_deny there).
    expect(firstUseLabel("deny_with_review", true)).toBe("Ask");
  });
});

// tool_rules is a SECURITY field with a closed enum and a duplicate refusal.
// The mirror exists so the editor refuses what the API refuses instead of
// round-tripping a 400 — if it drifts from internal/api/policy.go's
// validateToolRules, the console starts promising writes the server rejects.
describe("toolRulesProblem — mirrors validateToolRules", () => {
  it("accepts an empty list — that IS today's behaviour, not a missing value", () => {
    expect(toolRulesProblem([])).toBeNull();
  });

  it("accepts the documented example, default rule included", () => {
    expect(
      toolRulesProblem([
        { tool: "Read", effect: "allow" },
        { tool: "Bash", effect: "hold" },
        { tool: "WebFetch", effect: "deny" },
        { tool: TOOL_RULE_DEFAULT, effect: "hold" },
      ]),
    ).toBeNull();
  });

  it("refuses a duplicate tool — two rules for one tool means one does nothing", () => {
    expect(
      toolRulesProblem([
        { tool: "Bash", effect: "allow" },
        { tool: "Bash", effect: "deny" },
      ]),
    ).toMatch(/Two rules name/);
  });

  it("refuses an empty tool name and points at the default", () => {
    expect(toolRulesProblem([{ tool: "", effect: "hold" }])).toMatch(/use \* for the default/);
  });

  it("refuses surrounding whitespace — the match is exact, so it would never fire", () => {
    expect(toolRulesProblem([{ tool: "Bash ", effect: "hold" }])).toMatch(/never fire/);
  });

  it("refuses an effect outside the enum", () => {
    expect(
      toolRulesProblem([{ tool: "Bash", effect: "ask" as unknown as ToolEffect }]),
    ).toMatch(/not an effect/);
  });

  it("refuses more rules than the server accepts, and names the cap", () => {
    const many = Array.from({ length: MAX_TOOL_RULES + 1 }, (_, i) => ({
      tool: `T${i}`,
      effect: "hold" as const,
    }));
    expect(toolRulesProblem(many)).toContain(String(MAX_TOOL_RULES));
    expect(toolRulesProblem(many.slice(0, MAX_TOOL_RULES))).toBeNull();
  });

  it("refuses a tool name past the length cap", () => {
    expect(toolRulesProblem([{ tool: "x".repeat(MAX_TOOL_RULE_NAME_LEN + 1), effect: "hold" }])).toMatch(
      /exceeds/,
    );
    expect(toolRulesProblem([{ tool: "x".repeat(MAX_TOOL_RULE_NAME_LEN), effect: "hold" }])).toBeNull();
  });
});
