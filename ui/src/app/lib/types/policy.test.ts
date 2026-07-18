/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

import { describe, it, expect } from "vitest";
import { asFirstUseMode, firstUseRaisesApproval, firstUseLabel } from "./policy";

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

describe("firstUseLabel", () => {
  it("labels each mode, defaulting unknown/deny to Off", () => {
    expect(firstUseLabel("wait_for_review")).toBe("Ask & wait");
    expect(firstUseLabel("deny_with_review")).toBe("Ask");
    expect(firstUseLabel("always_deny")).toBe("Off");
    expect(firstUseLabel(false)).toBe("Off");
    expect(firstUseLabel("garbage")).toBe("Off");
  });
});
