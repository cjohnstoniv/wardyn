/**
 * Copyright 2025 The Wardyn Authors
 * SPDX-License-Identifier: Apache-2.0
 */

// The client-side matcher must answer exactly what internal/api's
// capValueMatches answers — a UI that says "granted" where the server says
// "refused" is worse than no annotation at all.
import { describe, it, expect } from "vitest";
import {
  anyCapabilityEnforced,
  capabilityAllowed,
  capabilityAnswer,
  capabilityValueMatches,
  capabilityValueOverlaps,
} from "./capabilities";
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

// ─── the shared matcher table ────────────────────────────────────────────────
// One list of (kind, grantValue, want, expected) rows per matcher, so the next
// change to internal/api/capabilities.go has a row to add on BOTH sides rather
// than a silent divergence. capValueMatches / capValueOverlaps are the Go twins
// (internal/api/capabilities.go); the Go-side consumer of this same table is
// filed as a follow-up (F014).
export const CAP_MATCH_CASES: Array<[string, string, string, boolean]> = [
  ["egress_host", "*", "anything.example", true],
  ["secret", "*", "STRIPE_LIVE_KEY", true],
  ["egress_host", "github.com", "github.com:443", true],
  ["egress_host", "GitHub.com.", "github.com", true],
  ["egress_host", "*.github.com", "api.github.com", true],
  ["egress_host", "*.github.com", "github.com", false],
  ["egress_host", "*.github.com", "evilgithub.com", false],
  // The asymmetry capValueOverlaps exists for: a wildcard WANT is not covered
  // by the narrower denied host.
  ["egress_host", "secret.example.com", "*.example.com", false],
  ["secret", "GITHUB_TOKEN", "github_token", false],
  ["image", "ghcr.io/a/b:1", "ghcr.io/a/b:2", false],
];

export const CAP_OVERLAP_CASES: Array<[string, string, string, boolean]> = [
  // Everything capValueMatches answers true, overlaps answers true.
  ["egress_host", "*.github.com", "api.github.com", true],
  // ...and the reverse direction, which is the whole point of the deny matcher.
  ["egress_host", "secret.example.com", "*.example.com", true],
  ["egress_host", "*.example.com", "secret.example.com", true],
  // Still no match when the sets are disjoint.
  ["egress_host", "secret.example.com", "*.other.com", false],
  // Only egress hosts are set-valued: every other kind stays an exact compare
  // in BOTH directions, so the reversal must not soften a near-miss.
  ["secret", "GITHUB_TOKEN", "github_token", false],
  ["workspace", "*", "1234", true],
];

describe("the Go/TS matcher table", () => {
  it.each(CAP_MATCH_CASES)("capabilityValueMatches(%s, %s, %s) === %s", (kind, grant, want, expected) => {
    expect(capabilityValueMatches(kind, grant, want)).toBe(expected);
  });

  it.each(CAP_OVERLAP_CASES)("capabilityValueOverlaps(%s, %s, %s) === %s", (kind, grant, want, expected) => {
    expect(capabilityValueOverlaps(kind, grant, want)).toBe(expected);
  });
});

describe("capabilityAllowed — the DENY arm asks capValueOverlaps, not capValueMatches", () => {
  it("a deny on one host bites a wildcard want that contains it (capScan's deny arm)", () => {
    const c = caps({
      grants: [
        g({ value: "*.example.com" }),
        g({ id: "d", value: "secret.example.com", effect: "deny" }),
      ],
      enforcement: { egress_host: true },
    });
    expect(capabilityAllowed(c, "egress_host", "*.example.com")).toBe(false);
    expect(capabilityAnswer(c, "egress_host", "*.example.com")).toBe("denied");
  });

  it("a deny still does NOT bite a disjoint want", () => {
    const c = caps({ grants: [g({ id: "d", value: "secret.example.com", effect: "deny" })] });
    expect(capabilityAllowed(c, "egress_host", "*.other.com")).toBe(true);
  });

  it("the reversal is egress-only: a near-miss secret name is not a deny", () => {
    const c = caps({ grants: [g({ id: "d", capability: "secret", value: "TOKEN", effect: "deny" })] });
    expect(capabilityAllowed(c, "secret", "TOKEN_2")).toBe(true);
  });
});

describe("capabilityAnswer — the stale-group-snapshot arm", () => {
  it("an unanswerable group snapshot makes an otherwise-allowed value UNKNOWN", () => {
    // capScan's stale arm sits above the enforcement switch, and the group deny
    // rows it consults are withheld from GET /me/capabilities — so a clean
    // local scan is not evidence of permission.
    const c = caps({ grants: [g({ value: "*" })], groups_snapshot_stale: true });
    expect(capabilityAnswer(c, "egress_host", "github.com")).toBe("unknown");
    expect(capabilityAnswer(c, "egress_host", "github.com")).not.toBe("allowed");
  });

  it("unknown stays fail-open for capabilityAllowed — an advisory check never disables a honoured control", () => {
    const c = caps({ grants: [g({ value: "*" })], groups_snapshot_stale: true });
    expect(capabilityAllowed(c, "egress_host", "github.com")).toBe(true);
  });

  it("a stale snapshot never UN-refuses a local refusal", () => {
    const enforced = caps({ enforcement: { egress_host: true }, groups_snapshot_stale: true });
    expect(capabilityAnswer(enforced, "egress_host", "github.com")).toBe("denied");
    expect(capabilityAllowed(enforced, "egress_host", "github.com")).toBe(false);
    const denied = caps({
      grants: [g({ id: "d", value: "github.com", effect: "deny" })],
      groups_snapshot_stale: true,
    });
    expect(capabilityAnswer(denied, "egress_host", "github.com")).toBe("denied");
  });

  it("a fresh snapshot answers allowed, not unknown", () => {
    expect(capabilityAnswer(caps({ grants: [g({ value: "*" })] }), "egress_host", "github.com")).toBe("allowed");
  });
});
