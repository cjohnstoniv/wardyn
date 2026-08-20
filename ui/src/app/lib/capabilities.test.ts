/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The client-side matcher must answer exactly what internal/api's
// capValueMatches answers — a UI that says "granted" where the server says
// "refused" is worse than no annotation at all.
import { describe, it, expect } from "vitest";
import { anyCapabilityEnforced, capabilityAllowed, capabilityValueMatches } from "./capabilities";
import type { CapabilityGrant, MeCapabilities } from "./types";

function g(over: Partial<CapabilityGrant>): CapabilityGrant {
  return {
    id: "g",
    subject_type: "user",
    subject: "alice@corp.example",
    capability: "egress_host",
    value: "*",
    effect: "allow",
    created_at: "2026-08-01T00:00:00Z",
    ...over,
  };
}

function caps(over: Partial<MeCapabilities> = {}): MeCapabilities {
  return { grants: [], enforcement: {}, session_groups: [], groups_snapshot_stale: false, ...over };
}

describe("capabilityValueMatches", () => {
  it("'*' covers every value of every kind", () => {
    expect(capabilityValueMatches("egress_host", "*", "anything.example")).toBe(true);
    expect(capabilityValueMatches("secret", "*", "STRIPE_LIVE_KEY")).toBe(true);
    expect(capabilityValueMatches("workspace", "*", "1234")).toBe(true);
  });

  it("egress_host ignores a port and a trailing dot on either side", () => {
    expect(capabilityValueMatches("egress_host", "github.com", "github.com:443")).toBe(true);
    expect(capabilityValueMatches("egress_host", "GitHub.com.", "github.com")).toBe(true);
  });

  it("a '*.suffix' host covers subdomains but NOT the bare domain", () => {
    expect(capabilityValueMatches("egress_host", "*.github.com", "api.github.com")).toBe(true);
    expect(capabilityValueMatches("egress_host", "*.github.com", "github.com")).toBe(false);
    expect(capabilityValueMatches("egress_host", "*.github.com", "evilgithub.com")).toBe(false);
  });

  it("every other kind is an exact compare — a near-miss must not match", () => {
    expect(capabilityValueMatches("secret", "GITHUB_TOKEN", "GITHUB_TOKEN")).toBe(true);
    expect(capabilityValueMatches("secret", "GITHUB_TOKEN", "github_token")).toBe(false);
    expect(capabilityValueMatches("image", "ghcr.io/a/b:1", "ghcr.io/a/b:2")).toBe(false);
  });
});

describe("capabilityAllowed — deny beats allow beats the switch", () => {
  it("a null set is allowed: an admin, or an answer that hasn't loaded", () => {
    expect(capabilityAllowed(null, "egress_host", "github.com")).toBe(true);
  });

  it("an unenforced kind with no grants is allowed — 0.5's behaviour, byte for byte", () => {
    expect(capabilityAllowed(caps(), "egress_host", "github.com")).toBe(true);
  });

  it("an enforced kind with no matching grant is refused", () => {
    expect(capabilityAllowed(caps({ enforcement: { egress_host: true } }), "egress_host", "github.com")).toBe(false);
  });

  it("a matching allow wins over an enforced switch", () => {
    const c = caps({ grants: [g({ value: "*.github.com" })], enforcement: { egress_host: true } });
    expect(capabilityAllowed(c, "egress_host", "api.github.com")).toBe(true);
  });

  it("a deny bites even while the kind is NOT enforced, and even beside an allow", () => {
    const c = caps({
      grants: [g({ value: "github.com" }), g({ id: "d", value: "github.com", effect: "deny" })],
    });
    expect(capabilityAllowed(c, "egress_host", "github.com")).toBe(false);
  });

  it("a grant for another kind never answers this one", () => {
    const c = caps({ grants: [g({ capability: "secret", value: "*" })], enforcement: { egress_host: true } });
    expect(capabilityAllowed(c, "egress_host", "github.com")).toBe(false);
  });
});

describe("anyCapabilityEnforced", () => {
  it("is false for a null set and for an all-off map", () => {
    expect(anyCapabilityEnforced(null)).toBe(false);
    expect(anyCapabilityEnforced(caps({ enforcement: { egress_host: false } }))).toBe(false);
  });
  it("is true as soon as one kind is on", () => {
    expect(anyCapabilityEnforced(caps({ enforcement: { workspace: true } }))).toBe(true);
  });
});
